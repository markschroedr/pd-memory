package importer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/markschroedr/pd-memory/internal/config"
	inputlog "github.com/markschroedr/pd-memory/internal/log"
	"github.com/markschroedr/pd-memory/internal/model"
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
		if _, ok := object(h["source"])["subagent"]; ok {
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

type Routes struct {
	CaptureAfter string `json:"capture_after,omitempty"`
	Projects     []struct {
		Root string `json:"root"`
		Home string `json:"home"`
	} `json:"projects"`
}
type Options struct {
	Pi         []string `json:"pi,omitempty"`
	ClaudeCode []string `json:"claude_code,omitempty"`
	Codex      []string `json:"codex,omitempty"`
	Routes     string   `json:"routes,omitempty"`
	After      string   `json:"after,omitempty"`
	UserName   string   `json:"user_name,omitempty"`
	DryRun     bool     `json:"dry_run,omitempty"`
	Wait       bool     `json:"wait,omitempty"`
}
type Result struct {
	Files               int     `json:"files"`
	Submitted           int     `json:"submitted"`
	Reused              int     `json:"reused"`
	Skipped             int     `json:"skipped"`
	Messages            int     `json:"messages"`
	Characters          int     `json:"characters"`
	UserCharacters      int     `json:"userCharacters"`
	AssistantCharacters int     `json:"assistantCharacters"`
	Seqs                []int64 `json:"seqs"`
	DryRun              bool    `json:"dry_run"`
}

var runSession = regexp.MustCompile(`/run-[^/]+/session\.jsonl$`)
var slugChars = regexp.MustCompile(`[^a-z0-9]+`)

func Run(c *config.Config, l *inputlog.Store, o Options) (Result, error) {
	out := Result{Seqs: []int64{}, DryRun: o.DryRun}
	if o.Wait && o.DryRun {
		return out, fmt.Errorf("choose wait or dry-run")
	}
	var routes Routes
	if o.Routes != "" {
		b, e := os.ReadFile(o.Routes)
		if e != nil {
			return out, e
		}
		if e = model.Decode(b, &routes); e != nil {
			return out, e
		}
	}
	after := o.After
	if after == "" {
		after = routes.CaptureAfter
	}
	var cutoff time.Time
	var e error
	if after != "" {
		cutoff, e = time.Parse(time.RFC3339Nano, after)
		if e != nil {
			return out, e
		}
	}
	user := o.UserName
	if user == "" && len(c.Identity.UserNames) > 0 {
		user = c.Identity.UserNames[0]
	}
	if user == "" {
		user = "User"
	}
	files := map[string]string{}
	inputs := map[string][]string{"pi": o.Pi, "claude-code": o.ClaudeCode, "codex": o.Codex}
	count := 0
	for provider, paths := range inputs {
		for _, p := range paths {
			count++
			p, e = filepath.EvalSymlinks(p)
			if e != nil {
				return out, e
			}
			p, e = filepath.Abs(p)
			if e != nil {
				return out, e
			}
			info, e := os.Stat(p)
			if e != nil {
				return out, e
			}
			candidates := []string{}
			if !info.IsDir() {
				candidates = append(candidates, p)
			} else {
				e = filepath.WalkDir(p, func(path string, d os.DirEntry, err error) error {
					if err != nil {
						return err
					}
					if !d.IsDir() && strings.HasSuffix(path, ".jsonl") {
						candidates = append(candidates, path)
					}
					return nil
				})
				if e != nil {
					return out, e
				}
			}
			for _, path := range candidates {
				path, e = filepath.EvalSymlinks(path)
				if e != nil {
					return out, e
				}
				norm := filepath.ToSlash(path)
				if strings.Contains(norm, "/subagents/") || strings.Contains(norm, "/subagent-artifacts/") || (provider == "pi" && runSession.MatchString(norm)) || (provider == "claude-code" && strings.HasPrefix(filepath.Base(path), "agent-")) {
					continue
				}
				if old, ok := files[path]; ok && old != provider {
					return out, fmt.Errorf("same file assigned to two formats")
				}
				files[path] = provider
			}
		}
	}
	if count == 0 {
		return out, fmt.Errorf("supply at least one session path")
	}
	out.Files = len(files)
	type prepared struct {
		Source   inputlog.Source
		Messages int
		Dialogue Dialogue
	}
	sources := []prepared{}
	paths := []string{}
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		s, e := Parse(p, files[p])
		if e != nil {
			return out, e
		}
		if s == nil {
			out.Skipped++
			continue
		}
		captured := map[string]bool{}
		if l != nil {
			ids, e := l.Captured(s.ID)
			if e != nil {
				return out, e
			}
			for _, id := range ids {
				captured[id] = true
			}
		}
		messages := []Message{}
		hasUser := false
		for _, m := range s.Messages {
			at, _ := time.Parse(time.RFC3339Nano, m.Timestamp)
			if captured[m.ID] || (!cutoff.IsZero() && at.Before(cutoff)) {
				continue
			}
			if m.Role == "user" {
				m.Speaker = user
				hasUser = true
			} else {
				m.Speaker = "Agent"
			}
			messages = append(messages, m)
		}
		if !hasUser {
			out.Skipped++
			continue
		}
		d, e := Prepare(messages)
		if e != nil {
			return out, e
		}
		first, last := messages[0], messages[len(messages)-1]
		home, e := homeFor(routes, s.CWD)
		if e != nil {
			return out, e
		}
		ids := []string{}
		for _, m := range messages {
			ids = append(ids, m.ID)
		}
		external := s.ID + ":" + last.ID
		if files[p] == "pi" {
			external = "pi:" + external
		}
		date := first.Timestamp
		labelProvider := files[p]
		if labelProvider == "pi" {
			labelProvider = "Pi"
		}
		sources = append(sources, prepared{inputlog.Source{Kind: "coding_session", Text: d.Text, Label: fmt.Sprintf("%s session %s: %s through %s", labelProvider, s.ID, first.ID, last.ID), Happened: &date, Session: &s.ID, Home: &home, ExternalID: &external, Metadata: map[string]any{"source": files[p], "capture": "daily", "cwd": s.CWD, "primary_jsonl": p, "entry_ids": ids, "first_entry_id": first.ID, "through_entry_id": last.ID, "dialogue": d.Stats}, Participants: d.Participants}, len(messages), d})
	}
	sort.SliceStable(sources, func(i, j int) bool { return *sources[i].Source.Happened < *sources[j].Source.Happened })
	for _, p := range sources {
		if l != nil {
			r, e := l.Append("source", "system", p.Source)
			if e != nil {
				return out, e
			}
			out.Seqs = append(out.Seqs, r.Seq)
			if r.Reused {
				out.Reused++
			} else {
				out.Submitted++
			}
		}
		out.Messages += p.Messages
		out.Characters += len(p.Dialogue.Text)
		out.UserCharacters += p.Dialogue.Stats.UserChars
		out.AssistantCharacters += p.Dialogue.Stats.AssistantChars
	}
	return out, nil
}
func homeFor(routes Routes, cwd string) (string, error) {
	best, home := 0, ""
	path, e := filepath.Abs(cwd)
	if e != nil {
		return "", e
	}
	for _, r := range routes.Projects {
		root, e := filepath.Abs(r.Root)
		if e != nil {
			return "", e
		}
		rel, e := filepath.Rel(root, path)
		if e != nil {
			return "", e
		}
		if (rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".." && !filepath.IsAbs(rel))) && len(root) > best {
			best = len(root)
			home = r.Home
		}
	}
	if home != "" {
		return home, nil
	}
	leaf := strings.Trim(slugChars.ReplaceAllString(strings.ToLower(filepath.Base(cwd)), "-"), "-")
	if len(leaf) >= 2 && len(leaf) <= 64 && leaf[0] >= 'a' && leaf[0] <= 'z' {
		return leaf, nil
	}
	return "root", nil
}
