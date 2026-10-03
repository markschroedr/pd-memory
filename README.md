# pd-memory

Memory for agents that starts with a map. The agent gets a short overview of everything it knows at the start of a session, and opens the details when it needs them.

<p align="center"><img src="assets/map.svg" alt="A compact brief of headings and one-line entries. One entry opens into its full observation with history and related pages, which links back to the sources it came from." width="760"></p>

Most memory systems are built around search. That works when the agent knows what it is looking for, but in daily work it often doesn't even know there is something to search for. If I ask my agent what I should read this weekend, there is no good query. The answer depends on the shape of everything that is there, and that is what a search result doesn't show.

There are two failure modes to avoid. By trying to be complete, you are tempted to increase the token budget and bloat the context, and on the other end there is the search-only case, where the agent doesn't know what it can search for. pd-memory tries to sit between them with a dictionary-style overview. Subjects are pages, and below each page are one-line observations:

```text
## billing (project) — Usage-based billing for the platform. (14)
- 3f9a1c [3+] Customers are invoiced monthly from metered usage, and refunds need manual approval. (4; 12 Sep)
- 81d2e0 [2] The free tier was raised to 500 runs after the pricing review. (1; 28 Aug)
### invoicing (artifact) — Invoice generation and delivery. (5)
```

The id opens an entry, the number in brackets shows how prominent it is, a `+` means there is more detail behind it, and the end shows how many sources back it and when it happened. From there the agent can notice where it lacks detail, open the entry, follow links to related pages and check the sources behind a claim. Retrieval becomes part of the agent's thinking instead of a separate step.

## How it works

pd-memory reads conversations, meetings, coding sessions and documents, and a model turns them into observations. New material is compared with what is already there, so a repeated claim strengthens the existing observation and a later correction replaces the earlier one. Each observation keeps its sources, the evidence it came from, and its correction history.

<p align="center"><img src="assets/log.svg" alt="New entries are appended to log.db. A single worker folds them into memory.db, a graph of pages and observations, which can be deleted and rebuilt from the log." width="760"></p>

Everything that goes into memory is appended to a log first: sources, notes, corrections and forgets. A single worker reads the log in order and builds memory from it. The log is the only thing you need to back up, because memory can be deleted and rebuilt from it. A rebuild makes model calls again, so it costs money, but it loses nothing that was put in.

Closed periods are summarised once (days into weeks, weeks into months), and the current overview is refreshed after enough has changed. All of this happens when new material comes in, so there is no timer and no background service.

It is one Go executable and two SQLite files. Hosts call it as a CLI, and TypeScript hosts get a small typed client. Where it is still weak is when the big picture itself is the important information, because extracted observations tend to lose it.

This is an experimental personal project that I use daily with my own agents. The earlier TypeScript engine remains at tag `v0.1-typescript`.

## Setup

```sh
go build -o pd-memory .
cp config.example.toml /path/outside/git/pd-memory.toml
export PD_MEMORY_CONFIG=/path/outside/git/pd-memory.toml
```

Set the workspace directory, your name, providers and prices in the configuration. Credentials come from environment variables. Check your provider's retention controls before you enable `retention_verified`.

Generation uses either OpenAI directly with Flex, or OpenRouter with zero-retention routing. Embeddings use OpenRouter or a local Perplexity service, with no fallback between the two. Model calls cost money, and `stats` shows recorded usage and estimated cost.

## Use

```sh
pd-memory ingest --path notes.txt --kind document --label Notes --wait
pd-memory brief
pd-memory recall --for "architecture decisions"
pd-memory open ID --history
pd-memory note --line "A durable observation." --page root --actor user --wait
pd-memory help
```

Add `--json` for machine output. `catalog` lists every command's input and result schema with the engine version. `brief --folder PATH` adds context for one project, and `focus` adjusts what that project's brief shows. Reads never call a model or write to memory.

Without `--wait`, ingestion returns once the entry is in the log and reports the queued work. `maintain` and `brief --compose` run maintenance by hand when a workspace has been quiet. `rebuild` keeps the old memory next to the new one.

Separate workspaces are the privacy boundary. There are no read filters.

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

The importer also accepts `--claude-code` and `--codex` paths. It reads completed exchanges, skips subagents, and remembers message ids so the next import only captures new messages.

The [Pi extension](integrations/pi.ts) adds the brief at session start and memory tools. Its settings live in `~/.config/pd-memory/pi.json`:

```json
{"binary":"pd-memory","config":"/absolute/path/to/pd-memory.toml"}
```

Capture defaults to daily imports, and `capture_mode: "live"` captures after every turn.
