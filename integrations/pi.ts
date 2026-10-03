import { MemoryClient } from "./client";
import type { Command, Commands } from "./types";
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
  config: string;
  credentials?: string;
  projects?: ProjectRoute[];
  capture_mode?: "daily" | "live";
  capture_after?: string;
  timeout_ms?: number;
  situational_budget?: number;
};

const CONFIG_ENV = "PD_MEMORY_PI_CONFIG";
const CONFIG_PATH = process.env[CONFIG_ENV]?.trim() || join(homedir(), ".config", "pd-memory", "pi.json");

export default async function pdMemory(pi: PiApi): Promise<void> {
  if (!existsSync(CONFIG_PATH)) return;
  const settings = readSettings(CONFIG_PATH);
  const environment = childEnvironment(settings.credentials);
  const client = new MemoryClient({ ...settings, env: environment, timeout_ms: settings.timeout_ms ?? 3_600_000 });
  await registerTools(pi, client);
  // The first prompt gets the standing brief plus a recall for that prompt; a failed standing brief retries next prompt.
  // The session itself records the injection, so a resumed session in a new process is not briefed twice.
  pi.on("before_agent_start", async (event, context) => {
    if (context.sessionManager.getBranch().some((entry) => entry?.type === "custom_message" && entry.customType === "pd-memory-brief")) return;
    const folder = canonical(context.sessionManager.getCwd());
    try {
      const standing = await client.call("brief", { folder });
      let content = `Standing memory brief\n\n${standing.text}`;
      try {
        const recalled = await client.call("recall", { queries: [event.prompt], folder, budget: settings.situational_budget ?? 1500 });
        content += `\n\nRecalled for your first request\n\n${recalled.text}`;
      } catch (error) {
        context.ui.notify(`pd-memory recall unavailable: ${message(error)}`, "warning");
      }
      content += "\n\nWhen the conversation turns to a new topic or task, consider calling memory_recall for it.";
      return { message: { customType: "pd-memory-brief", content, display: false } };
    } catch (error) {
      context.ui.notify(`pd-memory brief unavailable: ${message(error)}`, "warning");
    }
  });

  if (settings.capture_mode === "live") {
    let captureTail: Promise<void> = Promise.resolve();
    const queueCapture = (context: PiContext, lifecycle: string) => {
      const queued = captureTail.then(async () => {
        try { await captureIncrement(settings, client, context); }
        catch (error) { context.ui.notify(`pd-memory capture failed at ${lifecycle}: ${message(error)}`, "warning"); }
      });
      captureTail = queued.catch(() => undefined);
      return queued;
    };
    pi.on("agent_settled", async (_event, context) => queueCapture(context, "agent settled"));
    pi.on("session_shutdown", async (_event, context) => queueCapture(context, "session shutdown"));
  }
}

async function registerTools(pi: PiApi, client: MemoryClient): Promise<void> {
  const catalog = await client.call("catalog", {});
  for (const definition of catalog.commands) {
    if (!["recall", "brief", "open", "browse", "note", "edit", "focus", "forget"].includes(definition.name)) continue;
    pi.registerTool({
      name: `memory_${definition.name}`,
      label: `Memory ${definition.name[0]!.toUpperCase()}${definition.name.slice(1)}`,
      description: definition.description,
      promptSnippet: definition.description,
      parameters: ["recall", "brief", "focus"].includes(definition.name) ? withoutFolder(definition.inputSchema) : definition.inputSchema,
      async execute(_id: string, params: any, signal: AbortSignal, _onUpdate: unknown, context: PiContext) {
        const result = await invokeCommand(client, definition.name as Command, params, signal, context);
        const text = "text" in result && ["recall", "brief"].includes(definition.name) ? result.text : JSON.stringify(result, null, 2);
        return { content: [{ type: "text", text }], details: result };
      },
    });
  }
}

async function invokeCommand<C extends Command>(client: MemoryClient, name: C, params: Commands[C]["input"], signal: AbortSignal, context: PiContext): Promise<Commands[C]["result"]> {
  const input: any = { ...params };
  if (name === "focus" || name === "recall" || (name === "brief" && !input.page && input.since === undefined)) {
    input.folder = canonical(context.sessionManager.getCwd());
  }
  return client.call(name, input, { signal });
}

async function captureIncrement(settings: PiMemoryConfig, client: MemoryClient, context: PiContext): Promise<void> {
  const sessionFile = context.sessionManager.getSessionFile();
  if (!sessionFile || /\/run-[^/]+\/session\.jsonl$/.test(sessionFile)) return;
  const session = context.sessionManager.getSessionId();
  const cursor = await client.call("captured-session-entries", { session });
  const captured = new Set(cursor.entry_ids);
  const messages = sessionMessages(context.sessionManager.getBranch()).filter((item) =>
    !captured.has(item.entryId) && (settings.capture_after === undefined || item.timestamp >= settings.capture_after));
  if (!messages.some((item) => item.role === "user")) return;
  const route = routeFor(settings, context.sessionManager.getCwd());
  const first = messages[0]!;
  const last = messages[messages.length - 1]!;
  await client.call("ingest", { dialogue_json: true, kind: "coding_session", label: `Pi session ${session}: ${first.entryId} through ${last.entryId}`,
    happened: first.timestamp, session, home: route.home, external_id: `pi:${session}:${last.entryId}`,
    metadata_json: JSON.stringify({ source: "pi", cwd: canonical(context.sessionManager.getCwd()),
      entry_ids: messages.map((item) => item.entryId), first_entry_id: first.entryId, through_entry_id: last.entryId }), wait: true },
    { stdin: JSON.stringify(messages.map((item) => ({ id: item.entryId, role: item.role, timestamp: item.timestamp, text: item.text,
      speaker: item.role === "user" ? "Mark" : "Agent" }))) });
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

function readSettings(path: string): PiMemoryConfig {
  const raw = JSON.parse(readFileSync(path, "utf8")) as PiMemoryConfig;
  if (typeof raw.binary !== "string" || !raw.binary.trim()) throw new Error(`${CONFIG_ENV} requires binary`);
  if (typeof raw.config !== "string" || !isAbsolute(raw.config)) throw new Error(`${CONFIG_ENV} requires absolute config`);
  if (raw.capture_mode !== undefined && raw.capture_mode !== "daily" && raw.capture_mode !== "live") {
    throw new Error(`${CONFIG_ENV} capture_mode must be daily or live`);
  }
  if (raw.situational_budget !== undefined && (!Number.isSafeInteger(raw.situational_budget) || raw.situational_budget < 200)) {
    throw new Error(`${CONFIG_ENV} situational_budget must be an integer of at least 200`);
  }
  if (raw.capture_after !== undefined && Number.isNaN(Date.parse(raw.capture_after))) throw new Error(`${CONFIG_ENV} capture_after must be an ISO timestamp`);
  return { ...raw, capture_mode: raw.capture_mode ?? "daily", binary: raw.binary, config: resolve(raw.config),
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
function withoutFolder(schema: Record<string, unknown>): Record<string, unknown> {
  const properties = { ...(schema.properties as Record<string, unknown>) };
  delete properties.folder;
  return { ...schema, properties, required: (schema.required as string[] | undefined)?.filter((key) => key !== "folder") };
}
function messageText(content: unknown): string {
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return "";
  return content.filter((item: any) => item?.type === "text" && typeof item.text === "string").map((item: any) => item.text).join("\n");
}
function inside(root: string, path: string): boolean { const child = relative(root, path); return child === "" || (!child.startsWith("..") && !isAbsolute(child)); }
function canonical(path: string): string { try { return realpathSync(resolve(path)); } catch { return resolve(path); } }
function message(error: unknown): string { return error instanceof Error ? error.message : String(error); }
