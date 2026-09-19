import { readFileSync } from "node:fs";
import { basename, relative, resolve } from "node:path";
import { PdMemoryRuntime, drain } from "../src/index.ts";
import { prepareDialogue } from "../src/dialogue.ts";

const args = process.argv.slice(2);
const configPath = requiredOption(args, "--config");
const routeConfigPath = requiredOption(args, "--routes");
const roots = repeatedOption(args, "--root");
if (roots.length === 0) throw new Error("At least one --root is required");
const wait = args.includes("--wait");
const extractionConcurrency = positiveInteger(option(args, "--extract-concurrency") ?? "4", "--extract-concurrency");
const routeConfig = JSON.parse(readFileSync(routeConfigPath, "utf8")) as {
  capture_after?: string;
  projects?: Array<{ root: string; home: string }>;
};
const captureAfter = routeConfig.capture_after === undefined ? null : new Date(routeConfig.capture_after).toISOString();
const projects = routeConfig.projects ?? [];
const paths = [...new Set(roots.flatMap((root) => [...new Bun.Glob("**/*.jsonl").scanSync({
  cwd: root, absolute: true, onlyFiles: true,
})]))].filter(isMainSessionPath).sort();

const memory = await PdMemoryRuntime.open(configPath);
let submitted = 0;
let reused = 0;
let skipped = 0;
let messages = 0;
let characters = 0;
let userCharacters = 0;
let assistantCharacters = 0;
const jobs: number[] = [];
let processed: unknown[] = [];
try {
  for (const path of paths) {
    const parsed = parseSession(path);
    if (parsed === null) { skipped++; continue; }
    const captured = memory.capturedSessionEntryIds(parsed.id);
    const uncaptured = parsed.messages.filter((message) => !captured.has(message.entryId)
      && (captureAfter === null || message.timestamp >= captureAfter));
    const completeThrough = lastCompletedAssistant(uncaptured);
    const pending = completeThrough < 0 ? [] : uncaptured.slice(0, completeThrough + 1);
    if (!pending.some((message) => message.role === "user")) { skipped++; continue; }
    const dialogueMessages = pending.map((message) => ({
      id: message.entryId, role: message.role, timestamp: message.timestamp, text: message.text,
      speaker: message.role === "user" ? "Mark" : "Agent",
    }));
    const dialogue = prepareDialogue(dialogueMessages);
    const first = pending[0]!;
    const last = pending.at(-1)!;
    const result = await memory.ingest({
      kind: "coding_session",
      messages: dialogueMessages,
      label: `Pi session ${parsed.id}: ${first.entryId} through ${last.entryId}`,
      happened: first.timestamp,
      sensitivity: 0.3,
      session: parsed.id,
      home: homeFor(projects, parsed.cwd),
      externalId: `pi:${parsed.id}:${last.entryId}`,
      metadata: {
        source: "pi", capture: "daily", cwd: parsed.cwd, primary_jsonl: path,
        entry_ids: pending.map((message) => message.entryId),
        first_entry_id: first.entryId, through_entry_id: last.entryId,
      },
    }, false) as { job: number; reused: boolean; conflict?: string };
    if (result.conflict) throw new Error(`Pi session ${parsed.id} conflicts with existing source: ${result.conflict}`);
    jobs.push(result.job);
    result.reused ? reused++ : submitted++;
    messages += dialogue.messages.length;
    characters += dialogue.text.length;
    userCharacters += dialogue.stats.userChars;
    assistantCharacters += dialogue.stats.assistantChars;
  }
  if (wait && jobs.length) {
    memory.store.recoverInterrupted(new Date(Date.now() - 30 * 60_000).toISOString());
    memory.store.retryRecoverableFailures();
    processed = await drain(memory.store, memory.config, [...new Set(jobs)], extractionConcurrency);
    const incomplete = [...new Set(jobs)].map((job) => memory.store.getJob(job)).filter((job) => job?.status !== "completed");
    if (incomplete.length) throw new Error(`Daily Pi ingestion did not complete: ${incomplete.map((job) => `${job?.id}:${job?.status ?? "unknown"}`).join(", ")}`);
  }
} finally {
  memory.close();
}
console.log(JSON.stringify({ files: paths.length, submitted, reused, skipped, messages, characters,
  userCharacters, assistantCharacters, jobs: [...new Set(jobs)], processed: processed.length }, null, 2));

type SessionMessage = { entryId: string; role: "user" | "assistant"; timestamp: string; text: string };
function parseSession(path: string): { id: string; cwd: string; messages: SessionMessage[] } | null {
  const text = readFileSync(path, "utf8");
  const lines = text.split(/\n/).filter(Boolean);
  const rows = lines.flatMap((line, index) => {
    try { return [JSON.parse(line)]; }
    catch (error) {
      if (index === lines.length - 1 && !text.endsWith("\n")) return [];
      throw new Error(`Invalid Pi JSONL in ${path} at line ${index + 1}: ${error instanceof Error ? error.message : String(error)}`);
    }
  });
  const header = rows[0];
  if (header?.type !== "session" || typeof header.id !== "string" || typeof header.cwd !== "string") return null;
  const entries = rows.slice(1).filter((row) => typeof row?.id === "string");
  const byId = new Map(entries.map((row) => [row.id, row]));
  const branch: any[] = [];
  let current = entries.at(-1);
  while (current !== undefined) {
    branch.push(current);
    current = typeof current.parentId === "string" ? byId.get(current.parentId) : undefined;
  }
  branch.reverse();
  const messages = branch.flatMap((entry): SessionMessage[] => {
    if (entry.type !== "message") return [];
    const role = entry.message?.role;
    if (role !== "user" && role !== "assistant") return [];
    if (role === "assistant" && entry.message.stopReason !== "stop") return [];
    const content = entry.message.content;
    const rawText = typeof content === "string" ? content : Array.isArray(content)
      ? content.filter((item) => item?.type === "text" && typeof item.text === "string").map((item) => item.text).join("\n")
      : "";
    if (!rawText.trim()) return [];
    return [{ entryId: entry.id, role, timestamp: new Date(entry.timestamp).toISOString(), text: rawText }];
  });
  return { id: header.id, cwd: header.cwd, messages };
}

function lastCompletedAssistant(messages: SessionMessage[]): number {
  for (let index = messages.length - 1; index >= 0; index--) if (messages[index]!.role === "assistant") return index;
  return -1;
}
function isMainSessionPath(path: string): boolean {
  return !/\/run-[^/]+\/session\.jsonl$/.test(path) && !path.includes("/subagent-artifacts/");
}
function homeFor(projects: Array<{ root: string; home: string }>, cwd: string): string {
  const route = [...projects].sort((left, right) => resolve(right.root).length - resolve(left.root).length)
    .find((project) => {
      const path = relative(resolve(project.root), resolve(cwd));
      return path === "" || (!path.startsWith("..") && !path.includes("../"));
    });
  if (route !== undefined) return route.home;
  const leaf = basename(cwd).toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "");
  return /^[a-z][a-z0-9-]{1,63}$/.test(leaf) ? leaf : "root";
}
function requiredOption(values: string[], name: string): string {
  const index = values.indexOf(name);
  const value = index < 0 ? undefined : values[index + 1];
  if (value === undefined || value.startsWith("--")) throw new Error(`${name} is required`);
  return resolve(value);
}
function repeatedOption(values: string[], name: string): string[] {
  const result: string[] = [];
  for (let index = 0; index < values.length; index++) {
    if (values[index] !== name) continue;
    const value = values[index + 1];
    if (value === undefined || value.startsWith("--")) throw new Error(`${name} requires a value`);
    result.push(resolve(value));
  }
  return result;
}
function option(values: string[], name: string): string | undefined {
  const index = values.indexOf(name);
  if (index < 0) return undefined;
  const value = values[index + 1];
  if (value === undefined || value.startsWith("--")) throw new Error(`${name} requires a value`);
  return value;
}
function positiveInteger(value: string, name: string): number {
  const parsed = Number(value);
  if (!Number.isInteger(parsed) || parsed < 1) throw new Error(`${name} must be a positive integer`);
  return parsed;
}
