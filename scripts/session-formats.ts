import { readFileSync } from "node:fs";

export type Provider = "pi" | "claude-code" | "codex";
type Row = Record<string, unknown>;
export type SessionMessage = { entryId: string; role: "user" | "assistant"; timestamp: string; text: string; complete: boolean };
export type Session = { id: string; cwd: string; messages: SessionMessage[] };
const object = (value: unknown): Row => value !== null && typeof value === "object" && !Array.isArray(value) ? value as Row : {};
const text = (value: unknown): string => typeof value === "string" ? value : "";

function content(value: unknown): string {
  if (typeof value === "string") return value;
  if (!Array.isArray(value)) return "";
  return value.map(object).filter(block => ["text", "input_text", "output_text"].includes(text(block.type)))
    .map(block => text(block.text)).join("\n");
}
function message(id: unknown, role: "user" | "assistant", timestamp: unknown, body: string, complete: boolean): SessionMessage {
  if (!text(id)) throw new Error("Session message has no stable id");
  if (!text(timestamp) || !Number.isFinite(Date.parse(text(timestamp)))) throw new Error(`Invalid timestamp for session message ${id}`);
  return { entryId: text(id), role, timestamp: new Date(text(timestamp)).toISOString(), text: body, complete };
}
function branch(rows: Row[], id: string, parent: string): Row[] {
  const entries = rows.filter(row => text(row[id]));
  const byId = new Map(entries.map(row => [text(row[id]), row]));
  const result: Row[] = [], seen = new Set<string>();
  let current = entries.at(-1);
  while (current) {
    const key = text(current[id]);
    if (seen.has(key)) throw new Error(`Cycle in session branch at ${key}`);
    seen.add(key); result.push(current);
    const next = text(current[parent]);
    if (next && !byId.has(next)) throw new Error(`Missing session parent ${next}`);
    current = next ? byId.get(next) : undefined;
  }
  return result.reverse();
}

export function parseSession(path: string, provider: Provider): Session | null {
  const raw = readFileSync(path, "utf8");
  const lines = raw.split("\n");
  const rows: Row[] = [];
  for (let index = 0; index < lines.length; index++) {
    if (!lines[index]!.trim()) continue;
    try {
      const value: unknown = JSON.parse(lines[index]!);
      if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error("Expected an object");
      rows.push(value as Row);
    } catch (error) {
      if (index === lines.length - 1 && !raw.endsWith("\n")) break;
      throw new Error(`Invalid JSONL in ${path}:${index + 1}: ${error}`);
    }
  }
  if (provider === "pi") return pi(rows);
  if (provider === "claude-code") return claude(rows);
  return codex(rows);
}

function pi(rows: Row[]): Session | null {
  const header = rows[0];
  if (header?.type !== "session" || !text(header.id) || !text(header.cwd)) return null;
  const messages: SessionMessage[] = [];
  for (const row of branch(rows.slice(1), "id", "parentId")) {
    if (row.type !== "message") continue;
    const value = object(row.message), role = value.role;
    if (role !== "user" && role !== "assistant") continue;
    if (role === "assistant" && value.stopReason !== "stop") continue;
    const body = content(value.content);
    if (body.trim()) messages.push(message(row.id, role, row.timestamp, body, role === "assistant"));
  }
  // Keep the existing Pi namespace so daily and live capture share their cursor.
  return { id: text(header.id), cwd: text(header.cwd), messages };
}

function claude(rows: Row[]): Session | null {
  const entries = rows.filter(row => row.isSidechain !== true);
  const header = entries.find(row => text(row.sessionId) && text(row.cwd));
  if (!header) return null;
  const messages: SessionMessage[] = [];
  for (const row of branch(entries, "uuid", "parentUuid")) {
    if (row.type !== "user" && row.type !== "assistant") continue;
    if (row.isMeta === true || row.isCompactSummary === true || row.isApiErrorMessage === true) continue;
    const value = object(row.message), role = value.role;
    if (role === "user" && Array.isArray(value.content) && value.content.some(block => object(block).type === "tool_result")) continue;
    if (role !== "user" && role !== "assistant") continue;
    const body = content(value.content);
    if (!body.trim()) continue;
    // Native logs can omit stop_reason. A following human message closes that exchange;
    // an unmarked trailing assistant message waits for a later import.
    if (role === "user" && messages.at(-1)?.role === "assistant") messages.at(-1)!.complete = true;
    if (role === "assistant" && ![undefined, null, "end_turn", "stop_sequence"].includes(value.stop_reason as string | null | undefined)) continue;
    messages.push(message(row.uuid, role, row.timestamp, body, ["end_turn", "stop_sequence"].includes(text(value.stop_reason))));
  }
  return { id: `claude-code:${header.sessionId}`, cwd: text(header.cwd), messages };
}

function codex(rows: Row[]): Session | null {
  const header = object(rows.find(row => row.type === "session_meta")?.payload);
  if (!text(header.id) || !text(header.cwd) || "subagent" in object(header.source)) return null;
  const messages: SessionMessage[] = [];
  for (const [index, row] of rows.entries()) {
    const value = object(row.payload);
    if (row.type === "event_msg" && value.type === "task_complete") {
      if (messages.at(-1)?.role === "assistant") messages.at(-1)!.complete = true;
      continue;
    }
    // response_item is canonical. event_msg mirrors must not become duplicate dialogue.
    if (row.type !== "response_item" || value.type !== "message") continue;
    const role = value.role;
    if (role !== "user" && role !== "assistant") continue;
    if (role === "assistant" && value.phase !== undefined && value.phase !== null && !["final", "final_answer"].includes(text(value.phase))) continue;
    const body = content(value.content);
    if (!body.trim()) continue;
    if (role === "user" && messages.at(-1)?.role === "assistant") messages.at(-1)!.complete = true;
    messages.push(message(text(value.id) || `row:${index + 1}`, role, row.timestamp, body,
      role === "assistant" && ["final", "final_answer"].includes(text(value.phase))));
  }
  return { id: `codex:${header.id}`, cwd: text(header.cwd), messages };
}

/** Keep a partial final turn for the next import, including its user message. */
export function completedMessages(messages: SessionMessage[]): SessionMessage[] {
  for (let index = messages.length - 1; index >= 0; index--) {
    if (messages[index]!.role === "assistant" && messages[index]!.complete) return messages.slice(0, index + 1);
  }
  return [];
}
