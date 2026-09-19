# pd-memory

An experimental agent-memory library built around **progressive disclosure and maintained claims**, rather than a growing collection of summaries.

The central question: what should an agent remember, and how can it find the right level of detail without loading everything?

Built with Bun, TypeScript, SQLite, and Zod. Designed for a single user's memory, with a CLI and an in-process library.

## How it works

```text
Source → semantic passages → candidate observations → reconciliation → SQLite
                                                     ↑
                                       related existing observations

Agent: brief → search → open observation → open passage → full source
```

- **Sources** preserve the input text. Passages reference immutable line ranges and have short context descriptions.
- **Observations** carry a claim, optional detail, claimant, authority, time, confidence, importance, durability, and sensitivity.
- **Pages** organize durable subjects. An observation can belong to several pages without being duplicated.
- **Reconciliation** decides whether to discard, create, attach evidence, update, merge, or supersede. Extraction is a proposal, not a commitment to retain everything.
- **Mutations** validate structural invariants and commit related changes transactionally. Corrections preserve history and provenance.

The model decides meaning. Deterministic code checks references, operation coverage, hierarchy, and sensitivity constraints. These checks cannot prove that a claim is true or a merge is semantically correct.

## Progressive disclosure

`brief` assembles a budgeted overview. `search` returns lean previews over both observations and source passages. `open` reveals the selected entry, then its evidence or full source when requested.

The agent can repeat these operations as it learns what to look for. Retrieval does not have to end after a single search.

Search fuses lexical and exact cosine rankings. Vector candidates stream from SQLite once per layer across all query facets; bounded heaps retain the best candidates. Observation memberships and source metadata are loaded in batches rather than per observation.

## Setup

Requires Bun 1.3 or newer.

```sh
bun install --frozen-lockfile
cp config.example.toml /path/outside/repo/pd-memory.toml
export PD_MEMORY_CONFIG=/path/outside/repo/pd-memory.toml
export OPENAI_API_KEY=...
export OPENROUTER_API_KEY=...
bun src/cli.ts doctor
```

Set the database path, model, pricing, identities, and provider policy in that configuration. Generation uses the OpenAI Responses API. Embeddings use OpenRouter or the supported local Perplexity endpoint.

Do not set `retention_verified=true` until you have verified the configured provider policy. `store=false` alone does not establish Zero Data Retention. Use `standard_store_false` only with explicit data-owner approval.

`doctor` checks configuration without calling a provider. `doctor --live` makes small provider requests and can incur charges.

## CLI

```sh
# Ingest a source and wait for reconciliation. This makes model calls.
bun src/cli.ts ingest --path /path/to/example.txt --kind document \
  --label "Example project decisions" --wait

# Explore the resulting memory.
bun src/cli.ts brief --budget 1200
bun src/cli.ts search --query "project decisions" --query "constraints and rationale"
bun src/cli.ts search --query "original wording" --layer chunks

# Use an actual id returned by search or brief.
bun src/cli.ts open OBSERVATION_ID --history
bun src/cli.ts open src:1/1
bun src/cli.ts open src:1 --full

# A task-specific overview can gather knowledge across pages.
bun src/cli.ts brief --for "Plan the next project milestone" --budget 1200

# Queue a collection; manifest paths resolve relative to the manifest.
bun src/cli.ts ingest --manifest /path/to/manifest.json --wait

bun src/cli.ts jobs
bun src/cli.ts worker
bun src/cli.ts stats
bun src/cli.ts help
```

Search and brief output are lean text by default; add `--json` for structured output. The CLI also supports direct notes, corrections, page merges, incremental briefs, and rebuilding embeddings. `catalog` exposes the command schemas without opening a database.

Repeated source content reuses its existing job. Conflicting source metadata is reported rather than silently overwritten. Failed jobs remain inspectable and retryable.

## Library

```ts
import { PdMemoryRuntime } from "./src/index";

const memory = await PdMemoryRuntime.open(process.env.PD_MEMORY_CONFIG!);
try {
  const result = await memory.command("brief", { budget: 1200 });
  console.log(result.text);
} finally {
  memory.close();
}
```

`src/index.ts` exports the runtime, command registry, ingestion functions, retrieval, and mutation services. The CLI and library share the command definitions. `integrations/pi.ts` is an optional subprocess adapter; `scripts/import-pi-sessions.ts` imports session increments using external configuration.

## Current limits

- This is a working prototype, not a production-grade or multi-tenant memory service.
- Vector search is exact and linear in corpus size. Streaming bounds candidate memory; it is not an approximate nearest-neighbor index.
- Confidence and importance are model-assigned ranking signals, not calibrated probabilities.
- Sensitivity profiles are read filters for a trusted caller, not authentication or tenant isolation.
- Semantic attribution, consolidation, and retention quality need evaluation. No comparative benchmark result is claimed.
- Brief budgets use an approximate word-based token estimate.
- Development schema changes generally use fresh runtimes, not historical migrations.

For local static verification, run `bun run check`. Keep datasets, credentials, runtime state, and generated evaluation results outside Git.
