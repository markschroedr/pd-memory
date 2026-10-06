package importer

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/markschroedr/pd-memory/internal/config"
)

type Session struct {
	ID       string
	CWD      string
	Messages []Message
}
type row map[string]any

func object(v any) row {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return row{}
}
func text(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
func content(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	parts := []string{}
	if xs, ok := v.([]any); ok {
		for _, x := range xs {
			b := object(x)
			if config.Contains([]string{"text", "input_text", "output_text"}, text(b["type"])) {
				parts = append(parts, text(b["text"]))
			}
		}
	}
	return strings.Join(parts, "\n")
}
func branch(rows []row, id, parent string) ([]row, error) {
	by := map[string]row{}
	var current row
	for _, r := range rows {
		if text(r[id]) != "" {
			by[text(r[id])] = r
			current = r
		}
	}
	out := []row{}
	seen := map[string]bool{}
	for current != nil {
		k := text(current[id])
		if seen[k] {
			return nil, fmt.Errorf("session branch cycle at %s", k)
		}
		seen[k] = true
		out = append(out, current)
		p := text(current[parent])
		if p != "" && by[p] == nil {
			return nil, fmt.Errorf("missing session parent %s", p)
		}
		current = by[p]
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}
func Parse(path, provider string) (*Session, error) {
	b, e := os.ReadFile(path)
	if e != nil {
		return nil, e
	}
	lines := strings.Split(string(b), "\n")
	rows := []row{}
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var r row
		if e = json.Unmarshal([]byte(line), &r); e != nil {
			if i == len(lines)-1 && !strings.HasSuffix(string(b), "\n") {
				break
			}
			return nil, fmt.Errorf("%s:%d: %w", path, i+1, e)
		}
		if r == nil {
			return nil, fmt.Errorf("JSONL row must be object")
		}
		rows = append(rows, r)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	s := &Session{Messages: []Message{}}
	add := func(id, role, ts, body string, complete bool) error {
		if id == "" {
			return fmt.Errorf("session message has no stable ID")
		}
		t, e := time.Parse(time.RFC3339Nano, ts)
		if e != nil {
			return e
		}
		s.Messages = append(s.Messages, Message{ID: id, Role: role, Timestamp: t.UTC().Format(time.RFC3339Nano), Text: body, Complete: complete})
		return nil
	}
	if provider == "pi" {
		h := rows[0]
		if h["type"] != "session" || text(h["id"]) == "" || text(h["cwd"]) == "" {
			return nil, nil
		}
		s.ID = text(h["id"])
		s.CWD = text(h["cwd"])
		br, e := branch(rows[1:], "id", "parentId")
		if e != nil {
			return nil, e
		}
		for _, r := range br {
			if r["type"] != "message" {
				continue
			}
			v := object(r["message"])
			role := text(v["role"])
			if role != "user" && role != "assistant" {
				continue
			}
			if role == "assistant" && v["stopReason"] != "stop" {
				continue
			}
			body := content(v["content"])
			if strings.TrimSpace(body) != "" {
				if e = add(text(r["id"]), role, text(r["timestamp"]), body, role == "assistant"); e != nil {
					return nil, e
				}
			}
		}
	} else if provider == "claude-code" {
		entries := []row{}
		for _, r := range rows {
			if r["isSidechain"] != true {
				entries = append(entries, r)
				if s.ID == "" && text(r["sessionId"]) != "" && text(r["cwd"]) != "" {
					s.ID = "claude-code:" + text(r["sessionId"])
					s.CWD = text(r["cwd"])
				}
			}
		}
		if s.ID == "" {
			return nil, nil
		}
		br, e := branch(entries, "uuid", "parentUuid")
		if e != nil {
			return nil, e
		}
		for _, r := range br {
			if !config.Contains([]string{"user", "assistant"}, text(r["type"])) || r["isMeta"] == true || r["isCompactSummary"] == true || r["isApiErrorMessage"] == true {
				continue
			}
			v := object(r["message"])
			role := text(v["role"])
			tool := false
			if blocks, ok := v["content"].([]any); ok {
				for _, b := range blocks {
					if object(b)["type"] == "tool_result" {
						tool = true
					}
				}
			}
			if role == "user" && tool {
				continue
			}
			if role != "user" && role != "assistant" {
				continue
			}
			body := content(v["content"])
			if strings.TrimSpace(body) == "" {
				continue
			}
			if role == "user" && len(s.Messages) > 0 && s.Messages[len(s.Messages)-1].Role == "assistant" {
				s.Messages[len(s.Messages)-1].Complete = true
			}
			stop := text(v["stop_reason"])
			if role == "assistant" && stop != "" && !config.Contains([]string{"end_turn", "stop_sequence"}, stop) {
				continue
			}
			if e = add(text(r["uuid"]), role, text(r["timestamp"]), body, config.Contains([]string{"end_turn", "stop_sequence"}, stop)); e != nil {
				return nil, e
			}
		}
	} else {
		var h row
		for _, r := range rows {
			if r["type"] == "session_meta" {
				h = object(r["payload"])
				break
			}
		}
		if text(h["id"]) == "" || text(h["cwd"]) == "" {
			return nil, nil
		}
		s.ID = "codex:" + text(h["id"])
		s.CWD = text(h["cwd"])
		for i, r := range rows {
			v := object(r["payload"])
			if r["type"] == "event_msg" && v["type"] == "task_complete" {
				if len(s.Messages) > 0 && s.Messages[len(s.Messages)-1].Role == "assistant" {
					s.Messages[len(s.Messages)-1].Complete = true
				}
				continue
			}
			if r["type"] != "response_item" || v["type"] != "message" {
				continue
			}
			role := text(v["role"])
			if role != "user" && role != "assistant" {
				continue
			}
			phase := text(v["phase"])
			if role == "assistant" && phase != "" && !config.Contains([]string{"final", "final_answer"}, phase) {
				continue
			}
			body := content(v["content"])
			if strings.TrimSpace(body) == "" {
				continue
			}
			if role == "user" && len(s.Messages) > 0 && s.Messages[len(s.Messages)-1].Role == "assistant" {
				s.Messages[len(s.Messages)-1].Complete = true
			}
			id := text(v["id"])
			if id == "" {
				id = fmt.Sprintf("row:%d", i+1)
			}
			if e = add(id, role, text(r["timestamp"]), body, role == "assistant" && config.Contains([]string{"final", "final_answer"}, phase)); e != nil {
				return nil, e
			}
		}
	}
	for i := len(s.Messages) - 1; i >= 0; i-- {
		if s.Messages[i].Role == "assistant" && s.Messages[i].Complete {
			s.Messages = s.Messages[:i+1]
			return s, nil
		}
	}
	s.Messages = []Message{}
	return s, nil
}

var slugChars = regexp.MustCompile(`[^a-z0-9]+`)
