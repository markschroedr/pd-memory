# pd-memory

Give agents an overview of what they know, then let them look up details when needed.

pd-memory turns conversations and documents into memory organized by topic. The goal is to help an agent notice relevant past decisions and context, even when you haven't explicitly asked it to remember something.

Experimental personal project, extracted from my daily agent setup.

## How it works

1. Read conversations and documents.
2. Keep useful facts and decisions, grouped by topic.
3. Update existing memory as new information arrives.
4. Give the agent a short overview, with tools to search and open more detail.

Each memory links back to its sources. Corrections keep the earlier version and its history.

Built with Bun, TypeScript, and SQLite. Use it through the CLI or the `PdMemoryRuntime` library.

## Design

Memory should read like a dictionary: each entry gives just enough to decide whether to zoom in. Pages are places to navigate to, not tags for every entity mentioned in a conversation.

Extraction proposes knowledge; integration compares it with existing memory and decides what to keep or change. Being true is not enough to keep a statement—it should still be useful several weeks later.

When a claim's meaning changes, supersede it instead of silently overwriting it, so the correction has a history. Authority belongs to each claim, not the source format: an assistant's proposal in a chat log is not the user's decision.

In a three-question pilot, pd-memory and chunk-only retrieval both scored 3/3. The harder question is whether the overview helps an agent notice relevant history without being told to search.

## Pi

The [Pi extension](integrations/pi.ts) adds a standing overview at session start and tools to search, open, and add memories; its settings live in `~/.config/pd-memory/pi.json`.

## Setup

Requires Bun 1.3+.

```sh
bun install --frozen-lockfile
mkdir -p ~/.config/pd-memory
export PD_MEMORY_CONFIG="$HOME/.config/pd-memory/pd-memory.toml"
cp -n config.example.toml "$PD_MEMORY_CONFIG"
export OPENAI_API_KEY="..."
export OPENROUTER_API_KEY="..."
```

Edit the configuration to set your database path, models, and identities. Review your provider's data-retention policy before setting `retention_verified=true`.

Text processing uses OpenAI. Embeddings use OpenRouter or a local Perplexity service. Ingestion and searches can incur API costs.

## Use

```sh
# Add a document.
bun src/cli.ts ingest --path notes.txt --kind document --label "Notes" --wait

# Get an overview, find related memory, and open a result.
bun src/cli.ts brief
bun src/cli.ts search --query "project decisions"
bun src/cli.ts open ID

# See all commands.
bun src/cli.ts help
```

Replace `ID` with an ID from the results. Add `--json` for structured output.
