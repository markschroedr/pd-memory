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

An agent can search its history, but it needs some idea of what is there in the first place. The top level is meant to read like a dictionary, with short entries that can be opened in more detail.

New material goes through two passes: extraction proposes observations, then integration compares them with existing memory. This is also where repeated information is combined and things that are unlikely to matter a few weeks later are dropped. Pages are for subjects worth returning to, rather than every person or object mentioned in a conversation.

When a claim is replaced, the old version stays in its history. Each claim also records who made it, independently of whether it came from a chat, a meeting, or a document. An agent's suggestion should remain an agent's suggestion until the user actually agrees to it.

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

The model and provider are configurable in `[openai]`, the settings for the OpenAI-compatible Responses API. The default is `gpt-5.6-luna` through direct OpenAI with Flex. OpenRouter is also supported; [config.example.toml](config.example.toml) shows the settings to change, including the allowed providers and pricing. Choose a model that supports strict JSON-schema output.

Embeddings use OpenRouter or a local Perplexity service. Ingestion and searches can incur API costs.

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

## Import coding sessions

The bundled importer reads Pi, Claude Code, and Codex session logs. Point it at the files or folders you use:

```sh
bun scripts/import-sessions.ts \
  --pi ~/.pi/agent/sessions \
  --claude-code ~/.claude/projects \
  --codex ~/.codex/sessions \
  --dry-run
```

Repeat any input flag for more paths. `--dry-run` previews complete conversations without writing to memory or calling a model. Use `--wait` instead to import and process them, or omit both flags to queue them for the worker.

Run the same command daily with cron or your scheduler. Message IDs keep repeat runs from importing the same messages again; unfinished turns wait for the next run. Use one canonical log when tools mirror each other's sessions.

`--routes` accepts project-to-page mappings, `--after` sets a starting date, and `--user-name` sets your speaker label. See `bun scripts/import-sessions.ts --help` for the format. The existing `import-pi-sessions.ts --root ...` command still works.
