import { Database } from "bun:sqlite";
import { mkdirSync } from "node:fs";
import { dirname } from "node:path";
import { createHash } from "node:crypto";
import type {
  ExistingObservationContext, ExistingPageContext, Extraction, ModelResult, RuntimeConfig, SourceInput, SubmitResult,
  SourceKind, PageCategory, Chunk,
} from "./types";

import type { ObservationValue } from "./observation";
import { nearestVectors } from "./vector-search";

export interface SourceRow {
  id: number; kind: SourceKind; label: string; text: string; hash: string;
  session: string | null; home: string | null; happened: string | null; sensitivity: number;
  external_id: string | null; metadata_json: string; participants_json: string;
  digest: string | null; ingested_at: string;
}
export interface JobRow {
  id: number; source_id: number; status: "queued" | "running" | "completed" | "failed";
  attempts: number; extraction_attempts: number; integration_attempts: number;
  error: string | null; extraction_json: string | null; extraction_finished_at: string | null;
  created_at: string; started_at: string | null; finished_at: string | null;
}
export type ClaimedJob = SourceRow & Pick<JobRow, "attempts" | "extraction_json"> & { job_id: number };
export interface PageRow {
  slug: string; category: PageCategory; line: string | null; aliases_json: string;
  parent: string | null; replaced_by: string | null; weight: number; line_inputs_json: string;
  provisional: number; created_at: string; updated_at: string;
}
export interface ObservationRow extends ObservationValue {
  id: string; entered: string; replaced_by: string | null;
}
interface ChunkRow {
  id: string; source_id: number; chunk_index: number; start_line: number; end_line: number; context: string;
}
interface EmbeddingRow { entry_id: string; vector: Uint8Array }
export interface ChangeRow {
  seq: number; at: string; by_actor: string; op: string; entry: string | null;
  before_json: string | null; reason: string | null;
}
export interface RetrievalObservation extends ObservationRow {
  pages: string[]; sources: Array<{ id: string; kind: SourceKind }>;
}
export interface RetrievalChunk {
  id: string; context: string; source_kind: SourceKind; happened: string | null;
  entered: string; sensitivity: number; weight: number; confidence: number; sources: number;
}
export type OpenEntry = ReturnType<MemoryStore["open"]>;

export class MemoryStore {
  readonly db: Database;

  constructor(path: string) {
    mkdirSync(dirname(path), { recursive: true });
    this.db = new Database(path, { create: true });
    this.db.exec("PRAGMA foreign_keys=ON; PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;");
    this.migrate();
  }

  close(): void { this.db.close(); }

  capturedSessionEntryIds(session: string): Set<string> {
    const rows = this.db.query("SELECT metadata_json FROM sources WHERE session=?").all(session) as Array<{ metadata_json: string }>;
    const ids = new Set<string>();
    for (const row of rows) {
      const metadata = JSON.parse(row.metadata_json) as { entry_ids?: unknown };
      if (!Array.isArray(metadata.entry_ids)) continue;
      for (const id of metadata.entry_ids) if (typeof id === "string" && id) ids.add(id);
    }
    return ids;
  }

  private migrate(): void {
    this.db.exec(`
      CREATE TABLE IF NOT EXISTS sources (
        id INTEGER PRIMARY KEY, kind TEXT NOT NULL, label TEXT NOT NULL, text TEXT NOT NULL,
        hash TEXT NOT NULL UNIQUE, session TEXT, home TEXT, happened TEXT, sensitivity REAL NOT NULL,
        external_id TEXT, metadata_json TEXT NOT NULL, participants_json TEXT NOT NULL DEFAULT '[]',
        digest TEXT, ingested_at TEXT NOT NULL
      );
      CREATE TABLE IF NOT EXISTS jobs (
        id INTEGER PRIMARY KEY, source_id INTEGER NOT NULL UNIQUE REFERENCES sources(id),
        status TEXT NOT NULL CHECK(status IN ('queued','running','completed','failed')),
        attempts INTEGER NOT NULL DEFAULT 0, extraction_attempts INTEGER NOT NULL DEFAULT 0,
        integration_attempts INTEGER NOT NULL DEFAULT 0, error TEXT, extraction_json TEXT, extraction_finished_at TEXT,
        created_at TEXT NOT NULL, started_at TEXT, finished_at TEXT
      );
      CREATE TABLE IF NOT EXISTS pages (
        slug TEXT PRIMARY KEY, category TEXT NOT NULL, line TEXT, aliases_json TEXT NOT NULL,
        parent TEXT REFERENCES pages(slug), replaced_by TEXT REFERENCES pages(slug),
        weight REAL NOT NULL DEFAULT 0.5, line_inputs_json TEXT NOT NULL DEFAULT '[]',
        provisional INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, updated_at TEXT NOT NULL
      );
      CREATE TABLE IF NOT EXISTS observations (
        id TEXT PRIMARY KEY, line TEXT NOT NULL, body TEXT, happened TEXT, entered TEXT NOT NULL,
        claimant TEXT, authority TEXT NOT NULL, kind TEXT NOT NULL, confidence REAL NOT NULL,
        weight REAL NOT NULL, durability REAL, sensitivity REAL NOT NULL,
        replaced_by TEXT REFERENCES observations(id)
      );
      CREATE TABLE IF NOT EXISTS observation_pages (
        observation_id TEXT NOT NULL REFERENCES observations(id), page_slug TEXT NOT NULL REFERENCES pages(slug),
        PRIMARY KEY(observation_id,page_slug)
      );
      CREATE TABLE IF NOT EXISTS chunks (
        id TEXT PRIMARY KEY, source_id INTEGER NOT NULL REFERENCES sources(id), chunk_index INTEGER NOT NULL,
        start_line INTEGER NOT NULL, end_line INTEGER NOT NULL, context TEXT NOT NULL,
        UNIQUE(source_id,chunk_index)
      );
      CREATE TABLE IF NOT EXISTS observation_sources (
        observation_id TEXT NOT NULL REFERENCES observations(id), source_id INTEGER NOT NULL REFERENCES sources(id),
        PRIMARY KEY(observation_id,source_id)
      );
      CREATE TABLE IF NOT EXISTS observation_evidence (
        observation_id TEXT NOT NULL REFERENCES observations(id), chunk_id TEXT NOT NULL REFERENCES chunks(id),
        PRIMARY KEY(observation_id,chunk_id)
      );
      CREATE TABLE IF NOT EXISTS embeddings (
        entry_id TEXT NOT NULL, model TEXT NOT NULL,
        input_hash TEXT NOT NULL, vector BLOB NOT NULL, PRIMARY KEY(entry_id,model)
      );
      CREATE TABLE IF NOT EXISTS changes (
        seq INTEGER PRIMARY KEY AUTOINCREMENT, at TEXT NOT NULL, by_actor TEXT NOT NULL,
        op TEXT NOT NULL, entry TEXT, before_json TEXT, reason TEXT
      );
      CREATE TABLE IF NOT EXISTS model_calls (
        id INTEGER PRIMARY KEY, job_id INTEGER REFERENCES jobs(id), phase TEXT NOT NULL,
        request_id TEXT, model TEXT NOT NULL, service_tier TEXT, usage_json TEXT NOT NULL,
        cost_usd REAL NOT NULL, repaired INTEGER NOT NULL, error TEXT, created_at TEXT NOT NULL
      );
      CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status,id);
      CREATE INDEX IF NOT EXISTS idx_observation_pages_page ON observation_pages(page_slug);
      CREATE INDEX IF NOT EXISTS idx_chunks_source ON chunks(source_id,chunk_index);
      CREATE INDEX IF NOT EXISTS idx_observation_evidence_chunk ON observation_evidence(chunk_id);
      CREATE VIRTUAL TABLE IF NOT EXISTS observations_fts USING fts5(id UNINDEXED,line,body);
      CREATE VIRTUAL TABLE IF NOT EXISTS chunks_fts USING fts5(id UNINDEXED,context,text);
      CREATE TRIGGER IF NOT EXISTS observations_fts_insert AFTER INSERT ON observations BEGIN
        INSERT INTO observations_fts(id,line,body) VALUES (new.id,new.line,coalesce(new.body,''));
      END;
      CREATE TRIGGER IF NOT EXISTS observations_fts_delete AFTER DELETE ON observations BEGIN
        DELETE FROM observations_fts WHERE id=old.id;
      END;
      CREATE TRIGGER IF NOT EXISTS observations_fts_update AFTER UPDATE OF line,body ON observations BEGIN
        DELETE FROM observations_fts WHERE id=old.id;
        INSERT INTO observations_fts(id,line,body) VALUES (new.id,new.line,coalesce(new.body,''));
      END;
    `);
    const missingFts = Number((this.db.query(`SELECT count(*) count FROM observations o
      WHERE NOT EXISTS (SELECT 1 FROM observations_fts f WHERE f.id=o.id)`).get() as { count: number }).count);
    if (missingFts) this.db.exec(`INSERT INTO observations_fts(id,line,body)
      SELECT o.id,o.line,coalesce(o.body,'') FROM observations o
      WHERE NOT EXISTS (SELECT 1 FROM observations_fts f WHERE f.id=o.id)`);
    const jobColumns = new Set((this.db.query("PRAGMA table_info(jobs)").all() as Array<{ name: string }>).map((row) => row.name));
    if (!jobColumns.has("extraction_json")) this.db.exec("ALTER TABLE jobs ADD COLUMN extraction_json TEXT");
    if (!jobColumns.has("extraction_finished_at")) this.db.exec("ALTER TABLE jobs ADD COLUMN extraction_finished_at TEXT");
    if (!jobColumns.has("extraction_attempts")) this.db.exec("ALTER TABLE jobs ADD COLUMN extraction_attempts INTEGER NOT NULL DEFAULT 0");
    if (!jobColumns.has("integration_attempts")) this.db.exec("ALTER TABLE jobs ADD COLUMN integration_attempts INTEGER NOT NULL DEFAULT 0");
    const pageColumns = new Set((this.db.query("PRAGMA table_info(pages)").all() as Array<{ name: string }>).map((row) => row.name));
    if (!pageColumns.has("parent")) this.db.exec("ALTER TABLE pages ADD COLUMN parent TEXT REFERENCES pages(slug)");
    if (!pageColumns.has("replaced_by")) this.db.exec("ALTER TABLE pages ADD COLUMN replaced_by TEXT REFERENCES pages(slug)");
    if (!pageColumns.has("weight")) this.db.exec("ALTER TABLE pages ADD COLUMN weight REAL NOT NULL DEFAULT 0.5");
    if (!pageColumns.has("line_inputs_json")) this.db.exec("ALTER TABLE pages ADD COLUMN line_inputs_json TEXT NOT NULL DEFAULT '[]'");
    if (!this.db.query("SELECT 1 FROM pages WHERE slug='root'").get()) {
      const now = new Date().toISOString();
      this.db.query(`INSERT INTO pages
        (slug,category,line,aliases_json,parent,replaced_by,weight,line_inputs_json,provisional,created_at,updated_at)
        VALUES ('root','topic','Memory root','[]',NULL,NULL,1,'[]',0,?,?)`).run(now, now);
    }
    if (this.db.query("SELECT 1 FROM pages WHERE slug <> 'root' AND parent IS NULL LIMIT 1").get()) {
      this.db.query("UPDATE pages SET parent='root' WHERE slug <> 'root' AND parent IS NULL").run();
    }
  }

  submit(input: SourceInput): SubmitResult {
    const hash = createHash("sha256").update(input.kind).update("\0").update(input.text).digest("hex");
    const existing = this.db.query<SourceRow, [string]>("SELECT * FROM sources WHERE hash=?").get(hash);
    if (existing) {
      const differs = existing.kind !== input.kind || existing.home !== input.home || existing.happened !== input.happened ||
        existing.sensitivity !== input.sensitivity || existing.external_id !== input.externalId;
      const job = this.db.query<Pick<JobRow, "id" | "status">, [number]>("SELECT id,status FROM jobs WHERE source_id=?").get(existing.id)!;
      return {
        source: `src:${existing.id}`, job: job.id, reused: !differs,
        ...(differs ? { conflict: "Identical content was submitted with different kind, happened, sensitivity, or external id" } : {}),
        status: job.status,
      };
    }
    const now = new Date().toISOString();
    const tx = this.db.transaction(() => {
      const inserted = this.db.query(`INSERT INTO sources
        (kind,label,text,hash,session,home,happened,sensitivity,external_id,metadata_json,participants_json,ingested_at)
        VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`).run(
          input.kind, input.label, input.text, hash, input.session, input.home, input.happened, input.sensitivity,
          input.externalId, JSON.stringify(input.metadata), JSON.stringify(input.participants), now,
        );
      const sourceId = Number(inserted.lastInsertRowid);
      const job = this.db.query("INSERT INTO jobs(source_id,status,created_at) VALUES (?,'queued',?)")
        .run(sourceId, now);
      this.db.query("INSERT INTO changes(at,by_actor,op,entry,reason) VALUES (?,'ingest','ingest',?,?)")
        .run(now, `src:${sourceId}`, "Source submitted");
      return { sourceId, jobId: Number(job.lastInsertRowid) };
    })();
    return { source: `src:${tx.sourceId}`, job: tx.jobId, reused: false, status: "queued" };
  }

  recoverInterrupted(before?: string): number {
    const result = before === undefined
      ? this.db.query("UPDATE jobs SET status='queued',error='Worker interrupted before commit' WHERE status='running'").run()
      : this.db.query("UPDATE jobs SET status='queued',error='Worker interrupted before commit' WHERE status='running' AND started_at<=?").run(before);
    return result.changes;
  }

  retryRecoverableFailures(maxAttempts = 3): number {
    if (!Number.isSafeInteger(maxAttempts) || maxAttempts < 1) throw new Error("Retry attempt limit must be positive");
    return this.db.query(`UPDATE jobs SET status='queued',error=NULL,started_at=NULL,finished_at=NULL
      WHERE status='failed' AND extraction_attempts + integration_attempts < ?`).run(maxAttempts).changes;
  }

  claimExtractionBatch(limit: number): ClaimedJob[] {
    return this.db.transaction(() => {
      const jobs = this.db.query("SELECT id FROM jobs WHERE status='queued' AND extraction_json IS NULL ORDER BY id LIMIT ?")
        .all(limit) as Array<{ id: number }>;
      const now = new Date().toISOString();
      for (const job of jobs) {
        this.db.query("UPDATE jobs SET status='running',extraction_attempts=extraction_attempts+1,error=NULL,started_at=?,finished_at=NULL WHERE id=?")
          .run(now, job.id);
      }
      const query = this.db.query<ClaimedJob, [number]>(`SELECT j.id AS job_id,j.attempts,j.extraction_json,s.*
        FROM jobs j JOIN sources s ON s.id=j.source_id WHERE j.id=?`);
      return jobs.map((job) => query.get(job.id)!);
    })();
  }

  saveExtraction(jobId: number, extraction: Extraction): void {
    this.db.query("UPDATE jobs SET status='queued',error=NULL,extraction_json=?,extraction_finished_at=? WHERE id=?")
      .run(JSON.stringify(extraction), new Date().toISOString(), jobId);
  }

  claimNextIntegration(): ClaimedJob | null {
    return this.db.transaction(() => {
      const job = this.db.query(`SELECT j.id FROM jobs j JOIN sources s ON s.id=j.source_id
        WHERE j.status='queued' AND j.extraction_json IS NOT NULL
          AND NOT EXISTS (
            SELECT 1 FROM jobs earlier JOIN sources es ON es.id=earlier.source_id
            WHERE earlier.status IN ('queued','running') AND (
              coalesce(es.happened,es.ingested_at) < coalesce(s.happened,s.ingested_at)
              OR (coalesce(es.happened,es.ingested_at) = coalesce(s.happened,s.ingested_at) AND es.id < s.id)
            )
          )
        ORDER BY coalesce(s.happened,s.ingested_at),s.id LIMIT 1`).get() as { id: number } | null;
      if (!job) return null;
      this.db.query("UPDATE jobs SET status='running',attempts=attempts+1,integration_attempts=integration_attempts+1,error=NULL,started_at=?,finished_at=NULL WHERE id=?").run(new Date().toISOString(), job.id);
      return this.db.query<ClaimedJob, [number]>(`SELECT j.id AS job_id,j.attempts,j.extraction_json,s.*
        FROM jobs j JOIN sources s ON s.id=j.source_id WHERE j.id=?`).get(job.id);
    })();
  }

  getJob(id: number): JobRow | null { return this.db.query<JobRow, [number]>("SELECT * FROM jobs WHERE id=?").get(id); }
  listJobs() {
    return this.db.query(`SELECT j.id,'src:'||j.source_id AS source,j.status,j.attempts,j.extraction_attempts,j.integration_attempts,
      j.error,j.created_at,j.started_at,j.finished_at,s.label,s.happened,j.extraction_finished_at
      FROM jobs j JOIN sources s ON s.id=j.source_id ORDER BY j.id`).all();
  }
  retryFailed(id?: number): number {
    const result = id === undefined
      ? this.db.query("UPDATE jobs SET status='queued',error=NULL,started_at=NULL,finished_at=NULL WHERE status='failed'").run()
      : this.db.query("UPDATE jobs SET status='queued',error=NULL,started_at=NULL,finished_at=NULL WHERE status='failed' AND id=?").run(id);
    return result.changes;
  }
  failJob(id: number, error: unknown): void {
    const message = error instanceof Error ? error.message : String(error);
    this.db.query("UPDATE jobs SET status='failed',error=?,finished_at=? WHERE id=?")
      .run(message.slice(0, 4000), new Date().toISOString(), id);
  }

  recordModelCall(jobId: number | null, phase: string, result: ModelResult<unknown>, config: RuntimeConfig, error: string | null = null): void {
    const input = Number(result.usage.input_tokens ?? 0);
    const output = Number(result.usage.output_tokens ?? 0);
    const cached = Number(result.usage.input_tokens_details?.cached_tokens ?? 0);
    const cost = phase.startsWith("embed")
      ? input * config.pricing.embeddingPerMillion / 1_000_000
      : ((input - cached) * config.pricing.inputPerMillion + cached * config.pricing.cachedInputPerMillion +
        output * config.pricing.outputPerMillion) / 1_000_000;
    this.db.query(`INSERT INTO model_calls
      (job_id,phase,request_id,model,service_tier,usage_json,cost_usd,repaired,error,created_at)
      VALUES (?,?,?,?,?,?,?,?,?,?)`).run(jobId, phase, result.requestId, result.model, result.serviceTier,
        JSON.stringify(result.usage), cost, result.repaired ? 1 : 0, error?.slice(0, 4000) ?? null, new Date().toISOString());
  }

  recordModelCallError(jobId: number, requestId: string | null, error: unknown): void {
    const message = error instanceof Error ? error.message : String(error);
    const row = requestId
      ? this.db.query("SELECT id FROM model_calls WHERE job_id=? AND request_id=? ORDER BY id DESC LIMIT 1").get(jobId, requestId) as { id: number } | null
      : this.db.query("SELECT id FROM model_calls WHERE job_id=? ORDER BY id DESC LIMIT 1").get(jobId) as { id: number } | null;
    if (row) this.db.query("UPDATE model_calls SET error=? WHERE id=?").run(message.slice(0, 4000), row.id);
  }

  allPages(): ExistingPageContext[] {
    return this.db.query<PageRow, []>("SELECT * FROM pages WHERE replaced_by IS NULL ORDER BY slug").all().map(pageContext);
  }

  readablePages(sensitivityMax: number): ExistingPageContext[] {
    const rows = this.db.query<PageRow & { line_sensitivity: number }, []>(`WITH source_sensitivity AS (
        SELECT p.slug,max(s.sensitivity) AS sensitivity FROM pages p,json_each(p.line_inputs_json) input
        JOIN sources s ON input.value='src:'||s.id GROUP BY p.slug
      ), observation_sensitivity AS (
        SELECT op.page_slug,max(o.sensitivity) AS sensitivity FROM observation_pages op
        JOIN observations o ON o.id=op.observation_id GROUP BY op.page_slug
      ) SELECT p.*,coalesce(CASE WHEN json_array_length(p.line_inputs_json)=0 THEN os.sensitivity
        ELSE ss.sensitivity END,0) AS line_sensitivity FROM pages p
      LEFT JOIN source_sensitivity ss ON ss.slug=p.slug
      LEFT JOIN observation_sensitivity os ON os.page_slug=p.slug
      WHERE p.replaced_by IS NULL ORDER BY p.slug`).all();
    return rows.map((row) => ({ ...pageContext(row), line: row.line_sensitivity <= sensitivityMax ? row.line : null }));
  }

  descendantPageSlugs(root: string): string[] {
    const exists = this.db.query("SELECT 1 FROM pages WHERE slug=? AND replaced_by IS NULL").get(root);
    if (!exists) throw new Error(`Unknown page ${root}`);
    return (this.db.query(`WITH RECURSIVE tree(slug) AS (
      SELECT slug FROM pages WHERE slug=? AND replaced_by IS NULL
      UNION ALL SELECT p.slug FROM pages p JOIN tree t ON p.parent=t.slug WHERE p.replaced_by IS NULL
    ) SELECT slug FROM tree`).all(root) as Array<{ slug: string }>).map((row) => row.slug);
  }

  exactPages(names: string[]): ExistingPageContext[] {
    if (!names.length) return [];
    const wanted = new Set(names.map(normalizeName));
    return this.db.query<PageRow, []>("SELECT * FROM pages").all().filter((row) => {
      const aliases = JSON.parse(row.aliases_json) as string[];
      return wanted.has(normalizeName(row.slug)) || aliases.some((a) => wanted.has(normalizeName(a)));
    }).map(pageContext);
  }

  pagesNamedIn(text: string): ExistingPageContext[] {
    const normalized = ` ${normalizeName(text)} `;
    return this.db.query<PageRow, []>("SELECT * FROM pages").all().filter((row) => {
      const names = [row.slug.replaceAll("-", " "), ...(JSON.parse(row.aliases_json) as string[])];
      return names.some((name) => {
        const candidate = normalizeName(name);
        return candidate.length >= 3 && normalized.includes(` ${candidate} `);
      });
    }).map(pageContext);
  }

  chunksForSource(sourceId: number): Chunk[] {
    const source = this.db.query<{ text: string }, [number]>("SELECT text FROM sources WHERE id=?").get(sourceId);
    if (!source) throw new Error(`Unknown source src:${sourceId}`);
    const lines = splitSourceLines(source.text);
    return this.db.query<ChunkRow, [number]>("SELECT * FROM chunks WHERE source_id=? ORDER BY chunk_index").all(sourceId)
      .map((row) => ({ id: row.id, source: `src:${sourceId}`, index: row.chunk_index,
        start_line: row.start_line, end_line: row.end_line, context: row.context,
        text: lines.slice(row.start_line - 1, row.end_line).join("\n") }));
  }

  replaceChunks(sourceId: number, starts: Array<{ start_line: number; context: string }>): Chunk[] {
    return this.db.transaction(() => {
      const source = this.db.query<{ text: string }, [number]>("SELECT text FROM sources WHERE id=?").get(sourceId);
      if (!source) throw new Error(`Unknown source src:${sourceId}`);
      const existing = this.db.query<{ id: string }, [number]>("SELECT id FROM chunks WHERE source_id=?").all(sourceId);
      if (existing.length) {
        const cited = this.db.query(`SELECT 1 FROM observation_evidence oe JOIN chunks c ON c.id=oe.chunk_id
          WHERE c.source_id=? LIMIT 1`).get(sourceId);
        if (cited) throw new Error(`Cannot re-chunk cited source src:${sourceId}`);
        for (const row of existing) {
          this.db.query("DELETE FROM embeddings WHERE entry_id=?").run(row.id);
          this.db.query("DELETE FROM chunks_fts WHERE id=?").run(row.id);
        }
        this.db.query("DELETE FROM chunks WHERE source_id=?").run(sourceId);
      }
      const lines = splitSourceLines(source.text);
      validateChunkStarts(starts, lines.length);
      const insert = this.db.query(`INSERT INTO chunks(id,source_id,chunk_index,start_line,end_line,context)
        VALUES (?,?,?,?,?,?)`);
      const insertFts = this.db.query("INSERT INTO chunks_fts(id,context,text) VALUES (?,?,?)");
      starts.forEach((start, index) => {
        const chunkIndex = index + 1;
        const endLine = index + 1 < starts.length ? starts[index + 1]!.start_line - 1 : lines.length;
        const id = `src:${sourceId}/${chunkIndex}`;
        const text = lines.slice(start.start_line - 1, endLine).join("\n");
        insert.run(id, sourceId, chunkIndex, start.start_line, endLine, start.context);
        insertFts.run(id, start.context, text);
      });
      return this.chunksForSource(sourceId);
    })();
  }

  chunkEmbeddingInputs(sourceId?: number): Array<{ id: string; text: string }> {
    const sources = sourceId === undefined
      ? this.db.query<{ id: number }, []>("SELECT id FROM sources ORDER BY id").all()
      : [{ id: sourceId }];
    return sources.flatMap((source) => this.chunksForSource(source.id)
      .map((chunk) => ({ id: chunk.id, text: `${chunk.context}\n${chunk.text}` })));
  }

  missingChunkEmbeddingInputs(sourceId: number, model: string): Array<{ id: string; text: string }> {
    return this.chunkEmbeddingInputs(sourceId)
      .filter((input) => !this.db.query("SELECT 1 FROM embeddings WHERE entry_id=? AND model=?").get(input.id, model));
  }

  storeChunkEmbeddings(model: string, values: Array<{ id: string; vector: number[]; inputHash: string }>): void {
    this.db.transaction(() => {
      const insert = this.db.query("INSERT OR REPLACE INTO embeddings(entry_id,model,input_hash,vector) VALUES (?,?,?,?)");
      for (const value of values) insert.run(value.id, model, value.inputHash, vectorToBlob(value.vector));
    })();
  }

  nearestChunkEmbeddings(queries: number[][], model: string, sensitivityMax = 1, pages?: string[]) {
    const pageClause = chunkPageClause(pages, sensitivityMax);
    const rows = this.db.query<EmbeddingRow, Array<string | number>>(`SELECT e.entry_id,e.vector FROM embeddings e
      JOIN chunks c ON c.id=e.entry_id JOIN sources s ON s.id=c.source_id
      WHERE e.model=? AND s.sensitivity<=?${pageClause.sql} ORDER BY c.source_id,c.chunk_index`)
      .iterate(model, sensitivityMax, ...pageClause.values);
    return nearestVectors(embeddingRows(rows), queries, 100);
  }

  lexicalChunkIds(query: string, sensitivityMax = 1, pages?: string[]): string[] {
    const terms = query.normalize("NFKC").match(/[\p{L}\p{N}]+/gu) ?? [];
    if (!terms.length) return [];
    const match = [...new Set(terms.map((term) => `"${term.replaceAll('"', '""')}"`))].join(" OR ");
    const pageClause = chunkPageClause(pages, sensitivityMax);
    const rows = this.db.query(`SELECT f.id FROM chunks_fts f JOIN chunks c ON c.id=f.id JOIN sources s ON s.id=c.source_id
      WHERE chunks_fts MATCH ? AND s.sensitivity<=?${pageClause.sql} ORDER BY bm25(chunks_fts) LIMIT 100`)
      .all(match, sensitivityMax, ...pageClause.values) as Array<{ id: string }>;
    return rows.map((row) => row.id);
  }

  retrievalChunks(ids: string[], sensitivityMax = 1, pages?: string[]): RetrievalChunk[] {
    if (!ids.length) return [];
    const pageClause = chunkPageClause(pages, sensitivityMax);
    return this.db.query<RetrievalChunk, Array<string | number>>(`SELECT c.id,c.context,s.kind AS source_kind,s.happened,
      s.ingested_at AS entered,s.sensitivity,coalesce(max(o.weight),0.3) AS weight,
      coalesce(max(o.confidence),0.5) AS confidence,count(DISTINCT o.id) AS sources
      FROM chunks c JOIN sources s ON s.id=c.source_id
      LEFT JOIN observation_evidence oe ON oe.chunk_id=c.id
      LEFT JOIN observations o ON o.id=oe.observation_id AND o.replaced_by IS NULL AND o.sensitivity<=?
      WHERE c.id IN (SELECT value FROM json_each(?)) AND s.sensitivity<=?${pageClause.sql}
      GROUP BY c.id`).all(sensitivityMax, JSON.stringify(ids), sensitivityMax, ...pageClause.values);
  }

  observationEmbeddingInputs(): Array<{ id: string; text: string }> {
    return this.db.query<Pick<ObservationRow, "id" | "line" | "body">, []>("SELECT id,line,body FROM observations WHERE replaced_by IS NULL ORDER BY id").all()
      .map((row) => ({ id: row.id, text: observationEmbeddingText(row) }));
  }

  storeObservationEmbeddings(model: string, values: Array<{ id: string; vector: number[]; inputHash: string }>): void {
    this.db.transaction(() => {
      const insert = this.db.query("INSERT OR REPLACE INTO embeddings(entry_id,model,input_hash,vector) VALUES (?,?,?,?)");
      for (const value of values) insert.run(value.id, model, value.inputHash, vectorToBlob(value.vector));
    })();
  }

  nearestObservationEmbeddings(queries: number[][], model: string, sensitivityMax = 1) {
    const rows = this.db.query<EmbeddingRow, [string, number]>(`SELECT e.entry_id,e.vector FROM embeddings e
      JOIN observations o ON o.id=e.entry_id WHERE e.model=? AND o.replaced_by IS NULL AND o.sensitivity<=?`)
      .iterate(model, sensitivityMax);
    return nearestVectors(embeddingRows(rows), queries, 100);
  }

  observationIdsForPages(slugs: string[]): string[] {
    const ids = new Set<string>();
    const query = this.db.query(`SELECT o.id FROM observations o JOIN observation_pages p ON p.observation_id=o.id
      WHERE p.page_slug=? AND o.replaced_by IS NULL ORDER BY o.entered DESC`);
    for (const slug of slugs) for (const row of query.all(slug) as Array<{ id: string }>) ids.add(row.id);
    return [...ids];
  }

  lexicalObservationIds(query: string, sensitivityMax = 1): string[] {
    const terms = query.normalize("NFKC").match(/[\p{L}\p{N}]+/gu) ?? [];
    if (!terms.length) return [];
    const match = [...new Set(terms.map((term) => `"${term.replaceAll('"', '""')}"`))].join(" OR ");
    return (this.db.query(`SELECT f.id FROM observations_fts f JOIN observations o ON o.id=f.id
      WHERE observations_fts MATCH ? AND o.replaced_by IS NULL AND o.sensitivity<=? ORDER BY bm25(observations_fts) LIMIT 100`).all(match, sensitivityMax) as Array<{ id: string }>)
      .map((row) => row.id);
  }

  retrievalObservations(sensitivityMax = 1, ids?: string[]): RetrievalObservation[] {
    if (ids?.length === 0) return [];
    const selection = ids === undefined ? "" : " AND id IN (SELECT value FROM json_each(?))";
    const rows = this.db.query<ObservationRow, Array<string | number>>(`SELECT * FROM observations
      WHERE replaced_by IS NULL AND sensitivity<=?${selection} ORDER BY entered,id`)
      .all(sensitivityMax, ...(ids === undefined ? [] : [JSON.stringify(ids)]));
    if (!rows.length) return [];
    const selected = JSON.stringify(rows.map((row) => row.id));
    const result = new Map(rows.map((row) => [row.id, { ...row, pages: [], sources: [] } as RetrievalObservation]));
    const pages = this.db.query<{ observation_id: string; page_slug: string }, [string]>(`SELECT observation_id,page_slug
      FROM observation_pages WHERE observation_id IN (SELECT value FROM json_each(?)) ORDER BY page_slug`).all(selected);
    for (const page of pages) result.get(page.observation_id)!.pages.push(page.page_slug);
    const sources = this.db.query<{ observation_id: string; id: string; kind: SourceKind }, [string]>(`SELECT os.observation_id,
      'src:'||os.source_id AS id,s.kind FROM observation_sources os JOIN sources s ON s.id=os.source_id
      WHERE os.observation_id IN (SELECT value FROM json_each(?)) ORDER BY os.source_id`).all(selected);
    for (const source of sources) result.get(source.observation_id)!.sources.push({ id: source.id, kind: source.kind });
    return [...result.values()];
  }

  observationContexts(ids: string[]): ExistingObservationContext[] {
    const rows = new Map(this.retrievalObservations(1, ids).map((row) => [row.id, row]));
    return ids.flatMap((id) => {
      const row = rows.get(id);
      return row ? [{ id, line: row.line, body: row.body, pages: row.pages, happened: row.happened,
        claimant: row.claimant, authority: row.authority, kind: row.kind,
        sources: row.sources.map((source) => source.id), sensitivity: row.sensitivity }] : [];
    });
  }

  sourceContexts(ids: string[]): Array<{ id: string; home: string | null; cwd: string | null }> {
    const unique = [...new Set(ids)];
    const rows = this.db.query<Pick<SourceRow, "id" | "home" | "metadata_json">, [string]>(`SELECT id,home,metadata_json
      FROM sources WHERE id IN (SELECT value FROM json_each(?))`).all(JSON.stringify(unique.map((id) => Number(id.slice(4)))));
    const byId = new Map(rows.map((row) => [`src:${row.id}`, row]));
    return unique.map((id) => {
      const row = byId.get(id);
      if (!row) throw new Error(`Missing source ${id}`);
      const metadata = JSON.parse(row.metadata_json) as Record<string, unknown>;
      return { id, home: row.home, cwd: typeof metadata.cwd === "string" ? metadata.cwd : null };
    });
  }

  open(id: string, options: { history?: boolean; full?: boolean; sensitivityMax?: number } = {}) {
    const sensitivityMax = options.sensitivityMax ?? 1;
    const historyRows = options.history
      ? sanitizeHistory(this.db.query("SELECT * FROM changes WHERE entry=? ORDER BY seq DESC LIMIT 20").all(id) as ChangeRow[], sensitivityMax)
      : undefined;
    if (/^src:\d+\/\d+$/.test(id)) {
      const match = /^src:(\d+)\/(\d+)$/.exec(id)!;
      const sourceId = Number(match[1]);
      const chunkIndex = Number(match[2]);
      const row = this.db.query(`SELECT c.*,s.text FROM chunks c JOIN sources s ON s.id=c.source_id
        WHERE c.source_id=? AND c.chunk_index=? AND s.sensitivity<=?`).get(sourceId, chunkIndex, sensitivityMax) as (ChunkRow & { text: string }) | null;
      if (!row) throw new Error(`Unknown or unavailable chunk ${id}`);
      const lines = splitSourceLines(row.text);
      const previous = this.db.query("SELECT id FROM chunks WHERE source_id=? AND chunk_index=?").get(sourceId, chunkIndex - 1) as { id: string } | null;
      const next = this.db.query("SELECT id FROM chunks WHERE source_id=? AND chunk_index=?").get(sourceId, chunkIndex + 1) as { id: string } | null;
      return { id, kind: "chunk" as const, context: row.context, text: lines.slice(row.start_line - 1, row.end_line).join("\n"),
        ...(previous ? { previous: previous.id } : {}),
        ...(next ? { next: next.id } : {}),
        ...(options.history ? { history: historyRows } : {}) };
    }
    if (/^src:\d+$/.test(id)) {
      const sourceId = Number(id.slice(4));
      const source = this.db.query<SourceRow, [number, number]>("SELECT * FROM sources WHERE id=? AND sensitivity<=?").get(sourceId, sensitivityMax);
      if (!source) throw new Error(`Unknown or unavailable source ${id}`);
      const job = this.db.query("SELECT id,status,error,finished_at FROM jobs WHERE source_id=?").get(sourceId);
      const neighbours = source.session ? this.sourceNeighbours(sourceId, source.session, sensitivityMax) : {};
      return { id, kind: "source" as const, source_kind: source.kind, label: source.label, session: source.session, home: source.home,
        happened: source.happened, sensitivity: source.sensitivity, external_id: source.external_id,
        metadata: JSON.parse(source.metadata_json), participants: JSON.parse(source.participants_json),
        digest: source.digest, chunks: (this.db.query("SELECT id,context FROM chunks WHERE source_id=? ORDER BY chunk_index").all(sourceId) as Array<{ id: string; context: string }>)
          .map((chunk) => `${chunk.id} ${chunk.context}`),
        ...(options.full ? { text: source.text } : {}), ...neighbours, job,
        ...(options.history ? { history: historyRows } : {}) };
    }
    const observation = this.db.query<ObservationRow, [string, number]>("SELECT * FROM observations WHERE id=? AND sensitivity<=?").get(id, sensitivityMax);
    if (observation) {
      const { kind: observationKind, ...fields } = observation;
      return { ...fields, kind: "observation" as const, observation_kind: observationKind,
        pages: (this.db.query("SELECT page_slug FROM observation_pages WHERE observation_id=? ORDER BY page_slug").all(id) as Array<{ page_slug: string }>).map((x) => x.page_slug),
        sources: (this.db.query("SELECT 'src:'||source_id AS source FROM observation_sources WHERE observation_id=? ORDER BY source_id").all(id) as Array<{ source: string }>).map((x) => x.source),
        evidence: (this.db.query("SELECT chunk_id FROM observation_evidence WHERE observation_id=? ORDER BY chunk_id").all(id) as Array<{ chunk_id: string }>).map((x) => x.chunk_id),
        ...(options.history ? { history: historyRows } : {}) };
    }
    const page = this.db.query<PageRow, [string]>("SELECT * FROM pages WHERE slug=?").get(id);
    if (page) {
      const visibleSlug = page.replaced_by ?? id;
      if (id !== "root" && !this.pageHasVisibleKnowledge(visibleSlug, sensitivityMax)) {
        throw new Error(`Unknown or unavailable id ${id}`);
      }
      const lineSensitivity = this.pageLineSensitivity(page);
      return { id, kind: "page" as const, category: page.category, line: lineSensitivity <= sensitivityMax ? page.line : null,
        aliases: JSON.parse(page.aliases_json), parent: page.parent, replaced_by: page.replaced_by, weight: page.weight,
        subpages: this.readablePages(sensitivityMax)
          .filter((child) => child.parent === id && this.pageHasVisibleKnowledge(child.slug, sensitivityMax))
          .map((child) => ({ slug: child.slug, line: child.line })),
        observations: (this.db.query(`SELECT o.id,o.line,o.body IS NOT NULL AS has_body,o.kind,o.happened,o.replaced_by
          FROM observations o JOIN observation_pages p ON p.observation_id=o.id
          WHERE p.page_slug=? AND o.sensitivity<=? ORDER BY o.entered,o.id`).all(id, sensitivityMax)),
        ...(options.history ? { history: historyRows } : {}) };
    }
    throw new Error(`Unknown or unavailable id ${id}`);
  }

  private sourceNeighbours(sourceId: number, session: string, sensitivityMax: number): { previous?: string; next?: string } {
    const rows = this.db.query(`SELECT id FROM sources WHERE session=? AND sensitivity<=?
      ORDER BY coalesce(happened,ingested_at),id`).all(session, sensitivityMax) as Array<{ id: number }>;
    const index = rows.findIndex((row) => row.id === sourceId);
    return { ...(index > 0 ? { previous: `src:${rows[index - 1].id}` } : {}),
      ...(index >= 0 && index + 1 < rows.length ? { next: `src:${rows[index + 1].id}` } : {}) };
  }

  private pageHasVisibleKnowledge(slug: string, sensitivityMax: number): boolean {
    return Boolean(this.db.query(`WITH RECURSIVE tree(slug) AS (
      SELECT slug FROM pages WHERE slug=?
      UNION ALL SELECT p.slug FROM pages p JOIN tree t ON p.parent=t.slug WHERE p.replaced_by IS NULL
    ) SELECT 1 FROM tree t JOIN observation_pages op ON op.page_slug=t.slug
      JOIN observations o ON o.id=op.observation_id WHERE o.sensitivity<=? LIMIT 1`).get(slug, sensitivityMax));
  }

  private pageLineSensitivity(page: PageRow): number {
    const inputs = JSON.parse(page.line_inputs_json ?? "[]") as string[];
    let maximum = 0;
    for (const input of inputs) if (/^src:\d+$/.test(input)) {
      const row = this.db.query("SELECT sensitivity FROM sources WHERE id=?").get(Number(input.slice(4))) as { sensitivity: number } | null;
      maximum = Math.max(maximum, row?.sensitivity ?? 0);
    }
    if (!inputs.length) {
      const row = this.db.query(`SELECT coalesce(max(o.sensitivity),0) sensitivity FROM observations o
        JOIN observation_pages op ON op.observation_id=o.id WHERE op.page_slug=?`).get(page.slug) as { sensitivity: number };
      maximum = Number(row.sensitivity);
    }
    return maximum;
  }

  stats(): unknown {
    return {
      sources: (this.db.query("SELECT count(*) AS n FROM sources").get() as { n: number }).n,
      jobs: this.db.query("SELECT status,count(*) AS n FROM jobs GROUP BY status ORDER BY status").all(),
      pages: (this.db.query("SELECT count(*) AS n FROM pages").get() as { n: number }).n,
      observations: (this.db.query("SELECT count(*) AS n FROM observations").get() as { n: number }).n,
      chunks: (this.db.query("SELECT count(*) AS n FROM chunks").get() as { n: number }).n,
      lineage: (this.db.query("SELECT count(*) AS n FROM observation_sources").get() as { n: number }).n,
      evidence: (this.db.query("SELECT count(*) AS n FROM observation_evidence").get() as { n: number }).n,
      model_calls: this.db.query("SELECT phase,model,service_tier,count(*) AS calls,sum(cost_usd) AS cost_usd FROM model_calls GROUP BY phase,model,service_tier").all(),
    };
  }
}

function chunkPageClause(pages: string[] | undefined, sensitivityMax: number): { sql: string; values: Array<string | number> } {
  if (!pages?.length) return { sql: "", values: [] };
  const placeholders = pages.map(() => "?").join(",");
  return { sql: ` AND EXISTS (SELECT 1 FROM observation_evidence oe
    JOIN observations o ON o.id=oe.observation_id JOIN observation_pages op ON op.observation_id=o.id
    WHERE oe.chunk_id=c.id AND o.replaced_by IS NULL AND o.sensitivity<=? AND op.page_slug IN (${placeholders}))`,
    values: [sensitivityMax, ...pages] };
}
function splitSourceLines(text: string): string[] { return text.split("\n"); }
function validateChunkStarts(starts: Array<{ start_line: number; context: string }>, lineCount: number): void {
  if (!starts.length || starts[0]!.start_line !== 1) throw new Error("Chunks must start at line 1");
  let previous = 0;
  for (const [index, start] of starts.entries()) {
    if (!Number.isInteger(start.start_line) || start.start_line <= previous || start.start_line > lineCount) {
      throw new Error(`Invalid chunk start at index ${index}`);
    }
    if (!start.context.trim()) throw new Error(`Chunk ${index + 1} needs a context line`);
    previous = start.start_line;
  }
}

function normalizeName(value: string): string {
  return value.normalize("NFKC").toLocaleLowerCase().replace(/[^\p{L}\p{N}]+/gu, " ").trim();
}
function pageContext(row: PageRow): ExistingPageContext {
  return { slug: row.slug, category: row.category, line: row.line, aliases: JSON.parse(row.aliases_json),
    parent: row.parent, replaced_by: row.replaced_by };
}
function sanitizeHistory(rows: ChangeRow[], sensitivityMax: number): ChangeRow[] {
  return rows.map((row) => {
    if (!row.before_json) return row;
    try {
      const before = JSON.parse(row.before_json);
      const sensitivity = before?.observation?.sensitivity ?? before?.sensitivity;
      return typeof sensitivity === "number" && sensitivity > sensitivityMax ? { ...row, before_json: null } : row;
    } catch { return { ...row, before_json: null }; }
  });
}
function* embeddingRows(rows: Iterable<EmbeddingRow>) {
  for (const row of rows) yield { id: row.entry_id, vector: blobToVector(row.vector) };
}
export function vectorToBlob(vector: number[]): Uint8Array {
  return new Uint8Array(new Float32Array(vector).buffer);
}
export function blobToVector(blob: Uint8Array): Float32Array {
  const copy = blob.buffer.slice(blob.byteOffset, blob.byteOffset + blob.byteLength);
  return new Float32Array(copy);
}

/** Observation embeddings always carry the claim line; the body only adds detail. */
export function observationEmbeddingText(value: { line: string; body: string | null }): string {
  const body = value.body?.trim();
  return body ? `${value.line}\n\n${body}` : value.line;
}
