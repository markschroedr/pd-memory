import { readFileSync, realpathSync, statSync } from "node:fs";
import { basename, isAbsolute, relative, resolve, sep } from "node:path";
import { parseArgs } from "node:util";
import { z } from "zod";
import { PdMemoryRuntime, drain } from "../src/index";
import { loadConfig } from "../src/config";
import { prepareDialogue } from "../src/dialogue";
import { completedMessages, parseSession, type Provider } from "./session-formats";

const help = `Import completed conversations from coding-agent JSONL logs.

bun scripts/import-sessions.ts --config FILE [--pi PATH] [--claude-code PATH] [--codex PATH]

Each input flag accepts a file or a directory and can be repeated. Directories are
searched recursively. Only explicitly supplied paths are read; subagent logs are skipped.

  --config FILE             Memory configuration (or PD_MEMORY_CONFIG)
  --routes FILE             Optional JSON: {"projects":[{"root":"/project","home":"page-slug"}],"capture_after":"ISO time"}
  --after ISO               Only messages at or after this time
  --user-name NAME          Speaker label; defaults to the first configured user name, then User
  --dry-run                 Parse and count complete messages; no database or API calls
  --wait                    Process queued sources (makes paid model/embedding calls)
  --extract-concurrency N   Parallel extraction workers; default 4

Without --wait, sources are queued for the memory worker. Repeated runs import only
new message IDs. Run this command daily with your scheduler. Keep credentials outside Git.
`;
const routeSchema = z.object({ capture_after: z.string().optional(), projects: z.array(z.object({ root: z.string().min(1), home: z.string().min(1) })).default([]) });

export async function main(args = process.argv.slice(2)): Promise<void> {
  const { values } = parseArgs({ args, options: {
    config: { type: "string" }, routes: { type: "string" }, after: { type: "string" },
    pi: { type: "string", multiple: true }, "claude-code": { type: "string", multiple: true }, codex: { type: "string", multiple: true },
    "user-name": { type: "string" }, "dry-run": { type: "boolean" }, wait: { type: "boolean" },
    "extract-concurrency": { type: "string", default: "4" }, help: { type: "boolean" },
  }, strict: true, allowPositionals: false });
  if (values.help) { console.log(help); return; }
  const configPath = values.config ?? process.env.PD_MEMORY_CONFIG;
  if (!configPath) throw new Error("Supply --config or PD_MEMORY_CONFIG");
  const concurrency = Number(values["extract-concurrency"]);
  if (!Number.isSafeInteger(concurrency) || concurrency < 1) throw new Error("--extract-concurrency must be a positive integer");
  if (values.wait && values["dry-run"]) throw new Error("Choose --dry-run or --wait, not both");
  const routes = routeSchema.parse(values.routes ? JSON.parse(readFileSync(values.routes, "utf8")) : {});
  const after = values.after ?? routes.capture_after;
  const captureAfter = after === undefined ? null : new Date(after).toISOString();
  const files = new Map<string, Provider>();
  let inputs = 0;
  for (const provider of ["pi", "claude-code", "codex"] as const) {
    for (const input of values[provider] ?? []) {
      inputs++;
      const root = realpathSync(input);
      const paths = statSync(root).isFile() ? [root] : [...new Bun.Glob("**/*.jsonl").scanSync({ cwd: root, absolute: true, onlyFiles: true })];
      for (const candidate of paths) {
        const path = realpathSync(candidate);
        if (!isMainSession(path, provider)) continue;
        if (files.has(path) && files.get(path) !== provider) throw new Error(`Same file assigned to two formats: ${path}`);
        files.set(path, provider);
      }
    }
  }
  if (!inputs) throw new Error("Supply at least one --pi, --claude-code, or --codex path");
  const config = await loadConfig(configPath);
  const user = values["user-name"] ?? config.identity.userNames[0] ?? "User";
  const memory = values["dry-run"] ? null : await PdMemoryRuntime.open(configPath);
  const stats = { files: files.size, submitted: 0, reused: 0, skipped: 0, messages: 0, characters: 0, userCharacters: 0, assistantCharacters: 0 };
  const jobs = new Set<number>();
  let processed = 0;
  try {
    for (const [path, provider] of [...files].sort(([a], [b]) => a.localeCompare(b))) {
      const session = parseSession(path, provider);
      if (!session) { stats.skipped++; continue; }
      if (memory) {
        for (const job of memory.store.db.query<{ id: number }, [string]>(
          "SELECT j.id FROM jobs j JOIN sources s ON s.id=j.source_id WHERE s.session=?").all(session.id)) jobs.add(job.id);
      }
      const captured = memory?.capturedSessionEntryIds(session.id) ?? new Set<string>();
      const pending = completedMessages(session.messages).filter(message => !captured.has(message.entryId)
        && (captureAfter === null || message.timestamp >= captureAfter));
      if (!pending.some(message => message.role === "user")) { stats.skipped++; continue; }
      const dialogueMessages = pending.map(message => ({ id: message.entryId, role: message.role,
        timestamp: message.timestamp, text: message.text, speaker: message.role === "user" ? user : "Agent" }));
      const dialogue = prepareDialogue(dialogueMessages);
      const first = pending[0]!, last = pending.at(-1)!;
      if (memory) {
        const result = await memory.ingest({ kind: "coding_session", messages: dialogueMessages,
          label: `${provider === "pi" ? "Pi" : provider} session ${session.id}: ${first.entryId} through ${last.entryId}`,
          happened: first.timestamp, sensitivity: 0.3, session: session.id, home: homeFor(routes.projects, session.cwd),
          externalId: provider === "pi" ? `pi:${session.id}:${last.entryId}` : `${session.id}:${last.entryId}`,
          metadata: { source: provider, capture: "daily", cwd: session.cwd, primary_jsonl: path,
            entry_ids: pending.map(message => message.entryId), first_entry_id: first.entryId, through_entry_id: last.entryId },
        }, false);
        if (result.conflict) throw new Error(`${provider} session ${session.id} conflicts with existing source: ${result.conflict}`);
        jobs.add(result.job);
        result.reused ? stats.reused++ : stats.submitted++;
      }
      stats.messages += dialogue.messages.length;
      stats.characters += dialogue.text.length;
      stats.userCharacters += dialogue.stats.userChars;
      stats.assistantCharacters += dialogue.stats.assistantChars;
    }
    if (values.wait && memory && jobs.size) {
      memory.store.recoverInterrupted(new Date(Date.now() - 30 * 60_000).toISOString());
      memory.store.retryRecoverableFailures();
      // Include earlier queue-only imports of these sessions, not just newly submitted jobs.
      processed = (await drain(memory.store, memory.config, [...jobs], concurrency)).length;
      const incomplete = [...jobs].map(id => memory.store.getJob(id)).filter(job => job?.status !== "completed");
      if (incomplete.length) throw new Error(`Session ingestion incomplete: ${incomplete.map(job => `${job?.id}:${job?.status}`).join(", ")}`);
    }
  } finally { memory?.close(); }
  console.log(JSON.stringify({ ...stats, dry_run: Boolean(values["dry-run"]), jobs: [...jobs], processed }, null, 2));
}

function isMainSession(path: string, provider: Provider): boolean {
  const normalized = path.split(sep).join("/");
  if (normalized.includes("/subagents/") || normalized.includes("/subagent-artifacts/")) return false;
  if (provider === "pi" && /\/run-[^/]+\/session\.jsonl$/.test(normalized)) return false;
  return provider !== "claude-code" || !basename(path).startsWith("agent-");
}
function homeFor(projects: Array<{ root: string; home: string }>, cwd: string): string {
  const route = [...projects].sort((a, b) => resolve(b.root).length - resolve(a.root).length).find(project => {
    const path = relative(resolve(project.root), resolve(cwd));
    return path === "" || (path !== ".." && !path.startsWith(`..${sep}`) && !isAbsolute(path));
  });
  if (route) return route.home;
  const leaf = basename(cwd).toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "");
  return /^[a-z][a-z0-9-]{1,63}$/.test(leaf) ? leaf : "root";
}
if (import.meta.main) await main();
