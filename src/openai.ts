import { requireApprovedRetention, requireCredentials } from "./config";
import type { ModelResult, RuntimeConfig } from "./types";
import { z } from "zod";

const usageSchema = z.object({
  input_tokens: z.number().nonnegative().optional(), output_tokens: z.number().nonnegative().optional(),
  input_tokens_details: z.object({ cached_tokens: z.number().nonnegative().optional() }).passthrough().optional(),
}).passthrough();
const responseSchema = z.object({
  id: z.string().optional(), model: z.string().optional(), service_tier: z.string().nullable().optional(),
  status: z.string().optional(), incomplete_details: z.unknown().optional(), usage: usageSchema.optional(),
  output: z.array(z.object({ type: z.string(), content: z.array(z.object({
    type: z.string(), text: z.string().optional(), refusal: z.string().optional(),
  })).optional() })),
});
const apiErrorSchema = z.object({ error: z.object({ message: z.string() }) });

export class OpenAIClient {
  constructor(readonly config: RuntimeConfig) {}

  async structured<T>(options: {
    name: string;
    schema: Record<string, unknown>;
    system: string;
    input: string;
    validate: (value: unknown) => T;
    sensitive?: boolean;
    onAttempt?: (result: ModelResult<unknown>, valid: boolean, error?: Error) => void;
  }): Promise<ModelResult<T>> {
    if (options.sensitive !== false) requireApprovedRetention(this.config);
    let firstRaw: string | null = null;
    let firstError: Error | null = null;
    for (let attempt = 0; attempt < 2; attempt++) {
      const input = attempt === 0 ? options.input : [
        options.input,
        "The prior response failed local validation. Return a corrected complete object.",
        `Validation error: ${firstError!.message}`,
        `Prior response: ${firstRaw}`,
      ].join("\n\n");
      const response = responseSchema.parse(await this.request("/responses", {
        model: this.config.openai.model,
        ...(this.config.openai.provider === "openai"
          ? { service_tier: this.config.openai.serviceTier }
          : { provider: { zdr: true, allow_fallbacks: false, require_parameters: true, only: this.config.openai.routing!.only } }),
        store: false,
        input: [
          { role: "system", content: [{ type: "input_text", text: options.system }] },
          { role: "user", content: [{ type: "input_text", text: input }] },
        ],
        text: { format: { type: "json_schema", name: options.name, strict: true, schema: options.schema } },
      }));
      const raw = outputText(response);
      const attemptResult: ModelResult<unknown> = {
        value: null, raw, repaired: attempt === 1, requestId: stringOrNull(response.id),
        model: typeof response.model === "string" ? response.model : this.config.openai.model,
        serviceTier: stringOrNull(response.service_tier), usage: response.usage ?? {},
      };
      try {
        const parsed: unknown = JSON.parse(raw);
        attemptResult.value = parsed;
        const value = options.validate(parsed);
        options.onAttempt?.(attemptResult, true);
        return { ...attemptResult, value };
      } catch (error) {
        const validationError = error instanceof Error ? error : new Error(String(error));
        options.onAttempt?.(attemptResult, false, validationError);
        firstRaw ??= raw;
        firstError = validationError;
      }
    }
    throw new Error(`Structured output failed validation after one repair: ${firstError!.message}`);
  }

  async probe(): Promise<unknown> {
    const schema = {
      type: "object", additionalProperties: false, required: ["ok"],
      properties: { ok: { type: "boolean" } },
    };
    const result = await this.structured({
      name: "pd_memory_capability_probe", schema,
      system: "Return the requested JSON only.", input: "Set ok to true.", sensitive: false,
      validate(value) {
        if (!value || typeof value !== "object" || !("ok" in value) || value.ok !== true) throw new Error("Probe did not return ok=true");
        return value;
      },
    });
    return {
      provider: this.config.openai.provider,
      configured_model: this.config.openai.model,
      returned_model: result.model,
      requested_service_tier: this.config.openai.provider === "openai" ? this.config.openai.serviceTier : null,
      returned_service_tier: result.serviceTier,
      strict_structured_output: true,
      store: false,
    };
  }

  private async request(path: string, body: unknown): Promise<unknown> {
    const key = requireCredentials(this.config);
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(), this.config.openai.timeoutMs);
    try {
      const response = await fetch(`${this.config.openai.baseUrl}${path}`, {
        method: "POST", signal: controller.signal,
        headers: { authorization: `Bearer ${key}`, "content-type": "application/json" },
        body: JSON.stringify(body),
      });
      const requestId = response.headers.get("x-request-id");
      const payload: unknown = await response.json();
      if (!response.ok) {
        const error = apiErrorSchema.safeParse(payload);
        const message = error.success ? error.data.error.message : response.statusText;
        throw new Error(`${this.config.openai.provider} ${path} failed (${response.status}${requestId ? `, ${requestId}` : ""}): ${message}`);
      }
      return payload;
    } finally {
      clearTimeout(timeout);
    }
  }
}

function outputText(response: z.infer<typeof responseSchema>): string {
  if (response.status === "incomplete") throw new Error(`OpenAI response incomplete: ${JSON.stringify(response.incomplete_details ?? {})}`);
  if (!Array.isArray(response.output)) throw new Error("OpenAI response has no output array");
  for (const item of response.output) {
    if (item.type !== "message" || !Array.isArray(item.content)) continue;
    for (const content of item.content) {
      if (content.type === "refusal") throw new Error(`OpenAI refused structured request: ${content.refusal ?? "unknown reason"}`);
      if (content.type === "output_text" && typeof content.text === "string") return content.text;
    }
  }
  throw new Error("OpenAI response has no output_text");
}
function stringOrNull(value: unknown): string | null { return typeof value === "string" ? value : null; }
