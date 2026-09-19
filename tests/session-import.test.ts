import { expect, test } from "bun:test";
import { Database } from "bun:sqlite";
import { appendFileSync, existsSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";

// Real CLI + SQLite boundary: duplicate imports, incomplete tails, and provider collisions
// would silently corrupt memory. Fixtures contain invented text only; no model calls.
test("imports completed Pi, Claude Code and Codex exchanges exactly once across appends", () => {
  const root = resolve(import.meta.dir, "..");
  const temp = mkdtempSync(join(tmpdir(), "pd-memory-session-test-"));
  const dbPath = join(temp, "memory.sqlite");
  const config = join(temp, "config.toml");
  writeFileSync(config, readFileSync(join(root, "config.example.toml"), "utf8").replace('"/absolute/path/to/runtime/memory.sqlite"', JSON.stringify(dbPath)));
  const at = "2026-01-01T00:00:00.000Z";
  const pi = (id: string, parentId: string | null, role: string, value: string) => ({ type: "message", id, parentId, timestamp: at,
    message: { role, content: [{ type: "text", text: value }, { type: "thinking", thinking: "DO_NOT_IMPORT" }], stopReason: "stop" } });
  const claude = (uuid: string, parentUuid: string | null, role: string, value: string) => ({ type: role, uuid, parentUuid, timestamp: at,
    sessionId: "shared", cwd: "/example/project", isSidechain: false, message: { role, content: [{ type: "text", text: value }], stop_reason: role === "assistant" ? "end_turn" : null } });
  const codex = (id: string, role: string, value: string) => ({ type: "response_item", timestamp: at,
    payload: { type: "message", id, role, phase: role === "assistant" ? "final_answer" : undefined, content: [{ type: role === "user" ? "input_text" : "output_text", text: value }] } });
  const fixtures = [
    { provider: "pi", rows: [{ type: "session", id: "shared", cwd: "/example/project", version: 3 }, pi("u1", null, "user", "pi-FIRST"),
        pi("abandoned", "u1", "assistant", "DO_NOT_IMPORT"), pi("a1", "u1", "assistant", "pi-REPLY1"), pi("u2", "a1", "user", "pi-SECOND")], final: pi("a2", "u2", "assistant", "pi-REPLY2") },
    { provider: "claude-code", rows: [claude("u1", null, "user", "claude-code-FIRST"),
        { ...claude("tool", "u1", "user", "DO_NOT_IMPORT"), message: { role: "user", content: [{ type: "tool_result", content: "DO_NOT_IMPORT" }] } },
        claude("a1", "tool", "assistant", "claude-code-REPLY1"), claude("u2", "a1", "user", "claude-code-SECOND")], final: claude("a2", "u2", "assistant", "claude-code-REPLY2") },
    { provider: "codex", rows: [{ type: "session_meta", payload: { id: "shared", cwd: "/example/project" } }, codex("u1", "user", "codex-FIRST"),
        { type: "event_msg", timestamp: at, payload: { type: "user_message", message: "DO_NOT_IMPORT" } },
        { type: "response_item", timestamp: at, payload: { type: "function_call_output", output: "DO_NOT_IMPORT" } },
        codex("a1", "assistant", "codex-REPLY1"), codex("u2", "user", "codex-SECOND")], final: codex("a2", "assistant", "codex-REPLY2") },
  ];
  const run = (provider: string, path: string, options: string[] = [], legacy = false) => {
    const result = Bun.spawnSync([process.execPath, join(root, "scripts", legacy ? "import-pi-sessions.ts" : "import-sessions.ts"),
      "--config", config, legacy ? "--root" : `--${provider}`, path, ...options], {
      env: { ...process.env, OPENAI_API_KEY: "", OPENROUTER_API_KEY: "" },
    });
    expect(result.exitCode, result.stderr.toString()).toBe(0);
    return JSON.parse(result.stdout.toString());
  };
  for (const fixture of fixtures) {
    const path = join(temp, `${fixture.provider}.jsonl`);
    const final = JSON.stringify(fixture.final);
    const prefix = final.slice(0, 12);
    writeFileSync(path, fixture.rows.map(row => JSON.stringify(row)).join("\n") + "\n" + prefix);
    const preview = run(fixture.provider, path, ["--dry-run"]);
    expect(preview.messages).toBe(2);
    if (fixture.provider === "pi") expect(existsSync(dbPath)).toBe(false);
    expect(run(fixture.provider, path, [], fixture.provider === "pi").submitted).toBe(1);
    expect(run(fixture.provider, path, [`--${fixture.provider}`, path]).messages).toBe(0);
    appendFileSync(path, final.slice(prefix.length) + "\n");
    expect(run(fixture.provider, path).submitted).toBe(1);
    expect(run(fixture.provider, path).messages).toBe(0);
  }
  const db = new Database(dbPath, { readonly: true });
  try {
    const rows = db.query<{ text: string; session: string }, []>("SELECT text,session FROM sources").all();
    expect(rows).toHaveLength(6);
    expect(new Set(rows.map(row => row.session)).size).toBe(3);
    const stored = rows.map(row => row.text).join("\n");
    expect(stored).not.toContain("DO_NOT_IMPORT");
    for (const { provider } of fixtures) for (const token of ["FIRST", "SECOND", "REPLY1", "REPLY2"]) {
      expect(stored.split(`${provider}-${token}`).length - 1).toBe(1);
    }
  } finally { db.close(); }
}, 30000);
