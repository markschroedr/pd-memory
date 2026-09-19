import { Parser } from "expr-eval";
import { z } from "zod";
import { EmbeddingClient } from "./embeddings";
import { MemoryStore, type RetrievalObservation, type RetrievalChunk, type ChangeRow } from "./store";
import type { RuntimeConfig } from "./types";

export interface ReadProfile { root: string; sensitivityMax: number; budget: number }
export type SearchLayer = "observations" | "chunks" | "both";
export interface SearchArgs { queries: string[]; pages?: string[]; limit?: number; layer?: SearchLayer; profile: ReadProfile }
const searchHitSchema = z.strictObject({
  id: z.string(), kind: z.enum(["observation", "chunk"]), bucket: z.number().int(), line: z.string(),
  body: z.boolean().optional(), sources: z.number().int().optional(),
});
export const searchResultSchema = z.strictObject({
  hits: z.array(searchHitSchema), more: z.number().int().nonnegative(), next: z.array(z.string()),
});
export const briefResultSchema = z.strictObject({
  text: z.string(), seq: z.number().int(), truncated: z.boolean(), next: z.array(z.string()), tokens: z.number().int(),
});
export type SearchHit = z.infer<typeof searchHitSchema>;
export type SearchResult = z.infer<typeof searchResultSchema>;
export interface BriefArgs { page?: string; for?: string; queries?: string[]; budget?: number; since?: number; profile: ReadProfile }
export type BriefResult = z.infer<typeof briefResultSchema>;

type RankedHit = { id: string; score: number } & (
  { kind: "observation"; row: RetrievalObservation } | { kind: "chunk"; row: RetrievalChunk }
);
type BriefObservation = RetrievalObservation & { rank: number };
type EntryChange = Pick<ChangeRow, "seq" | "op"> & { entry: string };

const parser = new Parser({ allowMemberAccess: false, operators: { assignment: false, fndef: false, random: false } });

export async function searchMemory(store: MemoryStore, client: EmbeddingClient, config: RuntimeConfig, args: SearchArgs): Promise<SearchResult> {
  const limit = args.limit ?? config.search.defaultLimit;
  if (!Number.isInteger(limit) || limit < 1 || limit > config.search.maxLimit) throw new Error(`search limit must be from 1 to ${config.search.maxLimit}`);
  const ranked = await rankSearch(store, client, config, args);
  const shown = ranked.slice(0, limit);
  return {
    hits: shown.map(({ id, kind, score, row }) => kind === "observation"
      ? { id, kind, bucket: bucket(score, config), line: row.line, body: Boolean(row.body), sources: row.sources.length }
      : { id, kind, bucket: bucket(score, config), line: row.context }),
    more: Math.max(0, ranked.length - shown.length),
    next: shown.map((hit) => hit.id),
  };
}

async function rankSearch(store: MemoryStore, client: EmbeddingClient, config: RuntimeConfig, args: SearchArgs): Promise<RankedHit[]> {
  if (!Array.isArray(args.queries) || !args.queries.length || args.queries.some((query) => !query.trim())) {
    throw new Error("search requires one or more non-empty queries");
  }
  const layer = args.layer ?? "both";
  const embedded = await client.embed(args.queries);
  store.recordModelCall(null, "embed_query", embedded, config);
  const includeObservations = layer !== "chunks";
  const includeChunks = layer !== "observations";
  const vectors = embedded.value.map((item) => item.vector);
  const observationMatches = includeObservations ? store.nearestObservationEmbeddings(vectors, config.embeddings.model, args.profile.sensitivityMax) : [];
  const chunkMatches = includeChunks ? store.nearestChunkEmbeddings(vectors, config.embeddings.model, args.profile.sensitivityMax, args.pages) : [];
  const fused = new Map<string, number>();

  for (let queryIndex = 0; queryIndex < args.queries.length; queryIndex++) {
    const query = args.queries[queryIndex]!;
    if (includeObservations) {
      addRrf(fused, store.lexicalObservationIds(query, args.profile.sensitivityMax), config.search.rrfK);
      const semantic = observationMatches[queryIndex]!.map((item) => item.id);
      addRrf(fused, semantic, config.search.rrfK);
      const namedPages = store.pagesNamedIn(query);
      addUniform(fused, store.observationIdsForPages(namedPages.map((page) => page.slug)), 1 / (config.search.rrfK + 1));
    }
    if (includeChunks) {
      addRrf(fused, store.lexicalChunkIds(query, args.profile.sensitivityMax, args.pages), config.search.rrfK);
      const semantic = chunkMatches[queryIndex]!.map((item) => item.id);
      addRrf(fused, semantic, config.search.rrfK);
    }
  }

  const maxRelevance = Math.max(0, ...fused.values());
  const ranked: RankedHit[] = [];
  if (includeObservations) {
    const rootPages = new Set(store.descendantPageSlugs(args.profile.root));
    const allowedPages = args.pages?.length ? new Set(args.pages) : null;
    for (const row of store.retrievalObservations(args.profile.sensitivityMax, [...fused.keys()])) {
      if (!fused.has(row.id) || (allowedPages && !row.pages.some((page: string) => allowedPages.has(page)))) continue;
      let relevance = maxRelevance ? fused.get(row.id)! / maxRelevance : 0;
      if (row.pages.some((page: string) => rootPages.has(page))) relevance *= 1.05;
      ranked.push({ id: row.id, kind: "observation", row,
        score: evaluateFormula(config, config.callSites.search, observationVariables(row, config, relevance)) });
    }
  }
  if (includeChunks) {
    const chunkIds = [...fused.keys()].filter((id) => /^src:\d+\/\d+$/.test(id));
    for (const row of store.retrievalChunks(chunkIds, args.profile.sensitivityMax, args.pages)) {
      const relevance = maxRelevance ? fused.get(row.id)! / maxRelevance : 0;
      ranked.push({ id: row.id, kind: "chunk", row,
        score: evaluateFormula(config, config.callSites.searchChunk, chunkVariables(row, config, relevance)) });
    }
  }
  return ranked.sort((a, b) => b.score - a.score || b.row.entered.localeCompare(a.row.entered) || a.id.localeCompare(b.id));
}

export async function briefMemory(store: MemoryStore, client: EmbeddingClient, config: RuntimeConfig, args: BriefArgs): Promise<BriefResult> {
  const budget = args.budget ?? args.profile.budget;
  if (!Number.isInteger(budget) || budget < 200) throw new Error("brief budget must be at least 200 tokens");
  if (args.since !== undefined && (!Number.isInteger(args.since) || args.since < 0)) throw new Error("brief since must be a non-negative integer");
  const seq = store.db.query<{ seq: number }, []>("SELECT coalesce(max(seq),0) seq FROM changes").get()!.seq;
  if (args.since !== undefined) return changedBrief(store, config, args.since, seq, budget, args.profile);
  if (args.for) {
    const found = await rankSearch(store, client, config, {
      queries: [args.for, ...(args.page ? [args.page] : []), ...(args.queries ?? [])],
      layer: "observations",
      profile: args.profile,
    });
    const rows = found.filter((hit) => hit.kind === "observation").slice(0, config.search.defaultLimit)
      .map((hit) => ({ ...hit.row, rank: hit.score }));
    return assembledBrief(store, config, rows, seq, budget,
      args.page ? `# ${args.page} — ${args.for}` : `# For: ${args.for}`, rows.length, args.profile);
  }
  const rootPages = new Set(store.descendantPageSlugs(args.profile.root));
  const tree = args.page ? new Set(store.descendantPageSlugs(args.page)) : null;
  const rows = store.retrievalObservations(args.profile.sensitivityMax)
    .filter((row) => !tree || row.pages.some((page) => tree.has(page)))
    .map((row): BriefObservation => ({ ...row,
      rank: evaluateFormula(config, config.callSites.briefStanding, observationVariables(row, config))
        * (row.pages.some((page) => rootPages.has(page)) ? 1.15 : 1),
      pages: tree ? row.pages.filter((page) => tree.has(page)) : row.pages,
    }));
  const recentBudget = args.page ? 0 : Math.min(config.brief.recentBudget, Math.floor(budget / 3));
  const standing = assembledBrief(store, config, rows, seq, budget - recentBudget, args.page ? `# ${args.page}` : "# Memory",
    args.page ? rows.length : config.brief.perPageCap, args.profile);
  if (args.page || recentBudget === 0) return standing;
  const recent = assembledRecent(store, seq, Math.min(recentBudget, budget - standing.tokens), args.profile);
  return {
    text: standing.text + recent.text,
    seq,
    truncated: standing.truncated || recent.truncated,
    next: [...standing.next, ...recent.next].slice(0, config.brief.nextCap),
    tokens: standing.tokens + recent.tokens,
  };
}

function assembledBrief(store: MemoryStore, config: RuntimeConfig, rows: BriefObservation[], seq: number, budget: number, title: string, perPageCap: number, profile?: ReadProfile): BriefResult {
  const pageMap = new Map<string, BriefObservation[]>();
  for (const row of rows) for (const page of row.pages) {
    const list = pageMap.get(page) ?? []; list.push(row); pageMap.set(page, list);
  }
  const readablePages = store.readablePages(profile?.sensitivityMax ?? 1);
  const parentBySlug = new Map(readablePages.map((page) => [page.slug, page.parent]));
  const pageRows = readablePages.filter((page) => pageMap.has(page.slug)).map((page) => {
    const observations = pageMap.get(page.slug)!.sort((a, b) => b.rank - a.rank || b.entered.localeCompare(a.entered));
    const freshness = Math.max(...observations.map((row) => observationVariables(row, config).freshness as number));
    const weight = Math.max(...observations.map((row) => row.weight));
    const inbound = new Set(observations.flatMap((row) => row.pages).filter((slug) => slug !== page.slug)).size;
    const score = evaluateFormula(config, config.callSites.pageRank, { weight, freshness, observations: observations.length, inbound });
    return { page, observations, score };
  }).sort((a, b) => b.score - a.score || a.page.slug.localeCompare(b.page.slug));

  const builder = new BudgetBuilder(budget);
  const included: typeof pageRows = [];
  const headings = new Map<string, string>();
  const spineLimit = Math.max(1, Math.floor(budget * config.brief.spineShare));
  let spineTokens = builder.tokensFor(title);
  for (const item of pageRows) {
    const heading = `\n## ${item.page.slug} (${item.page.category})${item.page.line ? ` — ${item.page.line}` : ""} (${item.observations.length})`;
    const headingTokens = builder.tokensFor(heading);
    if (spineTokens + headingTokens > spineLimit && included.length) break;
    if (spineTokens + headingTokens > budget) break;
    included.push(item);
    headings.set(item.page.slug, heading);
    spineTokens += headingTokens;
  }
  const rendered = new Set<string>();
  const selected = new Map<string, BriefObservation[]>();
  let selectedTokens = spineTokens;
  outer: for (let index = 0; index < perPageCap; index++) {
    for (const item of included) {
      const row = item.observations[index];
      if (!row || rendered.has(row.id)) continue;
      const line = renderObservation(row, config);
      const lineTokens = builder.tokensFor(line);
      if (selectedTokens + lineTokens > budget) break outer;
      const rows = selected.get(item.page.slug) ?? [];
      rows.push(row);
      selected.set(item.page.slug, rows);
      selectedTokens += lineTokens;
      rendered.add(row.id);
    }
  }
  builder.add(title);
  for (const { item, depth } of hierarchyOrder(included, parentBySlug)) {
    const heading = headings.get(item.page.slug)!;
    builder.add(`\n${"#".repeat(Math.min(6, depth + 2))}${heading.slice(3)}`);
    for (const row of selected.get(item.page.slug) ?? []) builder.add(renderObservation(row, config));
  }
  const omitted = rows.filter((row) => !rendered.has(row.id)).sort((a, b) => b.rank - a.rank).map((row) => row.id);
  return { text: builder.text, seq, truncated: included.length < pageRows.length || omitted.length > 0,
    next: omitted.slice(0, config.brief.nextCap), tokens: builder.tokens };
}

function hierarchyOrder<T extends { page: { slug: string } }>(items: T[], parentBySlug: Map<string, string | null>): Array<{ item: T; depth: number }> {
  const included = new Set(items.map((item) => item.page.slug));
  const children = new Map<string | null, T[]>();
  for (const item of items) {
    let parent = parentBySlug.get(item.page.slug) ?? null;
    const visited = new Set<string>();
    while (parent !== null && !included.has(parent) && !visited.has(parent)) {
      visited.add(parent);
      parent = parentBySlug.get(parent) ?? null;
    }
    const list = children.get(parent) ?? [];
    list.push(item);
    children.set(parent, list);
  }
  const ordered: Array<{ item: T; depth: number }> = [];
  const append = (parent: string | null, depth: number) => {
    for (const item of children.get(parent) ?? []) {
      ordered.push({ item, depth });
      append(item.page.slug, depth + 1);
    }
  };
  append(null, 0);
  return ordered;
}

function assembledRecent(store: MemoryStore, seq: number, budget: number, profile: ReadProfile): BriefResult {
  const builder = new BudgetBuilder(budget);
  if (!builder.add("\n\n## Recent")) return { text: "", seq, truncated: true, next: [], tokens: 0 };
  const allowedPages = new Set(store.descendantPageSlugs(profile.root));
  const changes = store.db.query(`SELECT seq,op,entry FROM changes
    WHERE entry IS NOT NULL ORDER BY seq DESC LIMIT 500`).all() as EntryChange[];
  const seen = new Set<string>();
  const candidates: Array<{ id: string; line: string; page: string }> = [];
  for (const change of changes) {
    if (seen.has(change.entry)) continue;
    seen.add(change.entry);
    try {
      const value = store.open(change.entry, { sensitivityMax: profile.sensitivityMax });
      if (value.kind !== "observation") continue;
      let current = value;
      let id = value.id;
      if (value.replaced_by) {
        if (change.op !== "replace") continue;
        seen.add(value.replaced_by);
        const replacement = store.open(value.replaced_by, { sensitivityMax: profile.sensitivityMax });
        if (replacement.kind !== "observation") continue;
        current = replacement;
        id = `${value.id} → ${value.replaced_by}`;
      }
      const page = current.pages.find((slug: string) => allowedPages.has(slug));
      if (page) candidates.push({ id, line: current.line, page });
    } catch { /* Hidden or unavailable entries are absent from this profile. */ }
  }

  const groups = new Map<string, Array<{ id: string; line: string }>>();
  const orderedPages: string[] = [];
  let selectedTokens = builder.tokens;
  let selected = 0;
  for (const candidate of candidates) {
    const headingTokens = groups.has(candidate.page) ? 0 : builder.tokensFor(`\n### ${candidate.page}`);
    const lineTokens = builder.tokensFor(`\n- ${candidate.id} ${candidate.line}`);
    if (selectedTokens + headingTokens + lineTokens > builder.budget) break;
    if (!groups.has(candidate.page)) { groups.set(candidate.page, []); orderedPages.push(candidate.page); }
    groups.get(candidate.page)!.push({ id: candidate.id, line: candidate.line });
    selectedTokens += headingTokens + lineTokens;
    selected++;
  }
  for (const page of orderedPages) {
    builder.add(`\n### ${page}`);
    for (const item of groups.get(page)!) builder.add(`\n- ${item.id} ${item.line}`);
  }
  const omitted = candidates.slice(selected).map((item) => item.id);
  return { text: builder.text, seq, truncated: omitted.length > 0, next: omitted, tokens: builder.tokens };
}

function changedBrief(store: MemoryStore, config: RuntimeConfig, since: number, seq: number, budget: number, profile: ReadProfile): BriefResult {
  const changes = store.db.query("SELECT seq,op,entry FROM changes WHERE seq>? AND entry IS NOT NULL ORDER BY seq").all(since) as EntryChange[];
  const latest = new Map<string, EntryChange>();
  for (const change of changes) latest.set(change.entry, change);
  const entries = [...latest.values()].sort((left, right) => Number(left.seq) - Number(right.seq));
  const builder = new BudgetBuilder(budget); builder.add(`# Memory changed since ${since}`);
  let shown = 0;
  for (const change of entries) {
    let rendered: string | null = null;
    try {
      const value = store.open(change.entry, { sensitivityMax: profile.sensitivityMax });
      if (value.kind === "observation") rendered = `\n- ${value.id}${value.replaced_by ? ` → ${value.replaced_by}` : ""} ${value.line}`;
      else if (value.kind === "page") rendered = `\n- ${value.id}${value.replaced_by ? ` → ${value.replaced_by}` : ""}${value.line ? ` — ${value.line}` : ""}`;
    } catch { /* Hidden or deleted entries are absent from this profile. */ }
    if (!rendered) { shown++; continue; }
    if (!builder.add(rendered)) break;
    shown++;
  }
  const truncated = shown < entries.length;
  const omitted = entries.slice(shown).map((change) => change.entry);
  const cursor = truncated ? (shown > 0 ? Number(entries[shown - 1]!.seq) : since) : seq;
  return { text: builder.text, seq: cursor, truncated,
    next: omitted.slice(0, config.brief.nextCap), tokens: builder.tokens };
}

function renderObservation(row: BriefObservation, config: RuntimeConfig): string {
  const marker = row.body ? "+" : "";
  return `\n- ${row.id} [${bucket(row.rank, config)}${marker}] ${row.line} (${row.sources.length})`;
}
function observationVariables(row: RetrievalObservation, config: RuntimeConfig, relevance = 0): Record<string, number> {
  const freshness = freshnessFor(row.happened ?? row.entered, row.durability);
  const sourcePrior = Math.max(0, ...row.sources.map((source) => config.sourcePriors[source.kind] ?? 0));
  return { weight: row.weight, confidence: row.confidence, freshness, source_prior: sourcePrior,
    sources: row.sources.length, relevance, observations: 0, subpages: 0, inbound: 0 };
}
function chunkVariables(row: RetrievalChunk, config: RuntimeConfig, relevance: number): Record<string, number> {
  return { relevance, weight: row.weight, confidence: row.confidence, sources: row.sources,
    source_prior: config.sourcePriors[row.source_kind] ?? 0,
    freshness: freshnessFor(row.happened ?? row.entered, null), observations: 0, subpages: 0, inbound: 0 };
}
function freshnessFor(happened: string, durability: number | null): number {
  if (durability === null) return 1;
  const ageDays = Math.max(0, (Date.now() - Date.parse(happened)) / 86_400_000);
  return 2 ** (-ageDays / durability);
}
function evaluateFormula(config: RuntimeConfig, name: string, values: Record<string, number>): number {
  const formula = config.formulas[name];
  if (!formula) throw new Error(`Unknown configured formula ${name}`);
  const result = parser.evaluate(formula, values);
  if (typeof result !== "number" || !Number.isFinite(result)) throw new Error(`Formula ${name} did not produce a finite number`);
  return Math.max(0, result);
}
function addRrf(scores: Map<string, number>, ids: string[], k: number): void {
  ids.forEach((id, index) => scores.set(id, (scores.get(id) ?? 0) + 1 / (k + index + 1)));
}
function addUniform(scores: Map<string, number>, ids: string[], value: number): void {
  ids.forEach((id) => scores.set(id, (scores.get(id) ?? 0) + value));
}
function bucket(score: number, config: RuntimeConfig): number {
  return score >= config.buckets.bucket3Min ? 3 : score >= config.buckets.bucket2Min ? 2 : 1;
}

class BudgetBuilder {
  text = ""; tokens = 0;
  constructor(readonly budget: number) {}
  tokensFor(value: string): number {
    const words = value.trim().match(/\S+/g)?.length ?? 0;
    return words === 0 ? 0 : Math.ceil(words / 0.75);
  }
  add(value: string): boolean {
    const tokens = this.tokensFor(value);
    if (this.tokens + tokens > this.budget) return false;
    this.text += value; this.tokens += tokens; return true;
  }
}
