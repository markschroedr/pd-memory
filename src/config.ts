import { resolve } from "node:path";
import type { RuntimeConfig } from "./types";

function table(value: unknown, name: string): Record<string, unknown> {
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error(`Missing [${name}] configuration`);
  return value as Record<string, unknown>;
}
function text(value: unknown, name: string): string {
  if (typeof value !== "string" || !value) throw new Error(`Missing ${name}`);
  return value;
}
function finite(value: unknown, name: string): number {
  if (typeof value !== "number" || !Number.isFinite(value)) throw new Error(`Invalid ${name}`);
  return value;
}

export async function loadConfig(path: string): Promise<RuntimeConfig> {
  const absolute = resolve(path);
  const raw = Bun.TOML.parse(await Bun.file(absolute).text()) as Record<string, unknown>;
  const workspace = table(raw.workspace, "workspace");
  const openai = table(raw.openai, "openai");
  const embeddings = table(raw.embeddings, "embeddings");
  const embeddingProvider = text(embeddings.provider ?? "openrouter", "embeddings.provider");
  if (embeddingProvider !== "openrouter" && embeddingProvider !== "local_pplx") throw new Error("embeddings.provider must be openrouter or local_pplx");
  if (embeddingProvider === "openrouter" && embeddings.zdr !== true) throw new Error("embeddings.zdr must be true for OpenRouter");
  if (embeddingProvider === "openrouter" && embeddings.allow_fallbacks !== false) throw new Error("embeddings.allow_fallbacks must be false for OpenRouter");
  const embeddingProviders = embeddingProvider === "openrouter" ? stringList(embeddings.only, "embeddings.only") : [];
  if (embeddingProvider === "openrouter" && !embeddingProviders.length) throw new Error("embeddings.only must name at least one trusted provider");
  const identity = table(raw.identity, "identity");
  const ingest = table(raw.ingest, "ingest");
  const chunk = table(raw.chunk, "chunk");
  const search = table(raw.search, "search");
  const brief = table(raw.brief, "brief");
  const buckets = table(raw.buckets, "buckets");
  const formulas = table(raw.formulas, "formulas");
  const callSites = table(raw.call_sites, "call_sites");
  const sourcePriors = table(raw.source_priors, "source_priors");
  const profilesRaw = table(raw.profiles, "profiles");
  const profiles = Object.fromEntries(Object.entries(profilesRaw).map(([name, value]) => {
    const profile = table(value, `profiles.${name}`);
    const budget = positiveInteger(profile.budget, `profiles.${name}.budget`);
    if (budget < 200) throw new Error(`profiles.${name}.budget must be at least 200`);
    return [name, { root: text(profile.root, `profiles.${name}.root`),
      sensitivityMax: unit(profile.sensitivity_max, `profiles.${name}.sensitivity_max`), budget }];
  }));
  const pricing = table(raw.pricing, "pricing");
  const serviceTier = text(openai.service_tier, "openai.service_tier");
  if (serviceTier !== "flex" && serviceTier !== "default") throw new Error("openai.service_tier must be flex or default");
  if (openai.store !== false) throw new Error("openai.store must be false");
  const retentionPolicy = text(openai.retention_policy, "openai.retention_policy");
  if (retentionPolicy !== "zero_data_retention" && retentionPolicy !== "modified_abuse_monitoring" && retentionPolicy !== "standard_store_false") {
    throw new Error("openai.retention_policy must be zero_data_retention, modified_abuse_monitoring, or standard_store_false");
  }
  const dbValue = text(workspace.db, "workspace.db");
  const defaultProfile = text(workspace.default_profile, "workspace.default_profile");
  if (!profiles[defaultProfile]) throw new Error(`Unknown workspace.default_profile ${defaultProfile}`);
  return {
    workspace: { db: resolve(absolute, "..", dbValue), defaultProfile },
    openai: {
      baseUrl: text(openai.base_url, "openai.base_url").replace(/\/$/, ""),
      apiKeyEnv: text(openai.api_key_env, "openai.api_key_env"),
      model: text(openai.model, "openai.model"),
      serviceTier,
      store: false,
      retentionPolicy: retentionPolicy as RuntimeConfig["openai"]["retentionPolicy"],
      retentionVerified: openai.retention_verified === true,
      timeoutMs: finite(openai.timeout_ms, "openai.timeout_ms"),
    },
    embeddings: {
      provider: embeddingProvider,
      baseUrl: text(embeddings.base_url, "embeddings.base_url").replace(/\/$/, ""),
      apiKeyEnv: embeddingProvider === "openrouter" ? text(embeddings.api_key_env, "embeddings.api_key_env") : null,
      model: text(embeddings.model, "embeddings.model"),
      dimension: positiveInteger(embeddings.dimension, "embeddings.dimension"),
      timeoutMs: finite(embeddings.timeout_ms, "embeddings.timeout_ms"),
      zdr: embeddingProvider === "openrouter",
      allowFallbacks: false,
      only: embeddingProviders,
    },
    identity: {
      userNames: stringList(identity.user_names, "identity.user_names"),
      thirdPartyNames: stringList(identity.third_party_names, "identity.third_party_names"),
    },
    ingest: { matchesPerCandidate: positiveInteger(ingest.matches_per_candidate ?? 5, "ingest.matches_per_candidate") },
    chunk: {
      targetTokens: positiveInteger(chunk.target_tokens, "chunk.target_tokens"),
      singleChunkTokens: positiveInteger(chunk.single_chunk_tokens, "chunk.single_chunk_tokens"),
      extractWindowTokens: positiveInteger(chunk.extract_window_tokens, "chunk.extract_window_tokens"),
    },
    search: {
      defaultLimit: positiveInteger(search.default_limit, "search.default_limit"),
      maxLimit: positiveInteger(search.max_limit, "search.max_limit"),
      rrfK: positiveInteger(search.rrf_k, "search.rrf_k"),
    },
    brief: {
      spineShare: unit(brief.spine_share, "brief.spine_share"),
      perPageCap: positiveInteger(brief.per_page_cap, "brief.per_page_cap"),
      nextCap: positiveInteger(brief.next_cap, "brief.next_cap"),
      recentBudget: positiveInteger(brief.recent_budget, "brief.recent_budget"),
    },
    buckets: { bucket3Min: unit(buckets.bucket_3_min, "buckets.bucket_3_min"), bucket2Min: unit(buckets.bucket_2_min, "buckets.bucket_2_min") },
    formulas: Object.fromEntries(Object.entries(formulas).map(([name, value]) => [name, text(value, `formulas.${name}`)])),
    callSites: {
      search: text(callSites.search, "call_sites.search"),
      searchChunk: text(callSites.search_chunk, "call_sites.search_chunk"),
      briefStanding: text(callSites.brief_standing, "call_sites.brief_standing"),
      briefSituational: text(callSites.brief_situational, "call_sites.brief_situational"),
      pageRank: text(callSites.page_rank, "call_sites.page_rank"),
    },
    sourcePriors: Object.fromEntries(["note_user", "note_agent", "conversation", "meeting", "chat_log", "document", "coding_session"]
      .map((kind) => [kind, finite(sourcePriors[kind], `source_priors.${kind}`)])) as RuntimeConfig["sourcePriors"],
    profiles,
    pricing: {
      inputPerMillion: finite(pricing.input_per_million, "pricing.input_per_million"),
      cachedInputPerMillion: finite(pricing.cached_input_per_million, "pricing.cached_input_per_million"),
      outputPerMillion: finite(pricing.output_per_million, "pricing.output_per_million"),
      embeddingPerMillion: finite(pricing.embedding_per_million, "pricing.embedding_per_million"),
    },
  };
}

function positiveInteger(value: unknown, name: string): number {
  const number = finite(value, name);
  if (!Number.isInteger(number) || number < 1) throw new Error(`Invalid ${name}`);
  return number;
}
function unit(value: unknown, name: string): number {
  const number = finite(value, name);
  if (number < 0 || number > 1) throw new Error(`Invalid ${name}`);
  return number;
}

function stringList(value: unknown, name: string): string[] {
  if (!Array.isArray(value) || value.some((item) => typeof item !== "string" || !item)) throw new Error(`Invalid ${name}`);
  return value as string[];
}

export function requireCredentials(config: RuntimeConfig): string {
  const key = process.env[config.openai.apiKeyEnv];
  if (!key) throw new Error(`Credential environment variable ${config.openai.apiKeyEnv} is not set`);
  return key;
}

export function requireEmbeddingCredentials(config: RuntimeConfig): string | null {
  if (config.embeddings.apiKeyEnv === null) return null;
  const key = process.env[config.embeddings.apiKeyEnv];
  if (!key) throw new Error(`Credential environment variable ${config.embeddings.apiKeyEnv} is not set`);
  return key;
}

export function requireApprovedRetention(config: RuntimeConfig): void {
  if (!config.openai.retentionVerified) {
    throw new Error("OpenAI retention is not operator-verified; set openai.retention_verified=true only after confirming the configured project policy");
  }
}
