package integration

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/markschroedr/pd-memory/internal/fold"

	_ "modernc.org/sqlite"
)

// Real CLI boundary: incomplete turns or replayed message IDs silently lose or duplicate source material.
// Keep this single importer journey; the expected transcript tokens are independent of the parser.
func TestSessionCaptureExactlyOnce(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("/tmp", "pd-memory-import-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "pd-memory")
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Dir = root
	if out, e := build.CombinedOutput(); e != nil {
		t.Fatalf("build: %v %s", e, out)
	}
	workspace := filepath.Join(dir, "workspace")
	config := filepath.Join(dir, "config.toml")
	base := fmt.Sprintf("settle_after = \"1ms\"\n[workspace]\ndir = %q\n[compose]\nmode = \"off\"\n", workspace)
	at := "2026-01-01T00:00:00Z"
	pi := func(id, parent, role, text string) map[string]any {
		return map[string]any{"type": "message", "id": id, "parentId": parent, "timestamp": at, "message": map[string]any{"role": role, "content": []any{map[string]any{"type": "text", "text": text}, map[string]any{"type": "thinking", "thinking": "DO_NOT_IMPORT"}}, "stopReason": "stop"}}
	}
	claude := func(id, parent, role, text string) map[string]any {
		return map[string]any{"type": role, "uuid": id, "parentUuid": parent, "timestamp": at, "sessionId": "shared", "cwd": "/example/project", "message": map[string]any{"role": role, "content": []any{map[string]any{"type": "text", "text": text}}, "stop_reason": "end_turn"}}
	}
	codex := func(id, role, text string) map[string]any {
		return map[string]any{"type": "response_item", "timestamp": at, "payload": map[string]any{"type": "message", "id": id, "role": role, "phase": "final_answer", "content": []any{map[string]any{"type": "text", "text": text}}}}
	}
	fixtures := []struct {
		provider string
		rows     []any
		final    any
	}{
		{"pi", []any{map[string]any{"type": "session", "id": "shared", "cwd": "/example/project"}, pi("u1", "", "user", "pi-FIRST"), pi("abandoned", "u1", "assistant", "DO_NOT_IMPORT"), pi("a1", "u1", "assistant", "pi-REPLY1"), pi("u2", "a1", "user", "pi-SECOND")}, pi("a2", "u2", "assistant", "pi-REPLY2")},
		{"claude-code", []any{claude("u1", "", "user", "claude-code-FIRST"), map[string]any{"type": "user", "uuid": "tool", "parentUuid": "u1", "timestamp": at, "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "content": "DO_NOT_IMPORT"}}}}, claude("a1", "tool", "assistant", "claude-code-REPLY1"), claude("u2", "a1", "user", "claude-code-SECOND")}, claude("a2", "u2", "assistant", "claude-code-REPLY2")},
		{"codex", []any{map[string]any{"type": "session_meta", "payload": map[string]any{"id": "shared", "cwd": "/example/project"}}, codex("u1", "user", "codex-FIRST"), map[string]any{"type": "event_msg", "timestamp": at, "payload": map[string]any{"type": "user_message", "message": "DO_NOT_IMPORT"}}, codex("a1", "assistant", "codex-REPLY1"), codex("u2", "user", "codex-SECOND")}, codex("a2", "assistant", "codex-REPLY2")},
	}
	run := func(provider, path string, flags ...string) map[string]any {
		if err := os.WriteFile(config, []byte(base+fmt.Sprintf("\n[[sources]]\nname = %q\nadapter = %q\npath = %q\nkind = \"coding_session\"\n", provider, provider, path)), 0600); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-time.Hour)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
		args := append([]string{"sync", "--config", config, "--source", provider, "--json"}, flags...)
		cmd := exec.Command(binary, args...)
		cmd.Env = append(os.Environ(), "OPENAI_API_KEY=", "OPENROUTER_API_KEY=")
		out, e := cmd.Output()
		if e != nil {
			t.Fatalf("import: %v %s", e, out)
		}
		var r struct {
			Sync map[string]any `json:"sync"`
		}
		if e = json.Unmarshal(out, &r); e != nil {
			t.Fatal(e)
		}
		return r.Sync
	}
	if err = os.MkdirAll(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	lock, err := fold.Lock(workspace, true)
	if err != nil {
		t.Fatal(err)
	}
	defer fold.Unlock(lock)
	for _, fixture := range fixtures {
		path := filepath.Join(dir, fixture.provider+".jsonl")
		parts := []string{}
		for _, r := range fixture.rows {
			b, _ := json.Marshal(r)
			parts = append(parts, string(b))
		}
		final, _ := json.Marshal(fixture.final)
		prefix := final[:12]
		if err = os.WriteFile(path, []byte(strings.Join(parts, "\n")+"\n"+string(prefix)), 0600); err != nil {
			t.Fatal(err)
		}
		if r := run(fixture.provider, path, "--dry-run"); r["messages"] != float64(2) {
			t.Fatalf("dry-run retained incomplete exchange: %v", r)
		}
		if fixture.provider == "pi" {
			if _, e := os.Stat(filepath.Join(workspace, "log.db")); !os.IsNotExist(e) {
				t.Fatal("dry-run created canonical log")
			}
		}
		if r := run(fixture.provider, path); r["submitted"] != float64(1) {
			t.Fatalf("first capture: %v", r)
		}
		if r := run(fixture.provider, path); r["messages"] != float64(0) {
			t.Fatalf("duplicate capture: %v", r)
		}
		f, e := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
		if e != nil {
			t.Fatal(e)
		}
		_, e = f.Write(append(final[12:], '\n'))
		f.Close()
		if e != nil {
			t.Fatal(e)
		}
		if r := run(fixture.provider, path); r["submitted"] != float64(1) {
			t.Fatalf("second complete capture: %v", r)
		}
		if r := run(fixture.provider, path); r["messages"] != float64(0) {
			t.Fatalf("second duplicate capture: %v", r)
		}
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(workspace, "log.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query("SELECT json_extract(payload,'$.text'),json_extract(payload,'$.session') FROM entries ORDER BY seq")
	if err != nil {
		t.Fatal(err)
	}
	all := []string{}
	sessions := map[string]bool{}
	for rows.Next() {
		var text, session string
		if err = rows.Scan(&text, &session); err != nil {
			t.Fatal(err)
		}
		all = append(all, text)
		sessions[session] = true
	}
	rows.Close()
	if len(all) != 6 || len(sessions) != 3 {
		t.Fatalf("source counts: %d, namespaces: %d", len(all), len(sessions))
	}
	stored := strings.Join(all, "\n")
	if strings.Contains(stored, "DO_NOT_IMPORT") {
		t.Fatal("noncanonical content imported")
	}
	for _, fixture := range fixtures {
		for _, token := range []string{"FIRST", "SECOND", "REPLY1", "REPLY2"} {
			if strings.Count(stored, fixture.provider+"-"+token) != 1 {
				t.Fatalf("missing or repeated %s-%s", fixture.provider, token)
			}
		}
	}
	artifact, _ := json.MarshalIndent(map[string]any{"sources": all, "sessions": sessions}, "", "  ")
	if err = os.WriteFile(filepath.Join(dir, "capture.json"), artifact, 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("CLI capture artifact: %s", filepath.Join(dir, "capture.json"))
}
