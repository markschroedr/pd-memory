import { Parser } from "expr-eval";
import { sep, resolve } from "node:path";
import { timelineBriefParts } from "./timeline";
import { z } from "zod";
import { EmbeddingClient } from "./embeddings";
import { MemoryStore, type RetrievalObservation, type RetrievalChunk, type ChangeRow } from "./store";
import type { ExistingPageContext, RuntimeConfig } from "./types";

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
export interface BriefArgs { page?: string; folder?: string; for?: string; queries?: string[]; budget?: number; since?: number; profile: ReadProfile; projectSemantic?: boolean }
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
  const budget = args.budget ?? (args.folder ? 8000 : args.profile.budget);
  if (!Number.isInteger(budget) || budget < 200) throw new Error("brief budget must be at least 200 tokens");
  if (args.since !== undefined && (!Number.isInteger(args.since) || args.since < 0)) throw new Error("brief since must be a non-negative integer");
  const seq = store.db.query<{ seq: number }, []>("SELECT coalesce(max(seq),0) seq FROM changes").get()!.seq;
  if (args.since !== undefined) return changedBrief(store, config, args.since, seq, budget, args.profile);
  if (args.folder && !args.page && !args.for) return folderBrief(store, client, config, args, seq, budget);
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
  const tree = args.page ? new Set(store.descendantPageSlugs(args.page)) : null;
  const rows = standingRows(store, config, args.profile)
    .filter((row) => !tree || row.pages.some((page) => tree.has(page)))
    .map((row) => tree ? { ...row, pages: row.pages.filter((page) => tree.has(page)) } : row);
  return args.page
    ? assembledBrief(store, config, rows, seq, budget, `# ${args.page}`, rows.length, args.profile)
    : globalBrief(store, config, rows, seq, budget, args.profile);
}

function standingRows(store: MemoryStore, config: RuntimeConfig, profile: ReadProfile): BriefObservation[] {
  const rootPages = new Set(store.descendantPageSlugs(profile.root));
  return store.retrievalObservations(profile.sensitivityMax).map((row) => ({ ...row,
    rank: evaluateFormula(config, config.callSites.briefStanding, observationVariables(row, config))
      * (row.pages.some((page) => rootPages.has(page)) ? 1.15 : 1),
  }));
}

function globalBrief(store: MemoryStore, config: RuntimeConfig, rows: BriefObservation[], seq: number,
  budget: number, profile: ReadProfile, excluded = new Set<string>()): BriefResult {
  const timeline = timelineBriefParts(store, config, profile.sensitivityMax, excluded);
  const shown = new Set([...excluded, ...timeline.openIds]);
  const stored = storedComposition(store, config, "global");
  const standing = stored ? composedResult(stored, "", seq, shown)
    : assembledBrief(store, config, rows.filter(row => !shown.has(row.id)), seq,
      budget, "", config.brief.perPageCap, profile, shown, undefined, profile.root);
  let directory = "";
  if (stored) {
    const pages = store.readablePages(profile.sensitivityMax);
    const parentBySlug = new Map(pages.map(page => [page.slug, page.parent]));
    const pageRows = pages.map(page => ({ page, observations: rows.filter(row => row.pages.includes(page.slug)),
      score: Math.max(0, ...rows.filter(row => row.pages.includes(page.slug)).map(row => row.rank)) }));
    const covered = pageRows.filter(item => item.observations.some(row => shown.has(row.id)));
    directory = subjectDirectory(config, pageRows, covered, pages, parentBySlug, profile.root,
      Math.floor(budget * config.brief.directoryShare), new BudgetBuilder(budget)).join("");
  }
  const text = ["# Memory", timeline.history, standing.text, directory, timeline.recent].map(part => part.trim()).filter(Boolean).join("\n\n");
  return { text, seq, truncated: standing.truncated || timeline.truncated,
    next: standing.next, tokens: estimateTokens(text) };
}

async function folderBrief(store: MemoryStore, client: EmbeddingClient, config: RuntimeConfig, args: BriefArgs,
  seq: number, budget: number): Promise<BriefResult> {
  const profile = args.profile;
  const folder = resolve(args.folder!);
  store.db.query("INSERT INTO folder_brief_requests(folder,last_requested_at) VALUES (?,?) ON CONFLICT(folder) DO UPDATE SET last_requested_at=excluded.last_requested_at")
    .run(folder, new Date().toISOString());
  const rows = standingRows(store, config, profile);
  const parts = folder.split(sep).filter(Boolean).reverse();
  const { pageScores, hiddenIds, hiddenPages } = folderSelection(store, folder, profile);
  const semantic = new Map<string, number>();
  if (args.projectSemantic === true && parts.length) {
    const matches = await rankSearch(store, client, config, { queries: [parts[0]!],
      layer: "observations", profile });
    for (const hit of matches.filter((item) => item.kind === "observation").slice(0, config.search.defaultLimit))
      semantic.set(hit.id, hit.score);
  }
  const projectRows = projectStandingRows(rows, pageScores, hiddenIds, hiddenPages, semantic);
  const projectShown = new Set<string>();
  const stored = storedComposition(store, config, `folder:${folder}`);
  const project = stored ? composedResult(stored, `# Project: ${parts[0] ?? folder}`, seq, projectShown)
    : projectRows.length ? assembledBrief(store, config, projectRows, seq, Math.floor(budget / 2),
      `# Project: ${parts[0] ?? folder}`, projectRows.length, profile, projectShown, pageScores) : null;
  const global = globalBrief(store, config, rows, seq, Math.ceil(budget / 2), profile, projectShown);
  if (!project) return global;
  return { text: global.text + "\n\n" + project.text, seq, truncated: global.truncated || project.truncated,
    next: [...global.next, ...project.next].slice(0, config.brief.nextCap), tokens: global.tokens + project.tokens };
}

export function folderSelection(store: MemoryStore, folder: string, profile: ReadProfile) {
  const parts = folder.split(sep).filter(Boolean).reverse();
  const pages = store.readablePages(profile.sensitivityMax);
  const parentBySlug = new Map(pages.map((page) => [page.slug, page.parent]));
  const pageScores = new Map<string, number>();
  for (const page of pages) {
    if (page.slug === "root") continue;
    const names = [page.slug, ...page.aliases];
    const score = Math.max(0, ...parts.map((part, depth) =>
      Math.max(...names.map((name) => nameSimilarity(part, name))) * 0.85 ** depth));
    if (score < 0.7) continue;
    for (const slug of store.descendantPageSlugs(page.slug)) {
      let depth = 0;
      for (let parent = slug; parent !== page.slug && parent; parent = parentBySlug.get(parent) ?? "") depth++;
      pageScores.set(slug, Math.max(pageScores.get(slug) ?? 0, score / (depth + 1)));
    }
  }
  const hidden = store.briefExclusions(folder);
  const hiddenPages = new Set<string>();
  for (const id of hidden) if (pages.some((page) => page.slug === id))
    for (const slug of store.descendantPageSlugs(id)) hiddenPages.add(slug);
  for (const slug of hiddenPages) pageScores.delete(slug);
  const hiddenIds = new Set([...hidden].filter((id) => !hiddenPages.has(id)));
  for (const id of store.observationIdsForPages([...hiddenPages])) hiddenIds.add(id);

  return { pageScores, hiddenIds, hiddenPages };
}

function projectStandingRows(rows: BriefObservation[], pageScores: Map<string, number>, hiddenIds: Set<string>,
  hiddenPages: Set<string>, semantic = new Map<string, number>()): BriefObservation[] {
  return rows.filter((row) => !hiddenIds.has(row.id)).flatMap((row) => {
    const pageScore = Math.max(0, ...row.pages.map((page) => pageScores.get(page) ?? 0));
    const searchScore = semantic.get(row.id) ?? 0;
    if (!pageScore && !searchScore) return [];
    return [{ ...row, rank: row.rank * (pageScore ? 1 + pageScore : 0.5 * searchScore),
      pages: pageScore ? row.pages.filter((page) => pageScores.has(page)) : row.pages.filter((page) => !hiddenPages.has(page)).slice(0, 1) }];
  }).filter((row) => row.pages.length);
}

function nameSimilarity(folder: string, name: string): number {
  const a = folder.toLowerCase().replace(/[^\p{L}\p{N}]/gu, "");
  const b = name.toLowerCase().replace(/[^\p{L}\p{N}]/gu, "");
  if (a.length < 3 || b.length < 3) return 0;
  const costs = Array.from({ length: b.length + 1 }, (_, index) => index);
  for (let i = 1; i <= a.length; i++) {
    let previous = costs[0]!;
    costs[0] = i;
    for (let j = 1; j <= b.length; j++) {
      const old = costs[j]!;
      costs[j] = Math.min(costs[j]! + 1, costs[j - 1]! + 1, previous + (a[i - 1] === b[j - 1] ? 0 : 1));
      previous = old;
    }
  }
  let prefix = 0;
  while (prefix < Math.min(a.length, b.length) && a[prefix] === b[prefix]) prefix++;
  return Math.max(1 - costs[b.length]! / Math.max(a.length, b.length),
    prefix >= 5 && prefix / Math.min(a.length, b.length) >= 0.6 ? 0.8 : 0);
}

function assembledBrief(store: MemoryStore, config: RuntimeConfig, rows: BriefObservation[], seq: number, budget: number, title: string,
  perPageCap: number, profile?: ReadProfile, shown?: Set<string>, pageScores?: Map<string, number>, directoryRoot?: string): BriefResult {
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
    const score = evaluateFormula(config, config.callSites.pageRank, { weight, freshness, observations: observations.length, inbound })
      * (pageScores?.get(page.slug) ?? (pageScores ? 0.5 : 1));
    return { page, observations, score };
  }).sort((a, b) => (pageScores ? (pageScores.get(b.page.slug) ?? 0) - (pageScores.get(a.page.slug) ?? 0) : 0)
    || b.score - a.score || a.page.slug.localeCompare(b.page.slug));

  const builder = new BudgetBuilder(budget);
  const included: typeof pageRows = [];
  const headings = new Map<string, string>();
  // A directory of uncovered top-level subjects keeps every substantial subject discoverable.
  // Its unused reserve returns to observations.
  const mainBudget = budget - (directoryRoot ? Math.floor(budget * config.brief.directoryShare) : 0);
  const spineLimit = Math.max(1, Math.floor(mainBudget * config.brief.spineShare));
  let spineTokens = builder.tokensFor(title);
  for (const item of pageRows) {
    const heading = `\n## ${item.page.slug} (${item.page.category})${item.page.line ? ` — ${item.page.line}` : ""} (${item.observations.length})`;
    const headingTokens = builder.tokensFor(heading);
    if (spineTokens + headingTokens > spineLimit && included.length) break;
    if (spineTokens + headingTokens > mainBudget) break;
    included.push(item);
    headings.set(item.page.slug, heading);
    spineTokens += headingTokens;
  }
  const directory = directoryRoot
    ? subjectDirectory(config, pageRows, included, readablePages, parentBySlug, directoryRoot, budget - mainBudget, builder) : [];
  const directoryTokens = directory.reduce((sum, line) => sum + builder.tokensFor(line), 0);
  const rendered = new Set<string>();
  const selected = new Map<string, BriefObservation[]>();
  const spentByPage = new Map<string, number>();
  const affinity = (slug: string) => (pageScores?.get(slug) ?? 1) ** config.brief.projectAffinityExponent;
  const totalAffinity = pageScores ? included.reduce((sum, item) => sum + affinity(item.page.slug), 0) : 0;
  let selectedTokens = spineTokens;
  outer: for (let index = 0; index < perPageCap; index++) {
    for (const item of included) {
      const row = item.observations[index];
      if (!row || rendered.has(row.id)) continue;
      const line = renderObservation(row, config);
      const lineTokens = builder.tokensFor(line);
      if (selectedTokens + lineTokens > budget - directoryTokens) break outer;
      const page = item.page.slug;
      if (pageScores && (spentByPage.get(page) ?? 0) + lineTokens >
        Math.floor((budget - spineTokens) * affinity(page) / totalAffinity)) continue;
      const rows = selected.get(page) ?? [];
      rows.push(row);
      selected.set(page, rows);
      spentByPage.set(page, (spentByPage.get(page) ?? 0) + lineTokens);
      selectedTokens += lineTokens;
      rendered.add(row.id);
      shown?.add(row.id);
    }
  }
  builder.add(title);
  const ordered = pageScores ? included.filter((item) => selected.has(item.page.slug)).map((item) => ({ item, depth: 0 }))
    : hierarchyOrder(included, parentBySlug);
  for (const { item, depth } of ordered) {
    const heading = headings.get(item.page.slug)!;
    builder.add(`\n${"#".repeat(Math.min(6, depth + 2))}${heading.slice(3)}`);
    for (const row of selected.get(item.page.slug) ?? []) builder.add(renderObservation(row, config));
  }
  for (const line of directory) builder.add(line);
  const omitted = rows.filter((row) => !rendered.has(row.id)).sort((a, b) => b.rank - a.rank).map((row) => row.id);
  return { text: builder.text, seq, truncated: included.length < pageRows.length || omitted.length > 0,
    next: omitted.slice(0, config.brief.nextCap), tokens: builder.tokens };
}

function subjectDirectory(config: RuntimeConfig, pageRows: Array<{ page: { slug: string }; observations: BriefObservation[]; score: number }>,
  included: Array<{ page: { slug: string } }>, pages: ExistingPageContext[], parentBySlug: Map<string, string | null>,
  root: string, budget: number, builder: BudgetBuilder): string[] {
  const topOf = (slug: string): string | null => {
    const visited = new Set<string>();
    for (let current: string | null | undefined = slug; current && !visited.has(current); current = parentBySlug.get(current)) {
      visited.add(current);
      if (parentBySlug.get(current) === root) return current;
    }
    return null;
  };
  const covered = new Set(included.map((item) => topOf(item.page.slug)));
  const subjects = new Map<string, { ids: Set<string>; score: number }>();
  for (const item of pageRows) {
    const top = topOf(item.page.slug);
    if (!top || covered.has(top)) continue;
    const subject = subjects.get(top) ?? { ids: new Set<string>(), score: 0 };
    for (const row of item.observations) subject.ids.add(row.id);
    subject.score = Math.max(subject.score, item.score);
    subjects.set(top, subject);
  }
  const lineBySlug = new Map(pages.map((page) => [page.slug, page.line]));
  const lines = ["\n## Other subjects"];
  let tokens = builder.tokensFor(lines[0]!);
  for (const [slug, subject] of [...subjects].filter(([, value]) => value.ids.size >= config.brief.directoryMinObservations)
    .sort((a, b) => b[1].score - a[1].score || a[0].localeCompare(b[0]))) {
    const pageLine = lineBySlug.get(slug);
    const line = `\n- ${slug}${pageLine ? ` — ${pageLine}` : ""} (${subject.ids.size})`;
    const lineTokens = builder.tokensFor(line);
    if (tokens + lineTokens > budget) break;
    lines.push(line);
    tokens += lineTokens;
  }
  return lines.length > 1 ? lines : [];
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

interface StoredComposition { text: string; input_ids: string; seq: number }
function storedComposition(store: MemoryStore, config: RuntimeConfig, scope: string): StoredComposition | null {
  if (config.compose.mode === "off") return null;
  return store.db.query<StoredComposition, [string]>(
    "SELECT text,input_ids,seq FROM composed_briefs WHERE scope=?")
    .get(scope);
}
function composedResult(stored: StoredComposition, title: string, seq: number, shown: Set<string>): BriefResult {
  for (const id of JSON.parse(stored.input_ids) as string[]) shown.add(id);
  const text = [title, stored.text].filter(Boolean).join("\n\n");
  return { text, seq, tokens: estimateTokens(text), next: [], truncated: false };
}

// The composition input excludes every assembled wrapper: timeline, directory, and Recent.
export function currentStateMemory(store: MemoryStore, config: RuntimeConfig, args: BriefArgs): { brief: BriefResult; ids: string[] } {
  const seq = store.db.query<{ seq: number }, []>("SELECT coalesce(max(seq),0) seq FROM changes").get()!.seq;
  let rows = standingRows(store, config, args.profile);
  if (args.folder) {
    const global = store.db.query<{ input_ids: string }, []>("SELECT input_ids FROM composed_briefs WHERE scope='global'").get();
    const covered = new Set<string>(global ? JSON.parse(global.input_ids) : []);
    rows = rows.filter(row => !covered.has(row.id));
  }
  const shown = new Set<string>();
  const budget = args.budget ?? args.profile.budget;
  if (args.folder) {
    const { pageScores, hiddenIds, hiddenPages } = folderSelection(store, resolve(args.folder), args.profile);
    const projectRows = projectStandingRows(rows, pageScores, hiddenIds, hiddenPages);
    const brief = assembledBrief(store, config, projectRows, seq, budget, "", projectRows.length, args.profile, shown, pageScores);
    return { brief, ids: [...shown] };
  }
  const brief = assembledBrief(store, config, rows, seq, budget, "", config.brief.perPageCap, args.profile, shown);
  return { brief, ids: [...shown] };
}

function changedBrief(store: MemoryStore, config: RuntimeConfig, since: number, seq: number, budget: number, profile: ReadProfile): BriefResult {
  const changes = store.db.query("SELECT seq,op,entry FROM changes WHERE seq>? AND entry IS NOT NULL AND op NOT IN ('brief_hide','brief_show') ORDER BY seq").all(since) as EntryChange[];
  const latest = new Map<string, EntryChange>();
  for (const change of changes) latest.set(change.entry, change);
  const entries = [...latest.values()].sort((left, right) => Number(left.seq) - Number(right.seq));
  const builder = new BudgetBuilder(budget); builder.add(`# Memory changed since ${since}`);
  let shown = 0;
  for (const change of entries) {
    let rendered: string | null = null;
    try {
      const value = store.open(change.entry, { sensitivityMax: profile.sensitivityMax });
      if (value.kind === "observation") {
        const date = briefDate(value.happened);
        rendered = `\n- ${value.id}${value.replaced_by ? ` → ${value.replaced_by}` : ""}${value.forgotten_at ? " (forgotten)" : ""} ${value.line}${date ? ` (${date})` : ""}`;
      } else if (value.kind === "page") rendered = `\n- ${value.id}${value.replaced_by ? ` → ${value.replaced_by}` : ""}${value.line ? ` — ${value.line}` : ""}`;
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
  const date = briefDate(row.happened);
  return `\n- ${row.id} [${bucket(row.rank, config)}${marker}] ${row.line} (${row.sources.length}${date ? `; ${date}` : ""})`;
}
export function briefDate(happened: string | null): string {
  if (!happened) return "";
  const [year, month, day] = happened.slice(0, 10).split("-").map(Number);
  const monthName = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"][month - 1];
  return `${day} ${monthName}${year === new Date().getUTCFullYear() ? "" : ` ${year}`}`;
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
    return estimateTokens(value);
  }
  add(value: string): boolean {
    const tokens = this.tokensFor(value);
    if (this.tokens + tokens > this.budget) return false;
    this.text += value; this.tokens += tokens; return true;
  }
}

export function estimateTokens(value: string): number {
  const words = value.trim().match(/\S+/g)?.length ?? 0;
  return words === 0 ? 0 : Math.ceil(words / 0.75);
}
