import { z } from "zod";
import { browseMemory, browseSchema, browseResultSchema } from "./browse";
import { consolidateTimeline, openTimelineNode, forgetTimelineNode } from "./timeline";
import { EmbeddingClient } from "./embeddings";
import { MemoryMutations } from "./mutations";
import { composeMemory, refreshCompositions } from "./compose";
import { briefMemory, searchMemory, searchResultSchema as searchResult, briefResultSchema as briefResult } from "./retrieval";
import { MemoryStore, observationEmbeddingText } from "./store";
import { nonEmptyText, observationSchema, score } from "./observation";
import type { RuntimeConfig } from "./types";

export interface CommandContext {
  store: MemoryStore;
  config: RuntimeConfig;
  embeddings: EmbeddingClient;
}

const profile = nonEmptyText.optional().describe("Configured profile. The default profile is used when omitted.");
const pages = z.array(nonEmptyText).min(1);
const actor = z.enum(["user", "agent"]);
const openResult = z.object({ id: z.string(), kind: z.enum(["page", "observation", "source", "chunk", "timeline"]) }).passthrough();
const mutationResult = z.object({ id: z.string(), seq: z.number().int() }).passthrough();

const schemas = {
  browse: browseSchema,
  maintain: z.strictObject({}),
  timeline: z.strictObject({ action: z.literal("consolidate") }),
  search: z.strictObject({
    queries: z.array(nonEmptyText).min(1).describe("Search phrasings or facets supplied by the caller."),
    pages: pages.optional(), limit: z.number().int().positive().optional(),
    layer: z.enum(["observations", "chunks", "both"]).optional().describe("Retrieval layer. Defaults to both."), profile,
  }),
  brief: z.strictObject({
    page: nonEmptyText.optional(), folder: nonEmptyText.optional().describe("Starting folder for the project-focused brief."),
    include_global: z.boolean().optional().describe("Include standing global context in a folder brief. Defaults to true."),
    for: nonEmptyText.optional().describe("Current situation for a relevance-ranked brief."),
    queries: z.array(nonEmptyText).min(1).optional().describe("Additional caller-supplied facets."),
    budget: z.number().int().min(200).optional(), since: z.number().int().nonnegative().optional(),
    compose: z.boolean().optional().describe("Regenerate and store the requested current-state scope with the configured model."), profile,
  }),
  focus: z.strictObject({ action: z.enum(["hide", "show"]), id: nonEmptyText, folder: nonEmptyText, profile }),
  forget: z.strictObject({ id: nonEmptyText, reason: nonEmptyText, actor, profile }),
  open: z.strictObject({
    id: nonEmptyText, history: z.boolean().optional(),
    evidence: z.boolean().optional().describe("Expand timeline citations. Set false for lazy readers; defaults to true."),
    full: z.boolean().optional().describe("Return complete source text. Sources disclose only their digest by default."), profile,
  }),
  note: observationSchema.omit({ authority: true }).partial().extend({
    line: nonEmptyText, pages: pages.describe("Existing active page slugs."), actor,
    sensitivity: score.default(0.3), profile,
  }),
  edit: observationSchema.partial().extend({
    id: nonEmptyText, reason: nonEmptyText, actor, pages: pages.optional(), supersede: z.boolean().optional(),
    merge_into: nonEmptyText.optional().describe("Merge this page into another active page."),
    parent: nonEmptyText.optional().describe("Move this page below another active page."), profile,
  }).superRefine((value, context) => {
    if (value.merge_into && value.parent) context.addIssue({ code: "custom", message: "Choose merge_into or parent, not both" });
    if ((value.merge_into || value.parent) && Object.keys(value).some((key) => !["id", "reason", "actor", "merge_into", "parent", "profile"].includes(key))) {
      context.addIssue({ code: "custom", message: "A page operation cannot include observation fields" });
    }
  }),
};

export type NoteInput = Omit<z.input<typeof schemas.note>, "profile">;
export type EditInput = Omit<z.input<typeof schemas.edit>, "profile" | "actor" | "merge_into" | "parent">;
export type CommandName = keyof typeof schemas;

function defineCommand<S extends z.ZodType, R>(description: string, schema: S, resultSchema: z.ZodType,
  handler: (context: CommandContext, args: z.output<S>) => R) {
  return { description, schema, resultSchema,
    async run(context: CommandContext, input: unknown): Promise<Awaited<R>> {
      const result = await handler(context, schema.parse(input));
      resultSchema.parse(result);
      return result;
    },
  };
}

export const commands = {
  browse: defineCommand("Browse compact subject pages, current page observations, or timeline headlines with pagination. No model calls.",
    schemas.browse, browseResultSchema, (context, args) => browseMemory(context.store, args, resolveProfile(context.config, args.profile))),
  maintain: defineCommand("Build closed timeline periods, then refresh stored current-state compositions.", schemas.maintain,
    z.object({ timeline: z.unknown(), compositions: z.unknown(), latency_ms: z.number(), cost_usd: z.number() }), async (context) => {
      const started = Date.now();
      const before = context.store.db.query<{ cost: number }, []>("SELECT coalesce(sum(cost_usd),0) cost FROM model_calls").get()!.cost;
      const timeline = await consolidateTimeline(context.store, context.config);
      const compositions = await refreshCompositions(context.store, context.config);
      const after = context.store.db.query<{ cost: number }, []>("SELECT coalesce(sum(cost_usd),0) cost FROM model_calls").get()!.cost;
      return { timeline, compositions, latency_ms: Date.now() - started, cost_usd: after - before };
    }),
  timeline: defineCommand("Consolidate missing closed timeline nodes from days to years. Run after import, outside working agent sessions.",
    schemas.timeline, z.object({ built: z.array(z.object({ id: z.string(), kind: z.enum(["day", "week", "month", "year"]) })),
      failed: z.array(z.object({ id: z.string(), error: z.string() })), latency_ms: z.number() }),
    (context) => consolidateTimeline(context.store, context.config)),
  search: defineCommand(
    "Find observations and source chunks with fused lexical and semantic retrieval. Search on your own initiative whenever past recorded knowledge plausibly exists and would change your answer or your work, such as an earlier decision, requirement, investigation, or failure.",
    schemas.search, searchResult, (context, args) => searchMemory(context.store, context.embeddings, context.config,
      { ...args, profile: resolveProfile(context.config, args.profile) })),
  brief: defineCommand("Assemble a standing, page, situational, or incremental brief within one budget.",
    schemas.brief, briefResult, (context, args) => {
      const { compose, ...briefArgs } = args;
      const resolved = { ...briefArgs, profile: resolveProfile(context.config, args.profile) };
      return compose ? composeMemory(context.store, context.embeddings, context.config, resolved)
        : briefMemory(context.store, context.embeddings, context.config, resolved);
    }),
  focus: defineCommand("Hide or show a page or observation in this folder's project brief.",
    schemas.focus, z.object({ id: z.string(), hidden: z.boolean() }), (context, args) => {
      const current = context.store.open(args.id, { sensitivityMax: resolveProfile(context.config, args.profile).sensitivityMax });
      if (current.kind !== "page" && current.kind !== "observation") throw new Error("Focus accepts a page or observation");
      context.store.setBriefExclusion(args.folder, args.id, args.action === "hide");
      return { id: args.id, hidden: args.action === "hide" };
    }),
  forget: defineCommand("Forget a timeline node for rebuilding, or an observation that is wrong, obsolete, or not worth keeping. It leaves current memory; its history, sources, and evidence remain. Use edit instead to correct or replace a claim.",
    schemas.forget, mutationResult, (context, args) => {
      const sensitivityMax = resolveProfile(context.config, args.profile).sensitivityMax;
      if (/^(day|week|month|year):/.test(args.id)) {
        return forgetTimelineNode(context.store, args.id, args.reason, args.actor);
      }
      const current = context.store.open(args.id, { sensitivityMax });
      if (current.kind !== "observation") throw new Error(`${args.id} is not an observation`);
      return new MemoryMutations(context.store).forget(args.id, args.reason, args.actor);
    }),
  open: defineCommand("Open one page, observation, source, source chunk, or timeline node with its cited evidence. Source text requires full=true.",
    schemas.open, openResult, (context, args) => {
      const selected = resolveProfile(context.config, args.profile);
      if (/^(day|week|month|year):/.test(args.id)) return openTimelineNode(context.store, args.id, selected.sensitivityMax, args.evidence);
      return context.store.open(args.id, { history: args.history, full: args.full, sensitivityMax: selected.sensitivityMax });
    }),
  note: defineCommand("Add a direct observation to existing pages with explicit claimant authority.",
    schemas.note, mutationResult, async (context, args) => {
      const selected = resolveProfile(context.config, args.profile);
      for (const page of args.pages) context.store.open(page, { sensitivityMax: selected.sensitivityMax });
      const text = observationEmbeddingText({ line: args.line, body: args.body ?? null });
      const embedded = await context.embeddings.embed([text, `${args.line}\n${text}`]);
      context.store.recordModelCall(null, "embed_note", embedded, context.config);
      const { profile: _profile, ...input } = args;
      return new MemoryMutations(context.store).note(input, embedded.value[0]!, embedded.value[1]!, context.config.embeddings.model);
    }),
  edit: defineCommand("Correct or supersede an observation, or merge a page, while preserving history.",
    schemas.edit, mutationResult, async (context, args) => {
      const mutations = new MemoryMutations(context.store);
      const selected = resolveProfile(context.config, args.profile);
      const current = context.store.open(args.id, { sensitivityMax: selected.sensitivityMax });
      if (args.merge_into) {
        context.store.open(args.merge_into, { sensitivityMax: selected.sensitivityMax });
        return mutations.mergePage(args.id, args.merge_into, args.reason, args.actor);
      }
      if (args.parent) {
        context.store.open(args.parent, { sensitivityMax: selected.sensitivityMax });
        return mutations.reparentPage(args.id, args.parent, args.reason, args.actor);
      }
      if (current.kind !== "observation") throw new Error(`${args.id} is not an observation`);
      let embedded = null;
      if (args.line !== undefined || args.body !== undefined) {
        const text = observationEmbeddingText({ line: args.line ?? current.line, body: args.body === undefined ? current.body : args.body });
        const result = await context.embeddings.embed([text]);
        context.store.recordModelCall(null, "embed_edit", result, context.config);
        embedded = result.value[0]!;
      }
      const { actor, merge_into: _merge, parent: _parent, profile: _profile, ...input } = args;
      return mutations.edit(input, embedded, context.config.embeddings.model, actor);
    }),
};

export type CommandResult<N extends CommandName> = Awaited<ReturnType<(typeof commands)[N]["run"]>>;
export function runCommand<N extends CommandName>(context: CommandContext, name: N, input: unknown): Promise<CommandResult<N>>;
export function runCommand(context: CommandContext, name: string, input: unknown): Promise<unknown>;
export async function runCommand(context: CommandContext, name: string, input: unknown): Promise<unknown> {
  if (!Object.hasOwn(commands, name)) throw new Error(`Unknown command ${name}`);
  return commands[name as CommandName].run(context, input);
}

export function commandCatalog() {
  return (Object.keys(commands) as CommandName[]).map((name) => ({
    name, description: commands[name].description, inputSchema: z.toJSONSchema(commands[name].schema),
    outputSchema: z.toJSONSchema(commands[name].resultSchema),
  }));
}

function resolveProfile(config: RuntimeConfig, name?: string): RuntimeConfig["profiles"][string] {
  const selected = name ?? config.workspace.defaultProfile;
  const value = config.profiles[selected];
  if (!value) throw new Error(`Unknown profile ${selected}`);
  return value;
}
