# pd-memory

Memory for agents that starts with a map. The agent gets a short overview of everything it knows at the start of a session, and opens the details when it needs them.

<p align="center"><img src="assets/map.svg" alt="A compact brief of headings and one-line entries. One entry opens into its full observation with history and related pages, which links back to the sources it came from." width="760"></p>

Search-only memory fails when the agent doesn't know there is something to search for. "What should I read this weekend?" has no good query; the answer depends on the shape of everything that is there. Dumping everything into context fails the other way. pd-memory sits between them with a dictionary-style overview. Subjects are pages, and below each page are one-line observations:

```text
## billing (project) — Usage-based billing for the platform. (14)
- 3f9a1c [3+] Customers are invoiced monthly from metered usage, and refunds need manual approval. (4; 12 Sep)
- 81d2e0 [2] The free tier was raised to 500 runs after the pricing review. (1; 28 Aug)
### invoicing (artifact) — Invoice generation and delivery. (5)
```

The id opens an entry, the bracket shows prominence, `+` means more detail, and the end shows source count and date. The agent can open entries, follow links to related pages, and check the sources behind a claim.

## How it works

A model turns conversations, meetings, coding sessions and documents into observations. New material is compared with existing memory: a repeated claim strengthens an observation, a later correction replaces it. Each observation keeps its sources, evidence and correction history.

A separate, cheap rating pass scores each observation for reach, surprise, directive, sensitivity and durability, calibrated against the rest of memory. The brief ranks by these ratings. A changed rating prompt re-rates everything on the next run without a rebuild.

<p align="center"><img src="assets/log.svg" alt="New entries are appended to log.db. A single worker folds them into memory.db, a graph of pages and observations, which can be deleted and rebuilt from the log." width="760"></p>

Every input (sources, notes, corrections, forgets) is appended to a log first. A single worker folds the log in order into memory. Back up only the log: memory can be rebuilt from it, at the cost of new model calls. Closed periods are summarised once (days into weeks, weeks into months), and the overview is refreshed after enough has changed. There is no timer and no background service.

It is one Go executable and two SQLite files, called as a CLI. It is weakest when the big picture itself is the important information. This is an experimental personal project that I use daily. The earlier TypeScript engine remains at tag `v0.1-typescript`.

## InMind benchmark

[InMind](https://github.com/imlrz/InMind) ([paper](https://arxiv.org/abs/2607.24368)) tests this case: a user mentions a fact once, such as an allergy, and 38 sessions later asks something that never names it, such as a macaron recipe. Application counts answers that use the fact. All 125 tasks ran with the benchmark's answer model, judge and prompts; the InMind team re-judged the pd-memory answers.

| System | Direct recall | Target recall | Application |
| --- | --- | --- | --- |
| **pd-memory** | **93.6%** | **84.8%** | **70.4%** |
| Naive RAG (text-embedding-3-large) | 97.6% | 6.4% | 16.0% |
| MemoryOS | 96.8% | 7.2% | 14.4% |
| A-Mem | 100.0% | 12.0% | 9.6% |
| HippoRAG 2 | 93.6% | 0.8% | 8.8% |
| Mem0 | 76.8% | 6.4% | 6.4% |
| Fact already in context (control) | | 100.0% | 84.0% |

Other rows are from the [InMind leaderboard](https://keep-it-inmind.github.io/leaderboard/). pd-memory used `gpt-6-luna` to build memory, Perplexity `pplx-embed-v1-0.6b` embeddings (not the OpenAI default below), and about 1.3k to 2.1k tokens of context per question: the brief plus a recall for the question. Letting the agent search on its own instead scored 69.6%. One task failed during ingestion and counts as a miss. Each task builds a small memory, so large memories are not measured here.

## Quickstart

```sh
curl -fsSL https://raw.githubusercontent.com/markschroedr/pd-memory/main/install.sh | sh
pd-memory init
```

`init` asks for your name, a provider and one key, then checks the models live and optionally sets up Pi. The installer writes to `~/.local/bin`; binaries for macOS, Linux and Windows are on [Releases](https://github.com/markschroedr/pd-memory/releases).

## Configuration

- **OpenAI** (default): `OPENAI_API_KEY`, `gpt-6-luna` (Flex, `store=false`) and `text-embedding-3-small` at 1024 dimensions. Without zero data retention, init requires explicit approval of OpenAI's standard retention; declining writes nothing.
- **OpenRouter**: `OPENROUTER_API_KEY`, `openai/gpt-6-luna` through the Azure ZDR route only, plus Perplexity embeddings. ZDR is required and fallback disabled.

Config lives in `~/.config/pd-memory/pd-memory.toml` (override with `PD_MEMORY_CONFIG` or `--config`). Credentials live in a separate private file; environment variables override them. Providers can be mixed, including a local Perplexity embedding service; keep `[pricing]` aligned, and rebuild vectors if you change a workspace's embedding model. `doctor` checks setup; `stats` shows usage and estimated cost.

## Use

```sh
pd-memory ingest --path notes.txt --kind document --label Notes --wait
pd-memory brief
pd-memory recall --query "architecture decisions"
pd-memory open ID --history
pd-memory note --line "A durable observation." --page root --actor user --wait
pd-memory help
```

Add `--json` for machine output. `catalog` lists every command's schemas. `brief --folder PATH` adds one project's context; `focus` adjusts what it shows. Without `--wait`, writes return once logged. `rebuild` keeps the old memory next to the new one. Reads need no key: without embeddings, `recall` returns keyword matches and says so. Separate workspaces are the privacy boundary; there are no read filters.

TypeScript hosts use `MemoryClient` from `integrations/client.ts`. After command changes, regenerate types with `pd-memory catalog --typescript > integrations/types.ts`.

## Sources and sync

A **source** is a configured location with an adapter (`pi`, `claude-code`, `codex`, `files`, `sqlite`). A **unit** is one session, file or row. A **cursor** records what was imported. Nothing is imported from a unit until it has been inactive for `settle_after` (default 3 hours).

```sh
pd-memory sync --dry-run
pd-memory sync                 # queue settled increments without waiting
pd-memory sync --source notes --wait
```

Sync is idempotent. Session adapters keep imported message ids across branches and resumes. Set `after` before the first sync to skip old history. Excludes are globs relative to the source root; `**` matches any depth.

```toml
settle_after = "3h"
auto_sync = true

[[sources]]
name = "notes"
adapter = "files"            # .txt and .md; appends import only the new text
path = "~/notes"
kind = "document"
settle_after = "1h"
exclude = ["**/drafts/**"]

[[sources]]
name = "meetings"
adapter = "sqlite"
path = "~/meetings.sqlite"
kind = "meeting"
query = "SELECT id AS unit_id, updated_at AS time, transcript AS text, title FROM meetings"

[[routes]]
root = "~/projects/example"
home = "example"
```

The SQLite query must return unique `unit_id`, RFC3339 `time` (last activity) and `text`; `speaker` and `title` are optional. In both adapters, a rewritten prefix is reported, not re-imported.

Routes map folders to home pages by longest matching root. Unrouted folders use their last folder name; if it cannot form a page slug, sync reports the unit under `unrouted`, leaves its cursor alone, and continues. When a unit resumes, extraction sees digests of its earlier parts as read-only context.

## Pi

pd-memory is built for coding agents. It ships one live integration, this Pi extension, as a reference for other hosts. Claude Code and Codex sessions can only be imported as sources; they get no tools or brief.

The [Pi package](integrations/pi.ts) adds `memory_recall`, `memory_brief`, `memory_open`, `memory_note` and `memory_focus`; notes have agent authority. At session start it runs `pd-memory sync --auto` in the background (disable with `auto_sync = false`). Before each agent run it adds the current folder's brief as a `memory_brief` system context block. If the memory tools are deactivated, it injects nothing. The binary must be on Pi's `PATH`. Do not combine it with another host's memory injection.

For local development: `pi install /path/to/pd-memory`.

## License

Copyright (c) 2026 Mark Schröder.

pd-memory is licensed under the [GNU Affero General Public License v3.0](LICENSE). If you run a modified version as a network service, you must offer its source to its users. A commercial license is available on request: mark@schroedermark.com.

Commits before this license change remain available under the MIT License.
