import { createHash, randomBytes } from "node:crypto";
import type { Extraction, IntegrationProposal, Authority, SourceKind } from "./types";
import { MemoryStore, vectorToBlob, type ClaimedJob, type ObservationRow, type PageRow } from "./store";
import type { ObservationValue } from "./observation";
import type { EditInput, NoteInput } from "./commands";
export type { EditInput, NoteInput } from "./commands";

import type { EmbeddedValue } from "./embeddings";
export type { EmbeddedValue } from "./embeddings";

export class MemoryMutations {
  constructor(private readonly store: MemoryStore) {}

  commitIntegration(job: ClaimedJob, extraction: Extraction, proposal: IntegrationProposal,
    vectors: Map<number, EmbeddedValue>, embeddingModel: string): { observations: number; pages: string[]; seq: number } {
    return this.store.db.transaction(() => {
      const now = new Date().toISOString();
      const pageByKey = new Map(proposal.pages.map((page) => [page.key, page]));
      const evidenceByCandidate = new Map(extraction.observations.map((candidate) => [candidate.candidate_id, candidate.evidence]));
      const usedPageKeys = new Set(proposal.observations.flatMap((observation) => "page_keys" in observation ? observation.page_keys : []));
      const parentSlugs = new Set(proposal.pages.flatMap((page) => page.parent ? [page.parent] : []));
      for (const page of proposal.pages) if (page.action === "create" && !usedPageKeys.has(page.key) && !parentSlugs.has(page.slug)) {
        throw new Error(`Page ${page.slug} is neither used by an observation nor as a parent`);
      }
      for (const page of proposal.pages) for (const from of page.merge_from) {
        this.mergePage(from, page.slug, `Consolidated duplicate pages while integrating src:${job.id}`, "agent");
      }
      const createdPages = this.createProposedPages(job.id, proposal.pages, now);
      const moves = new Map(proposal.pages.filter((page) => page.action === "reuse" && page.parent !== null)
        .map((page) => [page.slug, page.parent!]));
      while (moves.size) {
        let progressed = false;
        for (const [slug, parent] of moves) {
          if (this.store.descendantPageSlugs(slug).includes(parent)) continue;
          if (this.page(slug).parent !== parent) this.reparentPage(slug, parent, `Reorganized while integrating src:${job.id}`, "agent");
          moves.delete(slug); progressed = true;
        }
        if (!progressed) throw new Error("Page moves contain a hierarchy cycle");
      }
      for (const page of proposal.pages) if (page.action === "reuse") {
        const current = this.page(page.slug);
        if (current.replaced_by) throw new Error(`Cannot reuse redirected page ${page.slug}`);
        const aliases = [...new Set([...(JSON.parse(current.aliases_json) as string[]), ...page.aliases])];
        this.assertPageNamesAvailable(page.slug, aliases, page.slug);
        const line = page.line ?? current.line;
        if (line !== current.line || JSON.stringify(aliases) !== current.aliases_json) {
          const inputs = [...new Set([...(JSON.parse(current.line_inputs_json) as string[]), `src:${job.id}`])];
          this.store.db.query("UPDATE pages SET line=?,aliases_json=?,line_inputs_json=?,updated_at=? WHERE slug=?")
            .run(line, JSON.stringify(aliases), JSON.stringify(inputs), now, page.slug);
          this.change(now, "ingest", "edit", page.slug, current, `Updated page while integrating src:${job.id}`);
        }
      }
      let changedObservations = 0;
      for (let index = 0; index < proposal.observations.length; index++) {
        const change = proposal.observations[index]!;
        const evidence = [...new Set(change.candidate_ids.flatMap((id) => {
          const chunks = evidenceByCandidate.get(id);
          if (!chunks) throw new Error(`Missing evidence for candidate ${id}`);
          return chunks;
        }))];
        this.assertEvidenceSource(evidence, job.id);
        if (change.op === "discard") continue;
        const oldRows = change.observation_ids.map((id) => {
          const row = this.store.db.query<ObservationRow, [string]>("SELECT * FROM observations WHERE id=?").get(id);
          if (!row || row.replaced_by) throw new Error(`Observation ${id} is missing or superseded`);
          return row;
        });
        if (change.op === "attach_source") {
          this.attachSource(change.observation_ids[0]!, job.id, now, "ingest");
          this.attachEvidence(change.observation_ids[0]!, evidence);
          continue;
        }
        if (change.candidate_ids.length && change.sensitivity < job.sensitivity) throw new Error("Observation sensitivity is below its source");
        const value = { ...change, sensitivity: Math.max(change.sensitivity, ...oldRows.map((row) => Number(row.sensitivity))) };
        const reason = `${change.op}: ${change.reason} (src:${job.id})`;
        const pages = change.page_keys.map((key) => {
          const page = pageByKey.get(key);
          if (!page) throw new Error(`Unknown page key ${key}`);
          return page.slug;
        });
        const embedded = vectors.get(index);
        if (!embedded) throw new Error(`Missing embedding for proposed observation ${index}`);
        if (change.op === "create") {
          this.createObservation(value, pages, [job.id], evidence, embedded, embeddingModel, now, "ingest", reason);
        } else if (change.op === "update") {
          const id = change.observation_ids[0]!;
          this.updateObservation(id, value, pages, embedded, embeddingModel, now, "ingest", reason, [job.id]);
          this.attachSource(id, job.id, now, "ingest", false);
          this.attachEvidence(id, evidence);
        } else {
          const oldSources = change.observation_ids.flatMap((id) => this.sourceIds(id));
          const oldEvidence = change.observation_ids.flatMap((id) => this.evidenceIds(id));
          if (change.op === "merge") for (const id of change.observation_ids) {
            for (const row of this.store.db.query("SELECT page_slug FROM observation_pages WHERE observation_id=?").all(id) as Array<{ page_slug: string }>) pages.push(row.page_slug);
          }
          const sources = [...oldSources, ...(change.candidate_ids.length ? [job.id] : [])];
          const replacement = this.createObservation(value, pages, sources, [...oldEvidence, ...evidence], embedded,
            embeddingModel, now, "ingest", reason);
          for (const id of change.observation_ids) this.supersedeObservation(id, replacement, now, "ingest", reason);
        }
        changedObservations++;
      }
      const submittedParticipants = JSON.parse(job.participants_json) as string[];
      this.store.db.query("UPDATE sources SET participants_json=?,digest=? WHERE id=?")
        .run(JSON.stringify(submittedParticipants.length ? submittedParticipants : extraction.participants), extraction.source_digest, job.id);
      this.store.db.query("UPDATE jobs SET status='completed',error=NULL,finished_at=? WHERE id=?").run(now, job.job_id);
      return { observations: changedObservations, pages: createdPages, seq: this.currentSeq() };
    })();
  }

  note(input: NoteInput, embedded: EmbeddedValue, chunkEmbedded: EmbeddedValue, embeddingModel: string): { id: string; source: string; seq: number } {
    return this.store.db.transaction(() => {
      const now = new Date().toISOString();
      this.assertPagesActive(input.pages);
      const sourceId = this.createDirectSource(input, now);
      const chunkId = this.createDirectChunk(sourceId, input.line, chunkEmbedded, embeddingModel);
      const id = this.createObservation({
        line: input.line, body: input.body ?? null, happened: input.happened ?? null,
        claimant: input.claimant ?? null, authority: input.actor as Authority, kind: input.kind ?? "fact",
        confidence: input.confidence ?? 1, weight: input.weight ?? 0.8,
        durability: input.durability ?? null, sensitivity: input.sensitivity ?? 0.3,
      }, input.pages, [sourceId], [chunkId], embedded, embeddingModel, now, input.actor, "Direct note");
      return { id, source: `src:${sourceId}`, seq: this.currentSeq() };
    })();
  }

  edit(input: EditInput, embedded: EmbeddedValue | null, embeddingModel: string, actor: "user" | "agent"): { id: string; seq: number } {
    return this.store.db.transaction(() => {
      const now = new Date().toISOString();
      const current = this.store.db.query<ObservationRow, [string]>("SELECT * FROM observations WHERE id=?").get(input.id);
      if (!current) throw new Error(`Unknown observation ${input.id}`);
      if (current.replaced_by) throw new Error(`Observation ${input.id} is already superseded by ${current.replaced_by}`);
      const pages = input.pages ?? (this.store.db.query("SELECT page_slug FROM observation_pages WHERE observation_id=? ORDER BY page_slug").all(input.id) as Array<{ page_slug: string }>).map((row) => row.page_slug);
      this.assertPagesActive(pages);
      const value = { ...current, ...defined(input), body: input.body === undefined ? current.body : input.body,
        claimant: input.claimant === undefined ? current.claimant : input.claimant,
        happened: input.happened === undefined ? current.happened : input.happened,
        durability: input.durability === undefined ? current.durability : input.durability };
      const sourceSensitivity = Number((this.store.db.query(`SELECT coalesce(max(s.sensitivity),0) value FROM sources s
        JOIN observation_sources os ON os.source_id=s.id WHERE os.observation_id=?`).get(input.id) as { value: number }).value);
      if (value.sensitivity < sourceSensitivity) throw new Error(`Sensitivity cannot be below source sensitivity ${sourceSensitivity}`);
      const textChanged = value.line !== current.line || value.body !== current.body;
      if (textChanged && !embedded) throw new Error("An embedding is required when observation text changes");
      const vector = embedded ?? this.existingEmbedding(input.id, embeddingModel);
      if (!vector) throw new Error(`Observation ${input.id} has no ${embeddingModel} embedding`);
      if (input.supersede) {
        const id = this.createObservation(value, pages, this.sourceIds(input.id), this.evidenceIds(input.id), vector,
          embeddingModel, now, actor, input.reason);
        this.supersedeObservation(input.id, id, now, actor, input.reason);
        return { id, seq: this.currentSeq() };
      }
      this.updateObservation(input.id, value, pages, vector, embeddingModel, now, actor, input.reason);
      return { id: input.id, seq: this.currentSeq() };
    })();
  }

  reparentPage(slug: string, parent: string, reason: string, actor: "user" | "agent"): { id: string; seq: number } {
    return this.store.db.transaction(() => {
      if (slug === "root" || slug === parent) throw new Error("Invalid page parent");
      const page = this.page(slug);
      const target = this.page(parent);
      if (page.replaced_by || target.replaced_by) throw new Error("Cannot reparent a redirected page");
      const descendants = this.store.descendantPageSlugs(slug);
      if (descendants.includes(parent)) throw new Error(`Page hierarchy cycle: ${parent} is below ${slug}`);
      const now = new Date().toISOString();
      this.store.db.query("UPDATE pages SET parent=?,updated_at=? WHERE slug=?").run(parent, now, slug);
      this.change(now, actor, "edit", slug, { parent: page.parent }, reason);
      return { id: slug, seq: this.currentSeq() };
    })();
  }

  mergePage(from: string, into: string, reason: string, actor: "user" | "agent"): { id: string; seq: number } {
    return this.store.db.transaction(() => {
      if (from === "root" || from === into) throw new Error("Invalid page merge");
      const source = this.page(from);
      const target = this.page(into);
      if (source.replaced_by) throw new Error(`Page ${from} already redirects to ${source.replaced_by}`);
      if (target.replaced_by) throw new Error(`Target page ${into} redirects to ${target.replaced_by}`);
      if (this.store.descendantPageSlugs(from).includes(into)) throw new Error(`Cannot merge ${from} into its descendant ${into}`);
      const now = new Date().toISOString();
      const before = { ...source, aliases: JSON.parse(source.aliases_json) };
      for (const row of this.store.db.query("SELECT observation_id FROM observation_pages WHERE page_slug=?").all(from) as Array<{ observation_id: string }>) {
        this.store.db.query("INSERT OR IGNORE INTO observation_pages(observation_id,page_slug) VALUES (?,?)").run(row.observation_id, into);
      }
      this.store.db.query("DELETE FROM observation_pages WHERE page_slug=?").run(from);
      this.store.db.query("UPDATE pages SET parent=? WHERE parent=?").run(into, from);
      const aliases = [...new Set([...(JSON.parse(target.aliases_json) as string[]), from, ...(JSON.parse(source.aliases_json) as string[])])];
      const inputs = [...new Set([...(JSON.parse(target.line_inputs_json) as string[]), ...(JSON.parse(source.line_inputs_json) as string[])])];
      this.store.db.query("UPDATE pages SET aliases_json=?,line_inputs_json=?,updated_at=? WHERE slug=?").run(JSON.stringify(aliases), JSON.stringify(inputs), now, into);
      this.store.db.query("UPDATE pages SET replaced_by=?,updated_at=? WHERE slug=?").run(into, now, from);
      this.change(now, actor, "edit", into, target, reason);
      this.change(now, actor, "replace", from, before, reason);
      return { id: into, seq: this.currentSeq() };
    })();
  }

  private createProposedPages(sourceId: number, pages: IntegrationProposal["pages"], now: string): string[] {
    const pending = new Map(pages.filter((page) => page.action === "create").map((page) => [page.slug, page]));
    const created: string[] = [];
    while (pending.size) {
      let progress = false;
      for (const [slug, page] of [...pending]) {
        const parent = page.parent ?? "root";
        if (pending.has(parent)) continue;
        const parentRow = this.page(parent);
        if (parentRow.replaced_by) throw new Error(`Parent page ${parent} redirects to ${parentRow.replaced_by}`);
        this.assertPageNamesAvailable(page.slug, page.aliases);
        this.store.db.query(`INSERT INTO pages
          (slug,category,line,aliases_json,parent,replaced_by,weight,line_inputs_json,provisional,created_at,updated_at)
          VALUES (?,?,?,?,?,NULL,0.5,?,0,?,?)`).run(page.slug, page.category, page.line, JSON.stringify(page.aliases), parent,
            JSON.stringify([`src:${sourceId}`]), now, now);
        this.change(now, "ingest", "create", page.slug, null, `Created while integrating src:${sourceId}`);
        created.push(page.slug);
        pending.delete(slug);
        progress = true;
      }
      if (!progress) throw new Error(`Page hierarchy contains a cycle or unknown parent: ${[...pending.keys()].join(", ")}`);
    }
    return created;
  }

  private createObservation(value: ObservationValue, pages: string[], sourceIds: number[], evidenceIds: string[], embedded: EmbeddedValue,
    embeddingModel: string, now: string, actor: string, reason: string): string {
    this.assertPagesActive(pages);
    const id = this.newObservationId();
    const sensitivity = Math.max(value.sensitivity, this.maxSourceSensitivity(sourceIds));
    this.store.db.query(`INSERT INTO observations
      (id,line,body,happened,entered,claimant,authority,kind,confidence,weight,durability,sensitivity)
      VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`).run(id, value.line, value.body ?? null, value.happened ?? null, now, value.claimant ?? null,
        value.authority, value.kind, value.confidence, value.weight, value.durability ?? null, sensitivity);
    for (const page of [...new Set(pages)]) this.store.db.query("INSERT INTO observation_pages(observation_id,page_slug) VALUES (?,?)").run(id, page);
    for (const sourceId of [...new Set(sourceIds)]) this.store.db.query("INSERT OR IGNORE INTO observation_sources(observation_id,source_id) VALUES (?,?)").run(id, sourceId);
    this.attachEvidence(id, evidenceIds);
    this.store.db.query("INSERT INTO embeddings(entry_id,model,input_hash,vector) VALUES (?,?,?,?)")
      .run(id, embeddingModel, embedded.inputHash, vectorToBlob(embedded.vector));
    this.change(now, actor, "create", id, null, reason);
    return id;
  }

  private updateObservation(id: string, value: ObservationValue, pages: string[], embedded: EmbeddedValue,
    embeddingModel: string, now: string, actor: string, reason: string, additionalSourceIds: number[] = []): void {
    const before = { observation: this.store.db.query("SELECT * FROM observations WHERE id=?").get(id),
      pages: (this.store.db.query("SELECT page_slug FROM observation_pages WHERE observation_id=? ORDER BY page_slug").all(id) as Array<{ page_slug: string }>).map((row) => row.page_slug) };
    const sensitivity = Math.max(value.sensitivity, this.maxSourceSensitivity([...this.sourceIds(id), ...additionalSourceIds]));
    this.store.db.query(`UPDATE observations SET line=?,body=?,happened=?,claimant=?,authority=?,kind=?,confidence=?,weight=?,durability=?,sensitivity=? WHERE id=?`)
      .run(value.line, value.body ?? null, value.happened ?? null, value.claimant ?? null, value.authority, value.kind,
        value.confidence, value.weight, value.durability ?? null, sensitivity, id);
    this.store.db.query("DELETE FROM observation_pages WHERE observation_id=?").run(id);
    for (const page of [...new Set(pages)]) this.store.db.query("INSERT INTO observation_pages(observation_id,page_slug) VALUES (?,?)").run(id, page);
    this.store.db.query("INSERT OR REPLACE INTO embeddings(entry_id,model,input_hash,vector) VALUES (?,?,?,?)")
      .run(id, embeddingModel, embedded.inputHash, vectorToBlob(embedded.vector));
    this.change(now, actor, "edit", id, before, reason);
  }

  private attachSource(observationId: string, sourceId: number, now: string, actor: string, record = true): void {
    const observation = this.store.db.query("SELECT replaced_by,sensitivity FROM observations WHERE id=?").get(observationId) as Pick<ObservationRow, "replaced_by" | "sensitivity"> | null;
    if (!observation) throw new Error(`Unknown observation ${observationId}`);
    if (observation.replaced_by) throw new Error(`Cannot attach a source to superseded observation ${observationId}`);
    const source = this.store.db.query("SELECT sensitivity FROM sources WHERE id=?").get(sourceId) as { sensitivity: number } | null;
    if (!source) throw new Error(`Unknown source src:${sourceId}`);
    const result = this.store.db.query("INSERT OR IGNORE INTO observation_sources(observation_id,source_id) VALUES (?,?)").run(observationId, sourceId);
    if (!result.changes) return;
    if (source.sensitivity > observation.sensitivity) {
      this.store.db.query("UPDATE observations SET sensitivity=? WHERE id=?").run(source.sensitivity, observationId);
    }
    if (record) this.change(now, actor, "edit", observationId, { sensitivity: observation.sensitivity }, `Attached src:${sourceId}`);
  }

  private supersedeObservation(id: string, replacement: string, now: string, actor: string, reason: string): void {
    const before = this.store.db.query("SELECT replaced_by FROM observations WHERE id=?").get(id) as { replaced_by: string | null } | null;
    if (!before) throw new Error(`Unknown observation ${id}`);
    if (before.replaced_by) throw new Error(`Observation ${id} already superseded`);
    this.store.db.query("UPDATE observations SET replaced_by=? WHERE id=?").run(replacement, id);
    this.change(now, actor, "replace", id, before, reason);
  }

  private createDirectSource(input: NoteInput, now: string): number {
    const kind: SourceKind = input.actor === "user" ? "note_user" : "note_agent";
    const text = input.body ? `${input.line}\n\n${input.body}` : input.line;
    const hash = createHash("sha256").update(kind).update("\0").update(text).update("\0").update(now).digest("hex");
    return Number(this.store.db.query(`INSERT INTO sources
      (kind,label,text,hash,session,home,happened,sensitivity,external_id,metadata_json,participants_json,digest,ingested_at)
      VALUES (?,?,?,?,NULL,NULL,?,?,NULL,'{}','[]',?,?)`).run(kind, input.line, text, hash, input.happened ?? null,
        input.sensitivity ?? 0.3, input.line, now).lastInsertRowid);
  }

  private createDirectChunk(sourceId: number, context: string, embedded: EmbeddedValue, embeddingModel: string): string {
    const source = this.store.db.query<{ text: string }, [number]>("SELECT text FROM sources WHERE id=?").get(sourceId)!;
    const id = `src:${sourceId}/1`;
    const endLine = (source.text as string).split("\n").length;
    this.store.db.query(`INSERT INTO chunks(id,source_id,chunk_index,start_line,end_line,context)
      VALUES (?,?,1,1,?,?)`).run(id, sourceId, endLine, context);
    this.store.db.query("INSERT INTO chunks_fts(id,context,text) VALUES (?,?,?)").run(id, context, source.text);
    this.store.db.query("INSERT INTO embeddings(entry_id,model,input_hash,vector) VALUES (?,?,?,?)")
      .run(id, embeddingModel, embedded.inputHash, vectorToBlob(embedded.vector));
    return id;
  }

  private attachEvidence(observationId: string, chunkIds: string[]): void {
    const insert = this.store.db.query("INSERT OR IGNORE INTO observation_evidence(observation_id,chunk_id) VALUES (?,?)");
    for (const chunkId of [...new Set(chunkIds)]) {
      const chunk = this.store.db.query("SELECT source_id FROM chunks WHERE id=?").get(chunkId) as { source_id: number } | null;
      if (!chunk) throw new Error(`Unknown evidence chunk ${chunkId}`);
      this.store.db.query("INSERT OR IGNORE INTO observation_sources(observation_id,source_id) VALUES (?,?)")
        .run(observationId, chunk.source_id);
      insert.run(observationId, chunkId);
    }
    const sensitivity = this.maxSourceSensitivity(this.sourceIds(observationId));
    this.store.db.query("UPDATE observations SET sensitivity=max(sensitivity,?) WHERE id=?").run(sensitivity, observationId);
  }

  private assertEvidenceSource(chunkIds: string[], sourceId: number): void {
    const query = this.store.db.query("SELECT source_id FROM chunks WHERE id=?");
    for (const chunkId of [...new Set(chunkIds)]) {
      const chunk = query.get(chunkId) as { source_id: number } | null;
      if (!chunk || Number(chunk.source_id) !== sourceId) throw new Error(`Evidence ${chunkId} is not from src:${sourceId}`);
    }
  }

  private sourceIds(observationId: string): number[] {
    return (this.store.db.query("SELECT source_id FROM observation_sources WHERE observation_id=? ORDER BY source_id").all(observationId) as Array<{ source_id: number }>)
      .map((row) => Number(row.source_id));
  }

  private evidenceIds(observationId: string): string[] {
    return (this.store.db.query("SELECT chunk_id FROM observation_evidence WHERE observation_id=? ORDER BY chunk_id").all(observationId) as Array<{ chunk_id: string }>)
      .map((row) => row.chunk_id);
  }

  private maxSourceSensitivity(sourceIds: number[]): number {
    let maximum = 0;
    const query = this.store.db.query("SELECT sensitivity FROM sources WHERE id=?");
    for (const sourceId of [...new Set(sourceIds)]) {
      const row = query.get(sourceId) as { sensitivity: number } | null;
      if (!row) throw new Error(`Unknown source src:${sourceId}`);
      maximum = Math.max(maximum, Number(row.sensitivity));
    }
    return maximum;
  }

  private existingEmbedding(id: string, model: string): EmbeddedValue | null {
    const row = this.store.db.query("SELECT input_hash,vector FROM embeddings WHERE entry_id=? AND model=?").get(id, model) as { input_hash: string; vector: Uint8Array } | null;
    return row ? { inputHash: row.input_hash, vector: Array.from(new Float32Array(row.vector.buffer.slice(row.vector.byteOffset, row.vector.byteOffset + row.vector.byteLength))) } : null;
  }

  private assertPagesActive(pages: string[]): void {
    if (!pages.length) throw new Error("An observation needs at least one page");
    for (const slug of [...new Set(pages)]) {
      const page = this.page(slug);
      if (page.replaced_by) throw new Error(`Page ${slug} redirects to ${page.replaced_by}`);
    }
  }

  private page(slug: string): PageRow {
    const page = this.store.db.query<PageRow, [string]>("SELECT * FROM pages WHERE slug=?").get(slug);
    if (!page) throw new Error(`Unknown page ${slug}`);
    return page;
  }

  private assertPageNamesAvailable(slug: string, aliases: string[], ownSlug?: string): void {
    const requested = [...new Set([slug, ...aliases].map(normalizeName))];
    for (const row of this.store.db.query("SELECT slug,aliases_json,replaced_by FROM pages").all() as Array<Pick<PageRow, "slug" | "aliases_json" | "replaced_by">>) {
      let owner = row;
      const seen = new Set<string>();
      while (owner.replaced_by && !seen.has(owner.slug)) { seen.add(owner.slug); owner = this.page(owner.replaced_by); }
      if (owner.slug === ownSlug) continue;
      const existing = [row.slug, ...(JSON.parse(row.aliases_json) as string[])].map(normalizeName);
      if (requested.some((name) => existing.includes(name))) throw new Error(`Page name or alias conflicts with ${row.slug}`);
    }
  }

  private change(at: string, actor: string, op: string, entry: string, before: unknown, reason: string): void {
    this.store.db.query("INSERT INTO changes(at,by_actor,op,entry,before_json,reason) VALUES (?,?,?,?,?,?)")
      .run(at, actor, op, entry, before == null ? null : JSON.stringify(before), reason);
  }

  private currentSeq(): number {
    return Number((this.store.db.query("SELECT max(seq) AS seq FROM changes").get() as { seq: number | null }).seq ?? 0);
  }

  private newObservationId(): string {
    while (true) {
      const id = `${Math.floor(Math.random() * 10)}${randomBytes(4).toString("hex").slice(0, 5)}`;
      if (!this.store.db.query("SELECT 1 FROM observations WHERE id=?").get(id)) return id;
    }
  }
}

function normalizeName(value: string): string {
  return value.normalize("NFKC").toLocaleLowerCase().replace(/[^\p{L}\p{N}]+/gu, " ").trim();
}

function defined(value: EditInput): Partial<ObservationValue> {
  const { id, pages, supersede, reason, ...fields } = value;
  return Object.fromEntries(Object.entries(fields).filter(([, item]) => item !== undefined));
}
