import { readFile } from "node:fs/promises";
import { resolve } from "node:path";
import { createHash } from "node:crypto";
import { EmbeddingClient } from "./embeddings";
import { OpenAIClient } from "./openai";
import { MemoryStore, observationEmbeddingText, type ClaimedJob } from "./store";
import { z } from "zod";
import { nonEmptyText, observationSchema, score as scoreSchema, happened as happenedSchema } from "./observation";
import { MemoryMutations } from "./mutations";
import type {
  Chunk, RuntimeConfig, SourceInput, SourceKind, SubmitResult,
} from "./types";
import { PAGE_CATEGORIES, SOURCE_KINDS } from "./types";

const chunkingSchema = z.strictObject({ chunks: z.array(z.strictObject({
  start_line: z.number().int().positive(), context: nonEmptyText,
})).min(1) });
const candidatePageSchema = z.strictObject({
  name: nonEmptyText, category: z.enum(PAGE_CATEGORIES), aliases: z.array(nonEmptyText),
});
const candidateSchema = observationSchema.extend({
  candidate_id: nonEmptyText, evidence: z.array(nonEmptyText).min(1),
  pages: z.array(candidatePageSchema).min(1),
});
const extractionSchema = z.strictObject({
  participants: z.array(nonEmptyText), observations: z.array(candidateSchema), source_digest: nonEmptyText,
});
const pageProposalSchema = z.strictObject({
  key: nonEmptyText, action: z.enum(["reuse", "create"]),
  slug: z.string().regex(/^[a-z][a-z0-9-]{1,63}$/), category: z.enum(PAGE_CATEGORIES),
  line: nonEmptyText.nullable(), aliases: z.array(nonEmptyText), parent: nonEmptyText.nullable(),
  merge_from: z.array(nonEmptyText),
});
const operationFields = {
  candidate_ids: z.array(nonEmptyText), observation_ids: z.array(nonEmptyText), reason: nonEmptyText,
};
const retainedProposalSchema = observationSchema.extend({
  ...operationFields, op: z.enum(["create", "update", "merge", "supersede"]),
  page_keys: z.array(nonEmptyText).min(1),
});
// A flat nullable payload keeps the provider contract compatible with strict structured output.
// Local parsing then narrows retained operations to a complete observation payload.
const wireProposalSchema = z.strictObject({
  ...operationFields, op: z.enum(["discard", "create", "attach_source", "update", "merge", "supersede"]),
  line: observationSchema.shape.line.nullable(), body: observationSchema.shape.body,
  happened: observationSchema.shape.happened, claimant: observationSchema.shape.claimant,
  authority: observationSchema.shape.authority.nullable(), kind: observationSchema.shape.kind.nullable(),
  confidence: scoreSchema.nullable(), weight: scoreSchema.nullable(),
  durability: observationSchema.shape.durability, sensitivity: scoreSchema.nullable(),
  page_keys: z.array(nonEmptyText),
});
const integrationSchema = z.strictObject({
  pages: z.array(pageProposalSchema), observations: z.array(wireProposalSchema),
});
export type CandidatePage = z.infer<typeof candidatePageSchema>;
export type CandidateObservation = z.infer<typeof candidateSchema>;
export type Extraction = z.infer<typeof extractionSchema>;
export type PageProposal = z.infer<typeof pageProposalSchema>;
export type ObservationPayload = z.infer<typeof observationSchema> & { page_keys: string[] };
export type ObservationProposal = z.infer<typeof retainedProposalSchema>
  | (Pick<z.infer<typeof wireProposalSchema>, "candidate_ids" | "observation_ids" | "reason"> & ({ op: "discard" } | { op: "attach_source" }));
export interface IntegrationProposal { pages: PageProposal[]; observations: ObservationProposal[] }
const CHUNK_SCHEMA = z.toJSONSchema(chunkingSchema);
export const EXTRACTION_SCHEMA = z.toJSONSchema(extractionSchema);
export const INTEGRATION_SCHEMA = z.toJSONSchema(integrationSchema);

const CHUNK_SYSTEM = `Split one source into passages so search can land on the right passage. Cut at topic changes, not fixed lengths. Each passage must discuss one thing. Return only each passage's first line number and one short context line: who and what is discussed. No narrative position words such as then, afterwards, at the start. Write the context in the source language. The target size is advisory; never split a coherent passage only to meet it.`;

const EXTRACTION_SYSTEM = `You select durable knowledge for one person's future agents. This is long-term memory, not a session handoff or a summary of the work performed. The source remains searchable; observations carry only knowledge worth bringing into future conversations.

Select the smallest useful set of knowledge that should shape understanding or decisions several weeks from now. Merely remaining true is not enough: each observation must warrant a future agent's attention. Preserve consequential intent, decisions, corrections, outcomes, open questions, and reusable lessons, with the reasons and conditions that make them useful. Routine execution and temporary task state belong only in the source, never in observation lines or bodies. Extract nothing when a passage adds no knowledge worth remembering.

Uncertainty: distinguish confirmed choices from proposals. When a choice, commitment, or fact is tentative, conditional, contested, or only proposed, say so in the line and lower confidence. Never record a proposal as settled. WARNING: Attribute a decision to the user only when the user explicitly confirms it; an assistant proposal is never the user's decision without that confirmation. Keep disagreements as separate claims. Prefer the latest explicit correction over superseded wording while retaining the correction's reason when useful.

Lines: one standalone sentence in the source language, naming its subjects explicitly, including the product or project when needed to distinguish similar features. An optional body preserves only lasting reasons, conditions, and dependencies needed to use the knowledge correctly. Evidence citations provide traceability; the prose carries meaning rather than a verification record.

Claimant and authority: claimant is who asserted the claim; authority classifies them as user, agent, third_party, or unknown. Preserve speaker boundaries when paraphrasing: an assistant's elaboration is not the person's belief or decision. The source channel does not set authority.

Kind: fact for a state of the world; question for something explicitly unresolved. Use commitment only when a person explicitly confirms a choice or an action they will take. A tentative real commitment stays tentative. Opinions, suggestions, preferences, evaluations, possibilities, and general discussion of future work are facts or questions, not commitments, unless someone actually commits.

Time: happened is when the claim's event occurred. Use the source date unless the text dates the claim itself. Null only when neither is known.

Scores, 0 to 1: confidence is how firmly the source establishes the claim (0.9 stated as fact by the responsible person, 0.6 plausible or partly agreed, 0.3 speculation or one-sided proposal). weight is how much this shapes the person's future work (0.9 materially changes future decisions, 0.5 useful lasting context, 0.2 peripheral durable detail). sensitivity: 0.0 safe anywhere, 0.3 personal work matters, 0.6 private life or about third parties, 1.0 intimate or secret; never below the source sensitivity. Durability is the relevance half-life in days (1, 7, 30, 365), null for permanent; it is not a score.

Evidence: cite one or more shown chunk ids for every observation. Cite only chunks from this source that directly support the claim.

Pages: Build a deliberately sparse directory of durable subjects that a future agent would intentionally revisit. Prefer the nearest existing broader page when something is a small sub-aspect of it. Create a page when it is likely to become a continuing destination with its own evolving body of observations. Keep incidental names in the observation text, and suggest only the smallest set of pages that are primary subjects of the observation. Categories: actor is who can act or hold responsibility (human, agent, team, company); artifact is something maintained over time that observations keep returning to (a product, a codebase, a contract), never a document, slide, list, or draft mentioned once; place is where something happens (office, website, channel); event is a dated occurrence with its own follow-ups (release, incident), never the meeting or conversation the source itself records; project is an ongoing undertaking with intent; topic is a named navigation subject that is none of the above (pricing, security). Most observations should settle into established projects, actors, artifacts, and topics. An observation about actors and artifacts needs no topic page merely for its subject matter. Classify by the page's primary role: a website is an artifact when observations concern building it and a place when they concern what happens there. Aliases are exact alternate names or acronyms only.

Digest: written last, after the observations. Three to six sentences, standalone, in the source language: what kind of exchange this was, who took part, what was decided or moved, and what stayed open. It is the reader's middle step between the observations and the full text.`;
const INTEGRATION_SYSTEM = `You edit one person's durable memory. Integrate the proposed knowledge into the smallest coherent collection that preserves useful meaning, reasons, and distinctions. Extraction proposes material; it does not determine how many observations or pages survive. Leave one retained observation for repeated knowledge, consolidating existing duplicates as well as new candidates. Keep the conditions, rationale, and operational details of one decision or behavior together in its line and body; retain independently useful claims separately.

Retain only knowledge that should shape understanding or decisions several weeks from now. Merely remaining true does not make a detail worth remembering. The source preserves work history; memory preserves consequential meaning. Discard routine implementation detail, temporary task state, and execution reporting, including within otherwise useful observations.

Each candidate includes ranked matches into the shared existing-observation table. The directory describes existing pages. Matches suggest comparisons, not equivalence. Merge only when the evidence establishes the same subject and claim. Similar behavior in different products is not the same claim; if subject identity is uncertain, keep the claims separate. The shared source table supplies home and checkout context for existing observations. Source location is context, not proof of subject identity. Compare across candidates as well as with existing memory. Use source dates and claim context rather than arrival order to interpret changes. Preserve who established each detail: an agent's elaboration or proposal never becomes the person's belief or decision when claims are combined. WARNING: Attribute a decision to the user only when the user explicitly confirms it; an assistant proposal is never the user's decision without that confirmation. Keep uncertainty and unresolved disagreement visible.

Group candidate_ids by the retained meaning. Choose discard for material not worth retaining; create for independent knowledge; attach_source when one existing observation already contains all useful detail; update to enrich that same claim; merge to consolidate several existing observations without losing their useful distinctions; supersede when new evidence replaces earlier meaning. Merge and supersede produce one replacement and preserve the old records as history. Write a short reason. Every candidate belongs to exactly one operation; each existing observation may be used at most once. Existing observations not referenced stay unchanged. Only merge may have no new candidates. Discard/create have no observation_ids; attach_source/update have one; merge and supersede have one or more. A merge combines at least two candidates or existing observations in total.

Pages are sparse navigation destinations, not tags. Consolidate duplicate pages relevant to this source as part of the same edit. Reuse broader subjects for small aspects; create only a durable destination. On a reused page, aliases add learned alternate names and a non-null line improves its description. merge_from lists existing pages that truly name the same subject as this page. Related subjects remain distinct. Keep stable slugs, preserve alternate names, and normalize names in retained observations when identity is clear. Descriptions and parents should reflect the subject's lasting scope, not merely its role in this source. New pages have a parent slug, or root. On a reused page, parent null preserves its location; a parent slug reparents it. Include pages needed by observations, new parents, or page consolidation. Each page_keys value references a declared page key. An observation may belong to several pages without being duplicated.

Write standalone lines in the source language and use bodies for useful detail. Consolidate a coherent claim and its rationale, not unrelated facts about the same subject. Preserve enduring meaning and qualifications when rewriting observations, while removing transient reporting. Sensitivity cannot fall below any contributing source or observation. Durability is a half-life in days or null for permanent. For discard and attach_source, payload fields are null and page_keys is empty.`;

export async function submitSource(store: MemoryStore, input: SourceInput): Promise<SubmitResult> {
  validateSourceInput(input);
  return store.submit(input);
}

export async function submitManifest(store: MemoryStore, manifestPath: string): Promise<SubmitResult[]> {
  const absolute = resolve(manifestPath);
  const raw = JSON.parse(await readFile(absolute, "utf8"));
  if (!raw || !Array.isArray(raw.sources)) throw new Error("Manifest must contain a sources array");
  const results: SubmitResult[] = [];
  for (const [index, item] of raw.sources.entries()) {
    if (!item || typeof item !== "object") throw new Error(`Invalid manifest source ${index}`);
    const path = requiredString(item.path, `sources[${index}].path`);
    const text = await readFile(resolve(absolute, "..", path), "utf8");
    if (typeof item.sha256 === "string") {
      const actual = createHash("sha256").update(text).digest("hex");
      if (actual !== item.sha256) throw new Error(`Hash mismatch for ${path}`);
    }
    const kind = requiredEnum(item.kind, SOURCE_KINDS, `sources[${index}].kind`) as SourceKind;
    results.push(await submitSource(store, {
      kind, text, label: requiredString(item.title, `sources[${index}].title`),
      happened: optionalDate(item.date, `sources[${index}].date`), sensitivity: 0.3,
      session: null, home: optionalSlug(item.home, `sources[${index}].home`),
      externalId: typeof item.external_id === "string" ? item.external_id : null,
      metadata: {
        listed_time: typeof item.listed_time === "string" ? item.listed_time : null,
        selector: typeof item.selector === "number" ? item.selector : null,
        selector_scope: typeof item.date === "string" ? item.date : null,
      },
      participants: parseSpeakerLabels(text),
    }));
  }
  return results;
}

export async function extractQueued(store: MemoryStore, config: RuntimeConfig, concurrency = 4): Promise<number> {
  if (!Number.isInteger(concurrency) || concurrency < 1) throw new Error("Extraction concurrency must be a positive integer");
  let extracted = 0;
  while (true) {
    const jobs = store.claimExtractionBatch(concurrency);
    if (!jobs.length) return extracted;
    const results = await Promise.allSettled(jobs.map((job) => extractClaimedJob(store, config, job)));
    extracted += results.filter((result) => result.status === "fulfilled").length;
  }
}

export async function processNext(store: MemoryStore, config: RuntimeConfig): Promise<unknown | null> {
  let job = store.claimNextIntegration();
  if (!job) {
    const extractionJob = store.claimExtractionBatch(1)[0];
    if (!extractionJob) return null;
    await extractClaimedJob(store, config, extractionJob);
    job = store.claimNextIntegration();
    if (!job) return null;
  }
  try {
    return await processClaimedJob(store, config, job);
  } catch (error) {
    store.failJob(job.job_id, error);
    console.error(`Job ${job.job_id} failed: ${error instanceof Error ? error.message : String(error)}`);
    return { job_id: job.job_id, status: "failed" };
  }
}

export async function drain(store: MemoryStore, config: RuntimeConfig, untilJobs: number[] = [], extractionConcurrency = 4): Promise<unknown[]> {
  await extractQueued(store, config, extractionConcurrency);
  const results: unknown[] = [];
  while (true) {
    if (untilJobs.length && untilJobs.every((id) => store.getJob(id)?.status === "completed")) return results;
    const result = await processNext(store, config);
    if (!result) return results;
    results.push(result);
  }
}

async function extractClaimedJob(store: MemoryStore, config: RuntimeConfig, job: ClaimedJob): Promise<void> {
  try {
    const client = new OpenAIClient(config);
    const metadata = sourceMetadata(job);
    const chunks = await ensureChunks(store, client, config, job);
    const windows = extractionWindows(chunks, config.chunk.extractWindowTokens);
    const extractions: Extraction[] = [];
    for (const [windowIndex, window] of windows.entries()) {
      const chunkIds = window.map((chunk) => chunk.id);
      const extractionResult = await client.structured({
        name: "pd_memory_extraction", schema: EXTRACTION_SCHEMA,
        system: `${EXTRACTION_SYSTEM} ${identityInstruction(config)}`,
        input: `${JSON.stringify({ ...metadata, window: windowIndex + 1, windows: windows.length })}\n\n<source>\n${renderChunks(window)}\n</source>`,
        validate: (value) => validateExtraction(value, job.sensitivity, chunkIds),
        onAttempt: (result, valid, error) => store.recordModelCall(job.job_id, valid ? "extract" : "extract_invalid", result, config, error?.message ?? null),
      });
      extractions.push(extractionResult.value);
    }
    store.saveExtraction(job.job_id, combineExtractions(extractions));
  } catch (error) {
    store.failJob(job.job_id, error);
    throw error;
  }
}

async function ensureChunks(store: MemoryStore, client: OpenAIClient, config: RuntimeConfig, job: ClaimedJob): Promise<Chunk[]> {
  const existing = store.chunksForSource(job.id);
  if (existing.length) return existing;
  const lineCount = (job.text as string).split("\n").length;
  let starts: Array<{ start_line: number; context: string }>;
  if (estimatedTokens(job.text) <= config.chunk.singleChunkTokens || job.kind === "note_user" || job.kind === "note_agent") {
    starts = [{ start_line: 1, context: job.label }];
  } else {
    const numbered = (job.text as string).split("\n").map((line, index) => `${index + 1}\t${line}`).join("\n");
    const result = await client.structured({
      name: "pd_memory_chunks", schema: CHUNK_SCHEMA,
      system: `${CHUNK_SYSTEM} Aim for about ${config.chunk.targetTokens} estimated tokens per passage.`,
      input: `<source>\n${numbered}\n</source>`,
      validate: (value) => validateChunking(value, lineCount),
      onAttempt: (attempt, valid, error) => store.recordModelCall(job.job_id, valid ? "chunk" : "chunk_invalid", attempt, config, error?.message ?? null),
    });
    starts = result.value;
  }
  return store.replaceChunks(job.id, starts);
}

async function ensureChunkEmbeddings(store: MemoryStore, config: RuntimeConfig, sourceId: number, jobId: number): Promise<void> {
  const inputs = store.missingChunkEmbeddingInputs(sourceId, config.embeddings.model);
  if (!inputs.length) return;
  const client = new EmbeddingClient(config);
  const result = await client.embed(inputs.map((input) => input.text));
  store.recordModelCall(jobId, "embed_chunks", result, config);
  store.storeChunkEmbeddings(config.embeddings.model, inputs.map((input, index) => ({ id: input.id, ...result.value[index]! })));
}

function validateChunking(value: unknown, lineCount: number): Array<{ start_line: number; context: string }> {
  const root = chunkingSchema.parse(value);
  const starts = root.chunks.map((item, index) => {
    if (!Number.isInteger(item.start_line) || item.start_line < 1 || item.start_line > lineCount) {
      throw new Error(`chunks[${index}].start_line is invalid`);
    }
    return { start_line: item.start_line as number, context: requiredString(item.context, `chunks[${index}].context`) };
  });
  if (!starts.length || starts[0]!.start_line !== 1) throw new Error("The first chunk must start at line 1");
  for (let index = 1; index < starts.length; index++) {
    if (starts[index]!.start_line <= starts[index - 1]!.start_line) throw new Error("Chunk starts must strictly increase");
  }
  return starts;
}

function extractionWindows(chunks: Chunk[], limit: number): Chunk[][] {
  const windows: Chunk[][] = [];
  let current: Chunk[] = [];
  let tokens = 0;
  for (const chunk of chunks) {
    const size = estimatedTokens(`[${chunk.id}] ${chunk.text}`);
    if (current.length && tokens + size > limit) { windows.push(current); current = []; tokens = 0; }
    current.push(chunk); tokens += size;
  }
  if (current.length) windows.push(current);
  return windows;
}

function renderChunks(chunks: Chunk[]): string {
  return chunks.map((chunk) => `[${chunk.id}] ${chunk.text}`).join("\n\n");
}

function combineExtractions(extractions: Extraction[]): Extraction {
  if (extractions.length === 1) return extractions[0]!;
  const participants = [...new Set(extractions.flatMap((extraction) => extraction.participants))];
  const observations = extractions.flatMap((extraction, windowIndex) => extraction.observations.map((observation) => ({
    ...observation, candidate_id: `w${windowIndex + 1}-${observation.candidate_id}`,
  })));
  return { participants, observations, source_digest: extractions.map((extraction) => extraction.source_digest).join("\n") };
}

function estimatedTokens(text: string): number {
  const words = text.trim().match(/\S+/g)?.length ?? 0;
  return words === 0 ? 0 : Math.ceil(words / 0.75);
}

async function processClaimedJob(store: MemoryStore, config: RuntimeConfig, job: ClaimedJob): Promise<unknown> {
  const client = new OpenAIClient(config);
  const embeddingClient = new EmbeddingClient(config);
  const metadata = sourceMetadata(job);
  const extraction = validateExtraction(JSON.parse(requiredString(job.extraction_json, "job.extraction_json")), job.sensitivity,
    store.chunksForSource(job.id).map((chunk) => chunk.id));
  await ensureChunkEmbeddings(store, config, job.id, job.job_id);
  if (extraction.observations.length === 0) {
    const committed = new MemoryMutations(store).commitIntegration(job, extraction,
      { pages: [], observations: [] }, new Map(), config.embeddings.model);
    return { job: job.job_id, source: `src:${job.id}`, context_supplied: {
      relevant_pages: [], directory_pages: 0, observations: [],
    }, ...committed };
  }
  const candidateTexts = extraction.observations.map(observationEmbeddingText);
  const candidateEmbeddingResult = await embeddingClient.embed(candidateTexts);
  store.recordModelCall(job.job_id, "embed_candidates", candidateEmbeddingResult, config);
  const context = retrieveContext(store, extraction, candidateEmbeddingResult.value.map((x) => x.vector), config);
  const integrationResult = await client.structured({
    name: "pd_memory_integration", schema: INTEGRATION_SCHEMA,
    system: `${INTEGRATION_SYSTEM} ${identityInstruction(config)}`,
    input: JSON.stringify({ source: { ...metadata, digest: extraction.source_digest },
      candidates: extraction.observations.map((candidate, index) => ({ ...candidate, matches: context.candidate_matches[index]!.matches })),
      existing: { directory: context.directory, observations: context.observations, sources: context.sources } }),
    validate: (value) => validateIntegration(value, extraction, context.directory, context.observations.map((x) => x.id), job.sensitivity),
    onAttempt: (result, valid, error) => store.recordModelCall(job.job_id, valid ? "integrate" : "integrate_invalid", result, config, error?.message ?? null),
  });
  const proposal = integrationResult.value;
  const changedTexts = proposal.observations.map((change) => change.op === "attach_source" || change.op === "discard" ? null : observationEmbeddingText(change));
  const changedEmbeddingResult = await embeddingClient.embed(changedTexts.filter((x): x is string => x !== null));
  store.recordModelCall(job.job_id, "embed_changes", changedEmbeddingResult, config);
  const vectors = new Map<number, { vector: number[]; inputHash: string }>();
  let vectorIndex = 0;
  changedTexts.forEach((value, index) => { if (value !== null) vectors.set(index, changedEmbeddingResult.value[vectorIndex++]!); });
  let committed;
  try {
    committed = new MemoryMutations(store).commitIntegration(job, extraction, proposal, vectors, config.embeddings.model);
  } catch (error) {
    store.recordModelCallError(job.job_id, integrationResult.requestId, error);
    throw error;
  }
  return { job: job.job_id, source: `src:${job.id}`, context_supplied: {
    relevant_pages: context.pages.map((x) => x.slug), directory_pages: context.directory.length,
    observations: context.observations.map((x) => x.id),
  }, ...committed };
}

function retrieveContext(store: MemoryStore, extraction: Extraction, vectors: number[][], config: RuntimeConfig) {
  const names = extraction.observations.flatMap((observation) => observation.pages.flatMap((page) => [page.name, ...page.aliases]));
  const exactPages = store.exactPages(names);
  const matches = store.nearestObservationEmbeddings(vectors, config.embeddings.model);
  const candidate_matches = matches.map((semantic, index) => {
    const candidate = extraction.observations[index]!;
    const scores = new Map(semantic.map((item) => [item.id, item.score]));
    const fused = new Map<string, number>();
    const rrf = (ids: string[]) => ids.forEach((id, rank) => fused.set(id, (fused.get(id) ?? 0) + 1 / (config.search.rrfK + rank + 1)));
    rrf(semantic.slice(0, 100).map((item) => item.id));
    rrf(store.lexicalObservationIds(observationEmbeddingText(candidate)));
    return { candidate_id: candidate.candidate_id,
      matches: [...fused.entries()].sort((a, b) => b[1] - a[1]).slice(0, config.ingest.matchesPerCandidate)
        .map(([id]) => ({ id, similarity: scores.get(id) ?? null })) };
  });
  const ids = [...new Set(candidate_matches.flatMap((candidate) => candidate.matches.map((match) => match.id)))];
  const observations = store.observationContexts(ids);
  const referencedSlugs = new Set(observations.flatMap((observation) => observation.pages));
  const relatedPages = store.exactPages([...referencedSlugs]);
  const pages = [...new Map([...exactPages, ...relatedPages].map((page) => [page.slug, page])).values()];
  const sources = store.sourceContexts(observations.flatMap((observation) => observation.sources));
  return { pages, directory: store.allPages(), observations, sources, candidate_matches };
}

function sourceMetadata(job: ClaimedJob): Record<string, unknown> {
  return {
    source: `src:${job.id}`, kind: job.kind, label: job.label, home: job.home, happened: job.happened,
    sensitivity: job.sensitivity, external_id: job.external_id, metadata: JSON.parse(job.metadata_json),
  };
}

export function validateExtraction(value: unknown, sourceSensitivity = 0, sourceChunkIds?: string[]): Extraction {
  const root = extractionSchema.parse(value);
  const participants = root.participants;
  const observationsRaw = root.observations;
  const ids = new Set<string>();
  const observations = observationsRaw.map((raw, index): CandidateObservation => {
    const item = raw;
    const candidate_id = item.candidate_id;
    if (ids.has(candidate_id)) throw new Error(`Duplicate candidate_id ${candidate_id}`);
    ids.add(candidate_id);
    const evidence = stringArray(item.evidence, `${candidate_id}.evidence`).map((id) => id.replace(/^\[|\]$/g, ""));
    if (!evidence.length) throw new Error(`Observation ${candidate_id} needs evidence`);
    const foreign = sourceChunkIds ? evidence.filter((id) => !sourceChunkIds.includes(id)) : [];
    if (foreign.length) {
      throw new Error(`Observation ${candidate_id} cites ${foreign.join(", ")}; valid ids are ${sourceChunkIds![0]} to ${sourceChunkIds![sourceChunkIds!.length - 1]}`);
    }
    return { ...item, evidence: [...new Set(evidence)] };
  });
  if (observations.some((x) => x.pages.length === 0)) throw new Error("Every observation needs at least one suggested page");
  const lowSensitivity = observations.find((x) => x.sensitivity < sourceSensitivity);
  if (lowSensitivity) throw new Error(`Observation ${lowSensitivity.candidate_id} sensitivity is below its source`);
  return { participants, observations, source_digest: requiredString(root.source_digest, "source_digest") };
}

export function validateIntegration(value: unknown, extraction: Extraction, existingPages: Array<{ slug: string; aliases?: string[]; parent?: string | null }>, existingObservationIds: string[], sourceSensitivity = 0): IntegrationProposal {
  const root = integrationSchema.parse(value);
  const existingPageSlugs = existingPages.map((page) => page.slug);
  const pageKeys = new Set<string>();
  const slugs = new Set<string>();
  let pages = root.pages.map((page, index) => {
    const key = requiredString(page.key, `pages[${index}].key`);
    const action = requiredEnum(page.action, ["reuse", "create"] as const, `${key}.action`) as "reuse" | "create";
    const slug = requiredString(page.slug, `${key}.slug`);
    if (!/^[a-z][a-z0-9-]{1,63}$/.test(slug)) throw new Error(`Invalid page slug ${slug}`);
    if (pageKeys.has(key) || slugs.has(slug)) throw new Error(`Duplicate page key or slug: ${key}/${slug}`);
    const aliases = stringArray(page.aliases, `${key}.aliases`);
    const merge_from = stringArray(page.merge_from, `${key}.merge_from`);
    if (action === "create" && merge_from.length) throw new Error("Page merges must target a reused page");
    if (action === "reuse" && !existingPageSlugs.includes(slug)) throw new Error(`Cannot reuse unknown page ${slug}`);
    if (action === "create" && existingPageSlugs.includes(slug)) throw new Error(`Page ${slug} already exists; reuse it`);
    pageKeys.add(key); slugs.add(slug);
    const parent = nullableText(page.parent, `${key}.parent`);
    if (slug === "root" && parent !== null) throw new Error("Cannot reparent root");
    return { ...page, key, action, slug, aliases, parent, merge_from };
  });
  const redirects = new Map<string, string>();
  for (const page of pages) for (const from of page.merge_from) {
    if (from === "root" || !existingPageSlugs.includes(from) || slugs.has(from) || redirects.has(from)) {
      throw new Error(`Invalid, overlapping, or chained page merge: ${from} into ${page.slug}`);
    }
    redirects.set(from, page.slug);
  }
  const finalNames = new Map<string, string>();
  for (const page of [...existingPages, ...pages]) for (const name of [page.slug, ...(page.aliases ?? [])]) {
    const owner = redirects.get(page.slug) ?? page.slug;
    const normalized = normalizeName(name);
    const previous = finalNames.get(normalized);
    if (previous && previous !== owner) throw new Error(`Page name ${name} conflicts with ${previous}; reuse or merge that page`);
    finalNames.set(normalized, owner);
  }
  const parents = new Map<string, string | null>(existingPages.filter((page) => !redirects.has(page.slug)).map((page) =>
    [page.slug, page.parent ? redirects.get(page.parent) ?? page.parent : null]));
  for (const page of pages) if (page.action === "create" || page.parent !== null) parents.set(page.slug, page.parent ?? "root");
  for (const [slug, parent] of parents) {
    if (parent !== null && !parents.has(parent)) throw new Error(`Invalid parent ${parent} for ${slug}`);
    const visited = new Set([slug]);
    let current: string | null | undefined = parent;
    while (current != null) {
      if (visited.has(current)) throw new Error(`Page hierarchy cycle at ${slug}`);
      visited.add(current); current = parents.get(current);
    }
  }
  const expectedCandidates = new Set(extraction.observations.map((x) => x.candidate_id));
  const seenCandidates = new Set<string>();
  const seenObservations = new Set<string>();
  const observations = root.observations.map((item, index): ObservationProposal => {
    const candidate_id = `observations[${index}]`;
    const op = requiredEnum(item.op, ["discard", "create", "attach_source", "update", "merge", "supersede"] as const, `${candidate_id}.op`);
    const candidate_ids = stringArray(item.candidate_ids, `${candidate_id}.candidate_ids`);
    const observation_ids = stringArray(item.observation_ids, `${candidate_id}.observation_ids`);
    const reason = requiredString(item.reason, `${candidate_id}.reason`);
    if (!candidate_ids.length && op !== "merge") throw new Error(`${op} needs candidates`);
    for (const id of candidate_ids) {
      if (!expectedCandidates.has(id) || seenCandidates.has(id)) throw new Error(`Unknown or repeated candidate ${id}`);
      seenCandidates.add(id);
    }
    for (const id of observation_ids) {
      if (!existingObservationIds.includes(id) || seenObservations.has(id)) throw new Error(`Unknown or repeated observation ${id}`);
      seenObservations.add(id);
    }
    const count = observation_ids.length;
    if (((op === "discard" || op === "create") && count !== 0)
      || ((op === "attach_source" || op === "update") && count !== 1)
      || (op === "merge" && (count < 1 || count + candidate_ids.length < 2)) || (op === "supersede" && count < 1)) {
      throw new Error(`Invalid observation_ids count for ${op}`);
    }
    if (op === "discard" || op === "attach_source") {
      if (item.line !== null || item.body !== null || stringArray(item.page_keys, `${candidate_id}.page_keys`).length ||
        item.happened !== null || item.claimant !== null || item.authority !== null || item.kind !== null ||
        item.confidence !== null || item.weight !== null || item.durability !== null || item.sensitivity !== null) {
        throw new Error(`${op} has non-empty unused fields for ${candidate_id}`);
      }
      return { op, candidate_ids, observation_ids, reason };
    }
    const keys = stringArray(item.page_keys, `${candidate_id}.page_keys`);
    if (!keys.length || keys.some((key) => !pageKeys.has(key))) throw new Error(`Invalid page_keys for ${candidate_id}`);
    const payload = retainedProposalSchema.parse(item);
    if (candidate_ids.length && payload.sensitivity < sourceSensitivity) throw new Error(`Observation ${candidate_id} sensitivity is below its source`);
    return payload;
  });
  if (seenCandidates.size !== expectedCandidates.size) throw new Error("Every extracted candidate must be integrated exactly once");
  const usedPageKeys = new Set(observations.flatMap((observation) => "page_keys" in observation ? observation.page_keys : []));
  const parentSlugs = new Set(pages.flatMap((page) => page.parent ? [page.parent] : []));
  pages = pages.filter((page) => page.action === "reuse" || usedPageKeys.has(page.key) || parentSlugs.has(page.slug));
  const emptyLine = pages.find((page) => page.line !== null && normalizeName(page.line) === normalizeName(page.slug));
  if (emptyLine) throw new Error(`Page ${emptyLine.slug} line repeats its name; use null or add information`);
  return { pages, observations };
}

export function parseSpeakerLabels(text: string): string[] {
  const names = new Set<string>();
  for (const line of text.split(/\r?\n/)) {
    const match = /^([^:\n]{1,80}):\s/.exec(line);
    if (match) names.add(match[1]!.trim());
  }
  return [...names];
}

function identityInstruction(config: RuntimeConfig): string {
  return `Authority identity: user claimants=${JSON.stringify(config.identity.userNames)}; known third-party claimants=${JSON.stringify(config.identity.thirdPartyNames)}.`;
}

function normalizeName(value: string): string {
  return value.normalize("NFKC").toLocaleLowerCase().replace(/[^\p{L}\p{N}]+/gu, " ").trim();
}

function validateSourceInput(input: SourceInput): void {
  requiredEnum(input.kind, SOURCE_KINDS, "kind"); requiredString(input.text, "text"); requiredString(input.label, "label");
  optionalSlug(input.home, "home"); optionalDate(input.happened, "happened"); range(input.sensitivity, "sensitivity");
}
function array(value: unknown, name: string): unknown[] { if (!Array.isArray(value)) throw new Error(`${name} must be an array`); return value; }
function stringArray(value: unknown, name: string): string[] {
  return array(value, name).map((item, index) => requiredString(item, `${name}[${index}]`));
}
function requiredString(value: unknown, name: string): string {
  if (typeof value !== "string" || !value.trim()) throw new Error(`${name} must be a non-empty string`); return value.trim();
}
function nullableText(value: unknown, name: string): string | null {
  if (value === null) return null; return requiredString(value, name);
}
function requiredEnum<T extends string>(value: unknown, options: readonly T[], name: string): T {
  if (typeof value !== "string" || !options.includes(value as T)) throw new Error(`${name} must be one of ${options.join(", ")}`); return value as T;
}
function range(value: unknown, name: string): number {
  if (typeof value !== "number" || !Number.isFinite(value) || value < 0 || value > 1) throw new Error(`${name} must be from 0 to 1`); return value;
}
function optionalSlug(value: unknown, name: string): string | null {
  if (value === null || value === undefined) return null;
  if (typeof value !== "string" || !/^[a-z][a-z0-9-]{1,63}$/.test(value)) throw new Error(`${name} must be a page slug`);
  return value;
}
function optionalDate(value: unknown, name: string): string | null {
  if (value === null || value === undefined) return null;
  return happenedSchema.parse(value);
}
