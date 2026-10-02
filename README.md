# pd-memory

Give agents an overview of what they know, then let them look up details when needed.

pd-memory turns conversations and documents into durable observations, grouped into pages worth returning to. New material is compared with existing memory, so repeated claims can be combined and later corrections can replace earlier ones. Each observation keeps its sources, evidence, and correction history.

Experimental personal project, extracted from my daily agent setup. Built with Go and SQLite. One executable, no service. The earlier TypeScript engine remains at tag `v0.1-typescript`.

## Setup

```sh
go build -o pd-memory .
cp config.example.toml /path/outside/git/pd-memory.toml
export PD_MEMORY_CONFIG=/path/outside/git/pd-memory.toml
```

Set the workspace directory, identity names, providers, and prices in the configuration. Credentials come from environment variables. Verify your provider's retention controls before enabling `retention_verified`.

Generation supports direct OpenAI Responses with Flex, or OpenRouter with explicit zero-retention routing. Embeddings use OpenRouter or a local Perplexity service. There is no fallback between embedding providers. Model calls incur costs; `stats` shows recorded usage and estimated cost.

## Use

```sh
pd-memory ingest --path notes.txt --kind document --label Notes --wait
pd-memory brief
pd-memory search --query "architecture decisions"
pd-memory open ID --history
pd-memory note --line "A durable observation." --page root --actor user --wait
pd-memory help
```

Add `--json` for machine output. `catalog` exposes command input and result schemas with the engine version. `brief --folder PATH` adds project context; `focus` adjusts that folder's selection. Plain reads never generate summaries or write to memory.

Ingestion and user mutations append to `log.db` and wake one locked worker. Without `--wait`, it reports queued work. The worker updates `memory.db`, then builds missing closed-period summaries and refreshes due current-state compositions. No timers or host maintenance calls are needed. Quiet workspaces wait for the next ingestion; `maintain` and `brief --compose` are manual alternatives.

Back up `log.db`: it is the canonical, append-only record. `memory.db` is rebuildable. `rebuild` makes paid calls and preserves the old projection beside the new one. Workspaces, not read filters, are the privacy boundary.

## TypeScript hosts

Use `MemoryClient` from `integrations/client.ts`. Its `call(command, input)` returns the command's typed result. It checks the binary version on first use and reports conflicts separately from other failures.

Regenerate the committed types after command changes:

```sh
pd-memory catalog --typescript > integrations/types.ts
```

## Coding sessions and Pi

```sh
pd-memory import sessions --pi /path/to/pi/sessions --dry-run
pd-memory import sessions --pi /path/to/pi/sessions --wait
```

The importer also accepts repeatable `--claude-code` and `--codex` paths. It reads canonical completed exchanges, skips subagents, and records message IDs so later imports capture only new messages. Routes and machine-specific fetching stay outside the repository.

The [Pi extension](integrations/pi.ts) adds a standing brief and memory tools. Its settings live in `~/.config/pd-memory/pi.json`:

```json
{"binary":"pd-memory","config":"/absolute/path/to/pd-memory.toml"}
```

Capture defaults to daily imports; `capture_mode: "live"` enables after-turn capture.
