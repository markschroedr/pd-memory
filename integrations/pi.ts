import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { spawn } from "node:child_process";
import { realpathSync } from "node:fs";
import { resolve } from "node:path";
import { MemoryClient } from "./client";
import type { Command } from "./types";

/** The engine owns configuration, source discovery, settlement, cursors and credentials. */
export default function pdMemory(pi: ExtensionAPI): void {
  const client = new MemoryClient();
  let registered = false;
  pi.on("session_start", async (_event, context) => {
    // A detached operator call must never keep a prompt or the Pi process waiting.
    const child = spawn("pd-memory", ["sync", "--auto"], { detached: true, stdio: "ignore" });
    child.on("error", error => context.ui.notify(`pd-memory sync unavailable: ${error.message}`, "warning"));
    child.on("exit", code => { if (code !== null && code !== 0) context.ui.notify(`pd-memory sync failed (exit ${code}); run pd-memory sync for details.`, "warning"); });
    child.unref();
    if (registered) return;
    try {
      const catalog = await client.call("catalog", {});
      for (const definition of catalog.commands) {
        if (!["recall", "brief", "open", "note", "focus"].includes(definition.name)) continue;
        const name = definition.name as Command;
        const parameters = toolSchema(definition.name, definition.inputSchema);
        const description = descriptions[definition.name] ?? definition.description;
        pi.registerTool({
          name: `memory_${name}`,
          label: `Memory ${name[0]!.toUpperCase()}${name.slice(1)}`,
          description,
          promptSnippet: description,
          parameters: parameters as any,
          async execute(_id, params: any, signal, _onUpdate, ctx) {
            const input: any = Object.fromEntries(Object.entries(params).filter(([, value]) => value !== null).map(([key, value]) =>
              [key, typeof value === "string" ? value.trim() : Array.isArray(value) ? value.map(item => typeof item === "string" ? item.trim() : item) : value]));
            for (const value of Object.values(input)) {
              if (value === "" || (Array.isArray(value) && (value.length === 0 || value.some(item => item === "")))) throw new Error("Memory strings must be nonempty after trimming.");
            }
            if (name === "focus" || name === "recall" || (name === "brief" && !input.page && input.since === undefined)) input.folder = canonical(ctx.cwd);
            if (name === "note") input.actor = "agent";
            const result = await client.call(name, input, { signal });
            const text = "text" in result && (name === "recall" || name === "brief") ? String(result.text) : JSON.stringify(result, null, 2);
            return { content: [{ type: "text" as const, text }], details: result };
          },
        });
      }
      registered = true;
    } catch (error) { context.ui.notify(`pd-memory tools unavailable: ${message(error)}`, "warning"); }
  });
  pi.on("before_agent_start", async (event, context) => {
    // Memory is on exactly when its tools are active; hosts switch it off, as minimal context does, by deactivating them.
    if (!pi.getActiveTools().includes("memory_brief")) return;
    try {
      const brief = await client.call("brief", { folder: canonical(context.cwd) });
      const block = `<context-block name="memory_brief">\n${brief.text}\n</context-block>`;
      event.systemPromptOptions.appendSystemPrompt = [event.systemPromptOptions.appendSystemPrompt, block].filter(Boolean).join("\n\n");
    } catch (error) { context.ui.notify(`pd-memory brief unavailable: ${message(error)}`, "warning"); }
  });
}

const fields: Record<string, string[]> = {
  recall: ["queries", "pages", "budget"], brief: ["page", "budget", "since"],
  open: ["id", "history", "full"], focus: ["action", "id"],
  note: ["line", "body", "pages", "claimant", "kind", "happened", "confidence"],
};
const required: Record<string, string[]> = { recall: ["queries"], brief: [], open: ["id"], focus: ["action", "id"], note: ["line", "pages"] };
function toolSchema(name: string, schema: Record<string, unknown>): Record<string, unknown> {
  const original = schema.properties as Record<string, any>;
  const properties = Object.fromEntries(fields[name]!.map(key => {
    let value = structuredClone(original[key]);
    if (["line", "body", "claimant", "happened", "page", "id"].includes(key)) value = { type: "string", minLength: 1 };
    if (key === "queries" || key === "pages") value = { type: "array", minItems: 1, items: { type: "string", minLength: 1 } };
    if (key === "budget") value = { type: "integer", minimum: 200 };
    if (key === "since") value = { type: "integer", minimum: 0 };
    if (key === "confidence") value = { type: "number", minimum: 0, maximum: 1 };
    if (required[name]!.includes(key)) return [key, value];
    // Optional tool fields accept null; null values are omitted before CLI dispatch.
    return [key, { anyOf: [value, { type: "null" }], default: null }];
  }));
  return { type: "object", properties, required: required[name], additionalProperties: false };
}
const descriptions: Record<string, string> = {
  recall: "Recall memory for a request or topic: observations grouped by page, then source passages, within a token budget.\nPass one or more queries: the request in plain words, short facets, or both. Recall whenever prior knowledge could change your work, and again when the conversation turns to a new topic. Observations already in the standing brief are listed by id only. When the person's circumstances could matter, consider also recalling related facts the request itself does not mention.",
  brief: "Read the standing, page, or incremental memory brief within one budget.",
  open: "Open one memory page, observation, source, or source chunk.\nSource text requires full=true.",
  focus: "Hide a page and its children, or one observation, from this folder's project brief; show reverses it.",
  note: "Add a direct durable observation to existing memory pages.\nDo not use this for tasks or reminders.",
};
function canonical(path: string): string { try { return realpathSync(resolve(path)); } catch { return resolve(path); } }
function message(error: unknown): string { return error instanceof Error ? error.message : String(error); }
