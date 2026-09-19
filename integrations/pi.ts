import { spawn, spawnSync } from "node:child_process";
import { existsSync, readFileSync, realpathSync } from "node:fs";
import { homedir } from "node:os";
import { isAbsolute, join, relative, resolve } from "node:path";

type PiApi = {
  registerTool(tool: Record<string, unknown>): void;
  on(event: string, handler: (event: any, context: PiContext) => unknown): void;
};
type PiContext = {
  sessionManager: {
    getSessionId(): string;
    getSessionFile(): string | undefined;
    getCwd(): string;
    getBranch(): any[];
  };
  ui: { notify(message: string, level: "warning" | "error" | "info"): void };
};
type ProjectRoute = { root: string; home: string };
type PiMemoryConfig = {
  binary: string;
  cli: string;
  config: string;
  credentials?: string;
  projects?: ProjectRoute[];
  profile?: string;
  capture_mode?: "daily" | "live";
  capture_after?: string;
  timeout_ms?: number;
};
type CommandDefinition = { name: string; description: string; inputSchema: Record<string, unknown> };

const CONFIG_ENV = "PD_MEMORY_PI_CONFIG";
const CONFIG_PATH = process.env[CONFIG_ENV]?.trim() || join(homedir(), ".config", "pd-memory", "pi.json");

export default function pdMemory(pi: PiApi): void {
  if (!existsSync(CONFIG_PATH)) return;
  const settings = readSettings(CONFIG_PATH);
  const environment = childEnvironment(settings.credentials);
  registerTools(pi, settings, environment);
  let briefedSession: string | null = null;

  pi.on("before_agent_start", async (_event, context) => {
    const session = context.sessionManager.getSessionId();
    if (briefedSession === session) return;
    try {
      const result = await invoke(settings, environment, ["brief", "--profile", settings.profile ?? "coding", "--json"]);
      if (!result?.text?.trim()) return;
      briefedSession = session;
      return { message: { customType: "pd-memory-brief", content: `Standing memory brief\n\n${result.text}`, display: false } };
    } catch (error) {
      context.ui.notify(`pd-memory brief unavailable: ${message(error)}`, "warning");
    }
  });

  if (settings.capture_mode === "live") {
    let captureTail: Promise<void> = Promise.resolve();
    const queueCapture = (context: PiContext, lifecycle: string) => {
      const queued = captureTail.then(async () => {
        try { await captureIncrement(settings, environment, context); }
        catch (error) { context.ui.notify(`pd-memory capture failed at ${lifecycle}: ${message(error)}`, "warning"); }
      });
      captureTail = queued.catch(() => undefined);
      return queued;
    };
    pi.on("agent_settled", async (_event, context) => queueCapture(context, "agent settled"));
    pi.on("session_shutdown", async (_event, context) => queueCapture(context, "session shutdown"));
  }
}

function registerTools(pi: PiApi, settings: PiMemoryConfig, environment: NodeJS.ProcessEnv): void {
  const catalog = invokeSync(settings, environment, ["catalog"]) as { commands: CommandDefinition[] };
  for (const definition of catalog.commands) {
    if (!["search", "brief", "open", "note"].includes(definition.name)) continue;
    pi.registerTool({
      name: `memory_${definition.name}`,
      label: `Memory ${definition.name[0]!.toUpperCase()}${definition.name.slice(1)}`,
      description: definition.description,
      promptSnippet: definition.description,
      parameters: definition.inputSchema,
      async execute(_id: string, params: any, signal: AbortSignal) {
        const result = await invokeCommand(settings, environment, definition.name, params, signal);
        const text = definition.name === "brief" ? result.text : definition.name === "search" ? formatSearch(result) : JSON.stringify(result, null, 2);
        return { content: [{ type: "text", text }], details: result };
      },
    });
  }
}

async function invokeCommand(settings: PiMemoryConfig, environment: NodeJS.ProcessEnv, name: string, params: any, signal: AbortSignal): Promise<any> {
  if (name === "search") return invoke(settings, environment, ["search", ...repeat("--query", params.queries), ...repeat("--page", params.pages),
    ...(params.layer ? ["--layer", params.layer] : []), ...(params.limit ? ["--limit", String(params.limit)] : []),
    ...(params.profile ? ["--profile", params.profile] : []), "--json"], undefined, signal);
  if (name === "brief") return invoke(settings, environment, ["brief", ...(params.page ? ["--page", params.page] : []),
    ...(params.for ? ["--for", params.for] : []), ...repeat("--query", params.queries), ...(params.budget ? ["--budget", String(params.budget)] : []),
    ...(params.since !== undefined ? ["--since", String(params.since)] : []), ...(params.profile ? ["--profile", params.profile] : []), "--json"], undefined, signal);
  if (name === "open") return invoke(settings, environment, ["open", params.id, ...(params.history ? ["--history"] : []),
    ...(params.full ? ["--full"] : []), ...(params.profile ? ["--profile", params.profile] : [])], undefined, signal);
  if (name === "note") return invoke(settings, environment, ["note", "--line", params.line, ...repeat("--page", params.pages), "--actor", params.actor,
    ...(params.body ? ["--body", params.body] : []), ...(params.claimant ? ["--claimant", params.claimant] : []),
    ...(params.kind ? ["--kind", params.kind] : []), ...(params.happened ? ["--happened", params.happened] : []),
    ...numberArgs(params), ...(params.profile ? ["--profile", params.profile] : [])], undefined, signal);
  throw new Error(`Unsupported pd-memory command ${name}`);
}

async function captureIncrement(settings: PiMemoryConfig, environment: NodeJS.ProcessEnv, context: PiContext): Promise<void> {
  const sessionFile = context.sessionManager.getSessionFile();
  if (!sessionFile || /\/run-[^/]+\/session\.jsonl$/.test(sessionFile)) return;
  const session = context.sessionManager.getSessionId();
  const cursor = await invoke(settings, environment, ["captured-session-entries", "--session", session]) as { entry_ids: string[] };
  const captured = new Set(cursor.entry_ids);
  const messages = sessionMessages(context.sessionManager.getBranch()).filter((item) =>
    !captured.has(item.entryId) && (settings.capture_after === undefined || item.timestamp >= settings.capture_after));
  if (!messages.some((item) => item.role === "user")) return;
  const route = routeFor(settings, context.sessionManager.getCwd());
  const first = messages[0]!;
  const last = messages[messages.length - 1]!;
  await invoke(settings, environment, ["ingest", "--dialogue-json", "--kind", "coding_session", "--label", `Pi session ${session}: ${first.entryId} through ${last.entryId}`,
    "--happened", first.timestamp, "--sensitivity", "0.3", "--session", session, "--home", route.home,
    "--external-id", `pi:${session}:${last.entryId}`, "--metadata-json", JSON.stringify({ source: "pi", cwd: canonical(context.sessionManager.getCwd()),
      entry_ids: messages.map((item) => item.entryId), first_entry_id: first.entryId, through_entry_id: last.entryId }), "--wait"], JSON.stringify(messages.map((item) => ({
        id: item.entryId, role: item.role, timestamp: item.timestamp, text: item.text,
        speaker: item.role === "user" ? "Mark" : "Agent",
      }))));
}

function sessionMessages(entries: any[]): Array<{ entryId: string; role: "user" | "assistant"; timestamp: string; text: string }> {
  const result: Array<{ entryId: string; role: "user" | "assistant"; timestamp: string; text: string }> = [];
  for (const entry of entries) {
    if (entry?.type !== "message") continue;
    const role = entry.message?.role;
    if (role !== "user" && role !== "assistant") continue;
    if (role === "assistant" && entry.message.stopReason !== "stop") continue;
    const text = messageText(entry.message.content).replaceAll("\u0000", "").trim();
    if (text) result.push({ entryId: entry.id, role, timestamp: new Date(entry.timestamp).toISOString(), text });
  }
  return result;
}

function invokeSync(settings: PiMemoryConfig, environment: NodeJS.ProcessEnv, args: string[]): unknown {
  const result = spawnSync(settings.binary, [settings.cli, ...args], {
    env: environment, encoding: "utf8", maxBuffer: 5 * 1024 * 1024, timeout: 10_000,
  });
  if (result.error) throw new Error(`Could not start pd-memory: ${result.error.message}`);
  if (result.status !== 0) throw new Error(`pd-memory ${args[0]} failed: ${(result.stderr ?? "").trim().slice(-4000)}`);
  return JSON.parse(result.stdout);
}

function invoke(settings: PiMemoryConfig, environment: NodeJS.ProcessEnv, args: string[], stdin?: string, signal?: AbortSignal): Promise<any> {
  if (signal?.aborted) return Promise.reject(new Error("pd-memory operation cancelled"));
  return new Promise((resolvePromise, rejectPromise) => {
    const child = spawn(settings.binary, [settings.cli, ...args, "--config", settings.config], { env: environment, stdio: ["pipe", "pipe", "pipe"] });
    const stdout: Buffer[] = [], stderr: Buffer[] = [];
    let settled = false;
    const finish = (error?: Error, value?: unknown) => {
      if (settled) return; settled = true; clearTimeout(timer); signal?.removeEventListener("abort", abort);
      error ? rejectPromise(error) : resolvePromise(value);
    };
    const abort = () => { child.kill("SIGTERM"); finish(new Error("pd-memory operation cancelled")); };
    const timer = setTimeout(() => { child.kill("SIGTERM"); finish(new Error("pd-memory operation timed out")); }, settings.timeout_ms ?? 3_600_000);
    signal?.addEventListener("abort", abort, { once: true });
    child.stdout.on("data", (chunk) => stdout.push(chunk)); child.stderr.on("data", (chunk) => stderr.push(chunk));
    child.on("error", (error) => finish(error));
    child.on("close", (code) => {
      if (settled) return;
      if (code !== 0) {
        const errorText = Buffer.concat(stderr).toString("utf8").trim() || Buffer.concat(stdout).toString("utf8").trim();
        return finish(new Error(`pd-memory ${args[0]} failed: ${errorText.slice(-4000)}`));
      }
      try { finish(undefined, JSON.parse(Buffer.concat(stdout).toString("utf8"))); }
      catch (error) { finish(new Error(`pd-memory returned invalid JSON: ${message(error)}`)); }
    });
    child.stdin.end(stdin);
  });
}

function readSettings(path: string): PiMemoryConfig {
  const raw = JSON.parse(readFileSync(path, "utf8")) as PiMemoryConfig;
  for (const key of ["binary", "cli", "config"] as const) if (typeof raw[key] !== "string" || !isAbsolute(raw[key])) throw new Error(`${CONFIG_ENV} requires absolute ${key}`);
  if (raw.capture_mode !== undefined && raw.capture_mode !== "daily" && raw.capture_mode !== "live") {
    throw new Error(`${CONFIG_ENV} capture_mode must be daily or live`);
  }
  if (raw.capture_after !== undefined && Number.isNaN(Date.parse(raw.capture_after))) throw new Error(`${CONFIG_ENV} capture_after must be an ISO timestamp`);
  return { ...raw, capture_mode: raw.capture_mode ?? "daily", binary: resolve(raw.binary), cli: resolve(raw.cli), config: resolve(raw.config),
    credentials: raw.credentials ? resolve(raw.credentials) : undefined,
    capture_after: raw.capture_after === undefined ? undefined : new Date(raw.capture_after).toISOString(),
    projects: raw.projects?.map((item) => ({ root: canonical(item.root), home: item.home })) };
}

function childEnvironment(credentials: string | undefined): NodeJS.ProcessEnv {
  const environment = { ...process.env };
  if (!credentials) return environment;
  for (const line of readFileSync(credentials, "utf8").split(/\r?\n/)) {
    const match = /^([A-Za-z_][A-Za-z0-9_]*)=(.*)$/.exec(line.trim());
    if (!match || environment[match[1]!]) continue;
    let value = match[2]!.trim();
    if ((value.startsWith('"') && value.endsWith('"')) || (value.startsWith("'") && value.endsWith("'"))) value = value.slice(1, -1);
    environment[match[1]!] = value;
  }
  return environment;
}

function routeFor(settings: PiMemoryConfig, cwd: string): ProjectRoute {
  const current = canonical(cwd);
  const match = [...(settings.projects ?? [])].filter((item) => inside(item.root, current)).sort((a, b) => b.root.length - a.root.length)[0];
  if (match) return match;
  const leaf = current.split("/").filter(Boolean).at(-1) ?? "root";
  const home = leaf.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "");
  return { root: current, home: /^[a-z][a-z0-9-]{1,63}$/.test(home) ? home : "root" };
}
function numberArgs(params: any): string[] {
  return ["confidence", "weight", "durability", "sensitivity"].flatMap((name) => params[name] === undefined || params[name] === null ? [] : [`--${name}`, String(params[name])]);
}
function repeat(flag: string, values: unknown): string[] { return Array.isArray(values) ? values.flatMap((value) => [flag, String(value)]) : []; }
function messageText(content: unknown): string {
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return "";
  return content.filter((item: any) => item?.type === "text" && typeof item.text === "string").map((item: any) => item.text).join("\n");
}
function formatSearch(result: any): string {
  const lines = (result.hits ?? []).map((hit: any) => hit.kind === "observation" ? `${hit.id} [${hit.bucket}${hit.body ? "+" : ""}] ${hit.line} (${hit.sources})` : `${hit.id} [${hit.bucket}] ${hit.line}`);
  if (result.more) lines.push(`+${result.more} more`); return lines.join("\n");
}
function inside(root: string, path: string): boolean { const child = relative(root, path); return child === "" || (!child.startsWith("..") && !isAbsolute(child)); }
function canonical(path: string): string { try { return realpathSync(resolve(path)); } catch { return resolve(path); } }
function message(error: unknown): string { return error instanceof Error ? error.message : String(error); }
