#!/usr/bin/env bun
import { readFile } from "node:fs/promises";
import { loadConfig, requireCredentials } from "./config";
import { EmbeddingClient, rebuildObservationEmbeddings } from "./embeddings";
import { drain, extractQueued, parseSpeakerLabels, processNext, submitManifest, submitSource } from "./ingestion";
import { OpenAIClient } from "./openai";
import { MemoryStore } from "./store";
import { commandCatalog, runCommand } from "./commands";
import { prepareDialogue } from "./dialogue";
import type { SearchResult } from "./retrieval";
import { SOURCE_KINDS, type SourceKind } from "./types";

const HELP = `pd-memory — progressive-disclosure memory

Usage:
  pd-memory catalog
  pd-memory doctor [--live] [--config PATH]
  pd-memory ingest (--path FILE | --stdin | --dialogue-json) --kind KIND --label TEXT [--home PAGE] [--happened DATE] [--sensitivity N] [--session KEY] [--external-id ID] [--metadata-json JSON] [--wait]
  pd-memory ingest --manifest FILE [--wait] [--extract-concurrency N]
  pd-memory worker [--one | --extract-only] [--extract-concurrency N]
  pd-memory jobs [--retry [JOB_ID]]
  pd-memory captured-session-entries --session KEY
  pd-memory search --query TEXT [--query TEXT ...] [--page SLUG ...] [--layer observations|chunks|both] [--limit N] [--profile NAME] [--json]
  pd-memory brief [--page SLUG] [--for SITUATION] [--query FACET ...] [--budget UNITS] [--since SEQ] [--profile NAME] [--json]
  pd-memory open ID [--history] [--full] [--profile NAME]
  pd-memory note --line TEXT --page SLUG [--page SLUG ...] --actor user|agent [--body TEXT] [score options] [--profile NAME]
  pd-memory edit ID --reason TEXT --actor user|agent [observation fields] [--supersede] [--profile NAME]
  pd-memory edit PAGE (--parent PAGE | --merge-into PAGE) --reason TEXT --actor user|agent [--profile NAME]
  pd-memory embeddings rebuild
  pd-memory stats

All commands accept --config PATH (default: PD_MEMORY_CONFIG or ./pd-memory.toml).
'ingest' durably queues work and returns immediately unless --wait is given.
'worker' recovers interrupted jobs and drains the queue serially; --one processes one job.
Kinds: ${SOURCE_KINDS.join(", ")}
`;

async function main(): Promise<void> {
  const args = process.argv.slice(2);
  const command = args.shift();
  if (!command || command === "help" || command === "--help" || command === "-h" || has(args, "--help")) { console.log(HELP); return; }
  if (command === "catalog") { assertEmpty(args); print({ commands: commandCatalog() }); return; }
  const configPath = take(args, "--config") ?? process.env.PD_MEMORY_CONFIG ?? "./pd-memory.toml";
  const config = await loadConfig(configPath);
  if (command === "doctor") {
    const local = {
      db: config.workspace.db,
      base_url: config.openai.baseUrl,
      credential_env: config.openai.apiKeyEnv,
      credential_set: Boolean(process.env[config.openai.apiKeyEnv]),
      model: config.openai.model,
      embedding_model: config.embeddings.model,
      embedding_provider: config.embeddings.provider,
      embedding_credential_env: config.embeddings.apiKeyEnv,
      embedding_credential_set: config.embeddings.apiKeyEnv === null || Boolean(process.env[config.embeddings.apiKeyEnv]),
      service_tier: config.openai.serviceTier,
      store: config.openai.store,
      retention_policy: config.openai.retentionPolicy,
      retention_verified: config.openai.retentionVerified,
    };
    if (!has(args, "--live")) { print(local); return; }
    requireCredentials(config);
    print({ ...local, live: { generation: await new OpenAIClient(config).probe(), embeddings: await new EmbeddingClient(config).probe() } });
    return;
  }
  const store = new MemoryStore(config.workspace.db);
  try {
    if (command === "ingest") {
      const manifest = take(args, "--manifest");
      let results;
      if (manifest) {
        results = await submitManifest(store, manifest);
      } else {
        const path = take(args, "--path");
        const stdin = has(args, "--stdin");
        const dialogueJson = has(args, "--dialogue-json");
        if (Number(path !== undefined) + Number(stdin) + Number(dialogueJson) !== 1) throw new Error("Choose exactly one of --path, --stdin, or --dialogue-json");
        const kind = required(take(args, "--kind"), "--kind") as SourceKind;
        if (!SOURCE_KINDS.includes(kind)) throw new Error(`Unknown source kind ${kind}`);
        const raw = path === undefined ? await Bun.stdin.text() : await readFile(path, "utf8");
        const metadataRaw = take(args, "--metadata-json");
        let metadata = metadataRaw === undefined ? {} : JSON.parse(metadataRaw);
        if (!metadata || typeof metadata !== "object" || Array.isArray(metadata)) throw new Error("--metadata-json must be an object");
        let text = raw;
        let participants = kind === "meeting" || kind === "conversation" ? parseSpeakerLabels(text) : [];
        if (dialogueJson) {
          const parsed = JSON.parse(raw) as unknown;
          const dialogue = prepareDialogue(Array.isArray(parsed) ? parsed
            : parsed && typeof parsed === "object" && "messages" in parsed ? parsed.messages : undefined);
          text = dialogue.text;
          participants = dialogue.participants;
          metadata = { ...metadata, dialogue: dialogue.stats };
        }
        results = [await submitSource(store, {
          kind, text, label: required(take(args, "--label"), "--label"),
          happened: take(args, "--happened") ?? null, sensitivity: numberOption(take(args, "--sensitivity"), 0.3, "--sensitivity"),
          session: take(args, "--session") ?? null, home: take(args, "--home") ?? null,
          externalId: take(args, "--external-id") ?? null, metadata, participants,
        })];
      }
      const conflicts = results.filter((result) => result.conflict);
      if (conflicts.length) { print({ submitted: results, conflicts }); process.exitCode = 2; return; }
      if (has(args, "--wait")) {
        const staleBefore = new Date(Date.now() - 30 * 60_000).toISOString();
        const recovered = store.recoverInterrupted(staleBefore);
        const retried = store.retryRecoverableFailures();
        const processed = await drain(store, config, results.map((result) => result.job),
          integerOption(take(args, "--extract-concurrency"), 4, "--extract-concurrency"));
        const incomplete = results.map((result) => store.getJob(result.job)).filter((job) => job?.status !== "completed");
        if (incomplete.length) throw new Error(`Waited ingestion did not complete: ${incomplete.map((job) => `${job?.id}:${job?.status ?? "unknown"}`).join(", ")}`);
        print({ submitted: results, recovered, retried, processed });
      } else print({ submitted: results });
      return;
    }
    if (command === "worker") {
      const recovered = store.recoverInterrupted();
      const concurrency = integerOption(take(args, "--extract-concurrency"), 4, "--extract-concurrency");
      const processed = has(args, "--one") ? [await processNext(store, config)].filter(Boolean)
        : has(args, "--extract-only") ? { extracted: await extractQueued(store, config, concurrency) }
        : await drain(store, config, [], concurrency);
      print({ recovered, processed }); return;
    }
    if (command === "jobs") {
      if (args.includes("--retry")) {
        const raw = valueAfterOptional(args, "--retry");
        const id = raw === null ? undefined : integer(raw, "job id");
        print({ retried: store.retryFailed(id), jobs: store.listJobs() });
      } else print({ jobs: store.listJobs() });
      return;
    }
    if (command === "captured-session-entries") {
      const session = required(take(args, "--session"), "--session");
      assertEmpty(args); print({ session, entry_ids: [...store.capturedSessionEntryIds(session)] }); return;
    }
    const context = { store, config, embeddings: new EmbeddingClient(config) };
    if (command === "search") {
      const json = has(args, "--json");
      const input = { queries: takeAll(args, "--query"), pages: optionalArray(takeAll(args, "--page")),
        layer: take(args, "--layer"), limit: optionalInteger(take(args, "--limit"), "--limit"), profile: take(args, "--profile") };
      assertEmpty(args); const result = await runCommand(context, "search", compact(input));
      if (json) print(result); else console.log(formatSearch(result)); return;
    }
    if (command === "brief") {
      const json = has(args, "--json");
      const input = { page: take(args, "--page"), for: take(args, "--for"), queries: optionalArray(takeAll(args, "--query")),
        budget: optionalInteger(take(args, "--budget"), "--budget"),
        since: optionalInteger(take(args, "--since"), "--since"), profile: take(args, "--profile") };
      assertEmpty(args); const result = await runCommand(context, "brief", compact(input));
      if (json) print(result); else console.log(result.text); return;
    }
    if (command === "open") {
      const input = { id: required(args.shift(), "ID"), history: has(args, "--history"), full: has(args, "--full"), profile: take(args, "--profile") };
      assertEmpty(args); print(await runCommand(context, "open", compact(input))); return;
    }
    if (command === "note") {
      const input = observationOptions(args, { line: required(take(args, "--line"), "--line"), pages: takeAll(args, "--page"),
        actor: required(take(args, "--actor"), "--actor"), body: take(args, "--body"), claimant: take(args, "--claimant"),
        kind: take(args, "--kind"), happened: take(args, "--happened"), profile: take(args, "--profile") });
      assertEmpty(args); print(await runCommand(context, "note", compact(input))); return;
    }
    if (command === "edit") {
      const input = observationOptions(args, { id: required(args.shift(), "ID"), reason: required(take(args, "--reason"), "--reason"),
        actor: required(take(args, "--actor"), "--actor"), line: take(args, "--line"), body: has(args, "--clear-body") ? null : take(args, "--body"),
        pages: optionalArray(takeAll(args, "--page")), claimant: take(args, "--claimant"), kind: take(args, "--kind"),
        authority: take(args, "--authority"), happened: take(args, "--happened"), supersede: has(args, "--supersede") || undefined,
        merge_into: take(args, "--merge-into"), parent: take(args, "--parent"), profile: take(args, "--profile") });
      assertEmpty(args); print(await runCommand(context, "edit", compact(input))); return;
    }
    if (command === "embeddings") {
      if (args.shift() !== "rebuild") throw new Error("Usage: pd-memory embeddings rebuild");
      print({ rebuilt: await rebuildObservationEmbeddings(store, new EmbeddingClient(config), config), model: config.embeddings.model }); return;
    }
    if (command === "stats") { print(store.stats()); return; }
    throw new Error(`Unknown command ${command}`);
  } finally { store.close(); }
}

function take(args: string[], flag: string): string | undefined {
  const index = args.indexOf(flag); if (index < 0) return undefined;
  const value = args[index + 1]; if (!value || value.startsWith("--")) throw new Error(`${flag} requires a value`);
  args.splice(index, 2); return value;
}
function takeAll(args: string[], flag: string): string[] {
  const values: string[] = [];
  while (args.includes(flag)) values.push(required(take(args, flag), flag));
  return values;
}
function has(args: string[], flag: string): boolean {
  const index = args.indexOf(flag); if (index < 0) return false; args.splice(index, 1); return true;
}
function valueAfterOptional(args: string[], flag: string): string | null {
  const index = args.indexOf(flag); if (index < 0) return null;
  const value = args[index + 1];
  if (!value || value.startsWith("--")) { args.splice(index, 1); return null; }
  args.splice(index, 2); return value;
}
function required<T>(value: T | undefined, name: string): T { if (value === undefined || value === "") throw new Error(`${name} is required`); return value; }
function numberOption(raw: string | undefined, fallback: number, name: string): number {
  if (raw === undefined) return fallback; const value = Number(raw); if (!Number.isFinite(value)) throw new Error(`${name} must be a number`); return value;
}
function integer(raw: string, name: string): number { const value = Number(raw); if (!Number.isInteger(value)) throw new Error(`${name} must be an integer`); return value; }
function integerOption(raw: string | undefined, fallback: number, name: string): number {
  if (raw === undefined) return fallback;
  const value = integer(raw, name); if (value < 1) throw new Error(`${name} must be positive`); return value;
}
function optionalInteger(raw: string | undefined, name: string): number | undefined {
  return raw === undefined ? undefined : integer(raw, name);
}
function optionalArray(values: string[]): string[] | undefined { return values.length ? values : undefined; }
function observationOptions(args: string[], base: Record<string, unknown>): Record<string, unknown> {
  return { ...base,
    confidence: optionalNumber(take(args, "--confidence"), "--confidence"),
    weight: optionalNumber(take(args, "--weight"), "--weight"),
    durability: optionalNumber(take(args, "--durability"), "--durability"),
    sensitivity: optionalNumber(take(args, "--sensitivity"), "--sensitivity"),
  };
}
function optionalNumber(raw: string | undefined, name: string): number | undefined {
  return raw === undefined ? undefined : numberOption(raw, 0, name);
}
function compact(value: Record<string, unknown>): Record<string, unknown> {
  return Object.fromEntries(Object.entries(value).filter(([, item]) => item !== undefined));
}
function assertEmpty(args: string[]): void { if (args.length) throw new Error(`Unknown arguments: ${args.join(" ")}`); }
function print(value: unknown): void { console.log(JSON.stringify(value, null, 2)); }
function formatSearch(result: SearchResult): string {
  const lines = result.hits.map((hit) => hit.kind === "observation"
    ? `${hit.id} [${hit.bucket}${hit.body ? "+" : ""}] ${hit.line} (${hit.sources})`
    : `${hit.id} [${hit.bucket}] ${hit.line}`);
  if (result.more) lines.push(`+${result.more} more`);
  return lines.join("\n");
}

main().catch((error) => { console.error(error instanceof Error ? error.message : String(error)); process.exitCode = 1; });
