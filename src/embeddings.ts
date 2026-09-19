import { createHash } from "node:crypto";
import { z } from "zod";
import { requireEmbeddingCredentials } from "./config";
import type { MemoryStore } from "./store";
import type { ModelResult, RuntimeConfig } from "./types";

export interface EmbeddedValue { vector: number[]; inputHash: string }
const usageSchema = z.object({ input_tokens: z.number().nonnegative().optional(),
  prompt_tokens: z.number().nonnegative().optional() }).passthrough();
const apiErrorSchema = z.object({ error: z.object({ message: z.string() }) });

export async function rebuildObservationEmbeddings(store: MemoryStore, client: EmbeddingClient, config: RuntimeConfig, batchSize = 64): Promise<number> {
  const observations = store.observationEmbeddingInputs();
  const chunks = store.chunkEmbeddingInputs();
  for (const [phase, inputs, save] of [
    ["embed_rebuild_observations", observations, (values: Array<EmbeddedValue & { id: string }>) => store.storeObservationEmbeddings(config.embeddings.model, values)],
    ["embed_rebuild_chunks", chunks, (values: Array<EmbeddedValue & { id: string }>) => store.storeChunkEmbeddings(config.embeddings.model, values)],
  ] as const) {
    for (let start = 0; start < inputs.length; start += batchSize) {
      const batch = inputs.slice(start, start + batchSize);
      const result = await client.embed(batch.map((item) => item.text));
      store.recordModelCall(null, phase, result, config);
      save(batch.map((item, index) => ({ id: item.id, ...result.value[index]! })));
    }
  }
  return observations.length + chunks.length;
}

export class EmbeddingClient {
  constructor(readonly config: RuntimeConfig) {}

  async embed(inputs: string[]): Promise<ModelResult<EmbeddedValue[]>> {
    if (!inputs.length) return { value: [], requestId: null, model: this.config.embeddings.model, serviceTier: null, usage: {}, raw: "", repaired: false };
    const local = this.config.embeddings.provider === "local_pplx";
    const response = await this.request(local ? "" : "/embeddings", local ? {
      texts: inputs, max_length: 1024, batch_size: 8, quantization: "none", normalize_embeddings: false,
    } : {
      model: this.config.embeddings.model, input: inputs, encoding_format: "float",
      provider: { zdr: this.config.embeddings.zdr, allow_fallbacks: this.config.embeddings.allowFallbacks, only: this.config.embeddings.only },
    });
    const vectorSchema = z.array(z.number()).length(this.config.embeddings.dimension)
      .refine((vector) => vector.some((value) => value !== 0), "Embedding must have non-zero norm");
    const metadata = { id: z.string().optional(), model: z.string().optional() };
    let vectors: number[][];
    let usage: z.infer<typeof usageSchema>;
    let id: string | undefined;
    let model: string | undefined;
    if (local) {
      const parsed = z.object({ ...metadata, embeddings: z.array(vectorSchema).length(inputs.length),
        token_counts: z.array(z.number().nonnegative()).optional() }).parse(response);
      vectors = parsed.embeddings;
      usage = { input_tokens: parsed.token_counts?.reduce((sum, count) => sum + count, 0) ?? 0 };
      ({ id, model } = parsed);
    } else {
      const parsed = z.object({ ...metadata, usage: usageSchema.optional(),
        data: z.array(z.object({ index: z.number().int().nonnegative(), embedding: vectorSchema })).length(inputs.length),
      }).parse(response);
      const ordered = parsed.data.sort((a, b) => a.index - b.index);
      if (ordered.some((item, index) => item.index !== index)) throw new Error("Embedding response has missing or duplicate input indexes");
      vectors = ordered.map((item) => item.embedding);
      usage = { ...parsed.usage, input_tokens: parsed.usage?.prompt_tokens ?? parsed.usage?.input_tokens };
      ({ id, model } = parsed);
    }
    const value = vectors.map((vector, index) => ({ vector, inputHash: createHash("sha256").update(inputs[index]!).digest("hex") }));
    return { value, requestId: id ?? null, model: model ?? this.config.embeddings.model,
      serviceTier: null, usage, raw: "", repaired: false };
  }

  async probe(): Promise<unknown> {
    const result = await this.embed(["pd-memory capability probe"]);
    return {
      provider: this.config.embeddings.provider, configured_model: this.config.embeddings.model,
      returned_model: result.model, dimensions: result.value[0]!.vector.length,
      ...(this.config.embeddings.provider === "openrouter" ? {
        zdr: this.config.embeddings.zdr, allow_fallbacks: this.config.embeddings.allowFallbacks, only: this.config.embeddings.only,
      } : {}),
    };
  }

  private async request(path: string, body: unknown): Promise<unknown> {
    const key = requireEmbeddingCredentials(this.config);
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(), this.config.embeddings.timeoutMs);
    try {
      const response = await fetch(`${this.config.embeddings.baseUrl}${path}`, {
        method: "POST", signal: controller.signal,
        headers: { ...(key === null ? {} : { authorization: `Bearer ${key}` }), "content-type": "application/json" }, body: JSON.stringify(body),
      });
      const requestId = response.headers.get("x-request-id");
      const payload: unknown = await response.json();
      if (!response.ok) {
        const error = apiErrorSchema.safeParse(payload);
        throw new Error(`Embedding ${path} failed (${response.status}${requestId ? `, ${requestId}` : ""}): ${error.success ? error.data.error.message : response.statusText}`);
      }
      return payload;
    } finally { clearTimeout(timeout); }
  }
}
