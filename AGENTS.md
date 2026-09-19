# Development

- Use Bun and TypeScript. SQLite is the canonical store.
- Keep semantic decisions in model calls and structural invariants in deterministic mutations.
- Define observation fields once. Derive runtime validation, model schemas, and TypeScript types from those definitions.
- Preserve source provenance and correction history. Do not silently change retrieval or authority semantics during refactors.
- Keep modules organized by subsystem. Prefer small, explicit code over speculative infrastructure.
- Keep credentials, runtime databases, private conversations, and evaluation outputs outside this repository.
- Use lightweight verification for ordinary changes. Do not add tests for implementation details or coverage targets.
