package importer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/markschroedr/pd-memory/internal/config"
	"github.com/markschroedr/pd-memory/internal/fold"
	inputlog "github.com/markschroedr/pd-memory/internal/log"
)

// Only an adapter interprets a cursor. The log preserves it with the imported increment.
type cursor = json.RawMessage
type unit struct {
	ID, Path, Folder, Title string
	Activity                time.Time
	data                    any
}
type increment struct {
	Text      string
	Speaker   string
	Messages  []Message
	Cursor    cursor
	Metadata  map[string]any
	Happened  string
	LastID    string
	Rewritten bool
}
type adapter interface {
	Units(config.Source) ([]unit, error)
	Cursor([]inputlog.Source) (cursor, error)
	Read(unit, cursor, time.Time) (increment, error)
}
type Options struct {
	Source string `json:"source,omitempty"`
	Auto   bool   `json:"auto,omitempty"`
	DryRun bool   `json:"dry_run,omitempty"`
	Wait   bool   `json:"wait,omitempty"`
}
type Result struct {
	Units     int      `json:"units"`
	Submitted int      `json:"submitted"`
	Skipped   int      `json:"skipped"`
	Messages  int      `json:"messages"`
	Rewritten []string `json:"rewritten"`
	Seqs      []int64  `json:"seqs"`
	DryRun    bool     `json:"dry_run"`
	Disabled  bool     `json:"disabled,omitempty"`
}

func Run(c *config.Config, l *inputlog.Store, o Options) (Result, error) {
	out := Result{Seqs: []int64{}, Rewritten: []string{}, DryRun: o.DryRun}
	if o.Auto && !c.AutoSync {
		out.Disabled = true
		return out, nil
	}
	if o.Wait && o.DryRun {
		return out, fmt.Errorf("choose wait or dry-run")
	}
	// Two callers must not render overlapping increments before either publishes its cursor.
	var lock *os.File
	var err error
	if !o.DryRun {
		lock, err = fold.LockFile(c.Workspace.Dir, "sync.lock", true)
		if err != nil {
			return out, err
		}
		defer fold.Unlock(lock)
	}
	history := map[string][]inputlog.Source{}
	if l != nil {
		entries, e := l.Entries()
		if e != nil {
			return out, e
		}
		for _, entry := range entries {
			if entry.Kind != "source" {
				continue
			}
			var src inputlog.Source
			if e = json.Unmarshal(entry.Payload, &src); e != nil {
				return out, e
			}
			if src.Session != nil {
				history[*src.Session] = append(history[*src.Session], src)
			}
		}
	}
	type prepared struct {
		src      inputlog.Source
		messages int
	}
	pending := []prepared{}
	matched := o.Source == ""
	now := time.Now()
	for _, s := range c.Sources {
		if o.Source != "" && o.Source != s.Name {
			continue
		}
		matched = true
		a, e := adapterFor(s.Adapter)
		if e != nil {
			return out, e
		}
		units, e := a.Units(s)
		if e != nil {
			return out, fmt.Errorf("source %s: %w", s.Name, e)
		}
		settle := c.SettleAfter
		if s.SettleAfter != "" {
			settle = s.SettleAfter
		}
		duration, _ := time.ParseDuration(settle)
		cutoff := time.Time{}
		if s.After != "" {
			cutoff, _ = time.Parse(time.RFC3339Nano, s.After)
		}
		for _, u := range units {
			out.Units++
			if now.Sub(u.Activity) < duration {
				out.Skipped++
				continue
			}
			cur, e := a.Cursor(history[u.ID])
			if e != nil {
				return out, e
			}
			inc, e := a.Read(u, cur, cutoff)
			if e != nil {
				return out, fmt.Errorf("unit %s: %w", u.ID, e)
			}
			if inc.Rewritten {
				out.Rewritten = append(out.Rewritten, u.Path)
				out.Skipped++
				continue
			}
			participants := []string{}
			if inc.Speaker != "" && strings.TrimSpace(inc.Text) != "" {
				inc.Text = inc.Speaker + ":\n" + inc.Text
			}
			if len(inc.Messages) > 0 {
				user := "User"
				if len(c.Identity.UserNames) > 0 {
					user = c.Identity.UserNames[0]
				}
				for i := range inc.Messages {
					if inc.Messages[i].Role == "user" {
						inc.Messages[i].Speaker = user
					} else {
						inc.Messages[i].Speaker = "Agent"
					}
				}
				d, e := Prepare(inc.Messages)
				if e != nil {
					return out, e
				}
				inc.Text = d.Text
				participants = d.Participants
				inc.Metadata["dialogue"] = d.Stats
			} else if s.Kind == "meeting" || s.Kind == "conversation" {
				participants = Speakers(inc.Text)
			}
			if strings.TrimSpace(inc.Text) == "" {
				out.Skipped++
				continue
			}
			home, e := homeFor(c.Routes, u.Folder)
			if e != nil {
				return out, e
			}
			external := u.ID + ":" + inc.LastID
			if s.Adapter == "pi" {
				external = "pi:" + external
			}
			metadata := inc.Metadata
			if metadata == nil {
				metadata = map[string]any{}
			}
			metadata["cursor"] = inc.Cursor
			metadata["source_name"] = s.Name
			label := u.Title
			if label == "" {
				label = u.Path
			}
			if len(inc.Messages) > 0 {
				provider := s.Adapter
				if provider == "pi" {
					provider = "Pi"
				}
				label = fmt.Sprintf("%s session %s: %s through %s", provider, u.ID, inc.Messages[0].ID, inc.LastID)
			}
			id := u.ID
			date := inc.Happened
			src := inputlog.Source{Kind: s.Kind, Text: inc.Text, Label: label, Happened: &date, Session: &id, Home: &home, ExternalID: &external, Metadata: metadata, Participants: participants}
			if e = src.Validate(); e != nil {
				return out, e
			}
			pending = append(pending, prepared{src, len(inc.Messages)})
			history[u.ID] = append(history[u.ID], src)
		}
	}
	if !matched {
		return out, fmt.Errorf("unknown source %s", o.Source)
	}
	sort.SliceStable(pending, func(i, j int) bool { return *pending[i].src.Happened < *pending[j].src.Happened })
	for _, p := range pending {
		out.Messages += p.messages
		if !o.DryRun {
			r, e := l.Append("source", "system", p.src)
			if e != nil {
				return out, e
			}
			out.Seqs = append(out.Seqs, r.Seq)
			if !r.Reused {
				out.Submitted++
			}
		}
	}
	return out, nil
}
func adapterFor(name string) (adapter, error) {
	switch name {
	case "pi", "claude-code", "codex":
		return sessionAdapter{name}, nil
	case "files":
		return fileAdapter{}, nil
	case "sqlite":
		return sqliteAdapter{}, nil
	}
	return nil, fmt.Errorf("unknown adapter %s", name)
}
func homeFor(routes []config.Route, cwd string) (string, error) {
	best, home := 0, ""
	path, e := filepath.Abs(cwd)
	if e != nil {
		return "", e
	}
	for _, r := range routes {
		root := r.Root
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

// Excludes are slash-separated path globs relative to the source root; ** matches any depth.
func paths(s config.Source, extensions []string) ([]string, error) {
	root, e := filepath.EvalSymlinks(s.Path)
	if e != nil {
		return nil, e
	}
	root, e = filepath.Abs(root)
	if e != nil {
		return nil, e
	}
	out := []string{}
	e = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, e := filepath.Rel(root, path)
		if e != nil {
			return e
		}
		if rel == "." && !d.IsDir() {
			rel = filepath.Base(path)
		}
		for _, pattern := range s.Exclude {
			if glob(pattern, filepath.ToSlash(rel)) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		if !d.IsDir() && config.Contains(extensions, strings.ToLower(filepath.Ext(path))) {
			out = append(out, path)
		}
		return nil
	})
	return out, e
}
func glob(pattern, path string) bool {
	p := strings.Split(pattern, "/")
	s := strings.Split(path, "/")
	var match func([]string, []string) bool
	match = func(p, s []string) bool {
		if len(p) == 0 {
			return len(s) == 0
		}
		if p[0] == "**" {
			return match(p[1:], s) || (len(s) > 0 && match(p, s[1:]))
		}
		if len(s) == 0 {
			return false
		}
		ok, _ := filepath.Match(p[0], s[0])
		return ok && match(p[1:], s[1:])
	}
	return match(p, s)
}
