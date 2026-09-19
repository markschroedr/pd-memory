export const SOURCE_KINDS = [
  "note_user", "note_agent", "conversation", "meeting", "chat_log", "document", "coding_session",
] as const;
export type SourceKind = (typeof SOURCE_KINDS)[number];
export const PAGE_CATEGORIES = ["actor", "artifact", "place", "event", "project", "topic"] as const;
export type PageCategory = (typeof PAGE_CATEGORIES)[number];
export const AUTHORITIES = ["user", "agent", "third_party", "unknown"] as const;
export type Authority = (typeof AUTHORITIES)[number];
export const OBSERVATION_KINDS = ["fact", "question", "commitment"] as const;
export type ObservationKind = (typeof OBSERVATION_KINDS)[number];

export interface RuntimeConfig {
  workspace: { db: string; defaultProfile: string };
  openai: {
    provider: "openai" | "openrouter";
    routing: { only: string[] } | null;
    baseUrl: string;
    apiKeyEnv: string;
    model: string;
    serviceTier: "flex" | "default";
    store: false;
    retentionPolicy: "zero_data_retention" | "modified_abuse_monitoring" | "standard_store_false";
    retentionVerified: boolean;
    timeoutMs: number;
  };
  embeddings: {
    provider: "openrouter" | "local_pplx";
    baseUrl: string; apiKeyEnv: string | null; model: string; dimension: number; timeoutMs: number;
    zdr: boolean; allowFallbacks: boolean; only: string[];
  };
  identity: { userNames: string[]; thirdPartyNames: string[] };
  ingest: { matchesPerCandidate: number };
  chunk: { targetTokens: number; singleChunkTokens: number; extractWindowTokens: number };
  search: { defaultLimit: number; maxLimit: number; rrfK: number };
  brief: { spineShare: number; perPageCap: number; nextCap: number; recentBudget: number };
  buckets: { bucket3Min: number; bucket2Min: number };
  formulas: Record<string, string>;
  callSites: { search: string; searchChunk: string; briefStanding: string; briefSituational: string; pageRank: string };
  sourcePriors: Record<SourceKind, number>;
  profiles: Record<string, { root: string; sensitivityMax: number; budget: number }>;
  pricing: { inputPerMillion: number; cachedInputPerMillion: number; outputPerMillion: number; embeddingPerMillion: number };
}

export interface SourceInput {
  kind: SourceKind;
  text: string;
  label: string;
  happened: string | null;
  sensitivity: number;
  session: string | null;
  externalId: string | null;
  metadata: Record<string, unknown>;
  participants: string[];
  home: string | null;
}

export type { CandidatePage, CandidateObservation, Extraction, PageProposal,
  ObservationPayload, ObservationProposal, IntegrationProposal } from "./ingestion";

export interface Chunk {
  id: string;
  source: string;
  index: number;
  start_line: number;
  end_line: number;
  context: string;
  text: string;
}

export interface ExistingPageContext {
  slug: string;
  category: PageCategory;
  line: string | null;
  aliases: string[];
  parent: string | null;
  replaced_by: string | null;
}
export interface ExistingObservationContext {
  id: string;
  line: string;
  body: string | null;
  pages: string[];
  happened: string | null;
  claimant: string | null;
  authority: Authority;
  kind: ObservationKind;
  sources: string[];
  sensitivity: number;
}

export interface ModelUsage {
  input_tokens?: number;
  output_tokens?: number;
  input_tokens_details?: { cached_tokens?: number };
  [key: string]: unknown;
}
export interface ModelResult<T> {
  value: T;
  requestId: string | null;
  model: string;
  serviceTier: string | null;
  usage: ModelUsage;
  raw: string;
  repaired: boolean;
}

export interface SubmitResult {
  source: string;
  job: number;
  reused: boolean;
  conflict?: string;
  status: string;
}
