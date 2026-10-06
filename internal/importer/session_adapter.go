package importer

import (
	"encoding/json"
	"os"
	"time"

	"github.com/markschroedr/pd-memory/internal/config"
	inputlog "github.com/markschroedr/pd-memory/internal/log"
)

type sessionAdapter struct{ provider string }

func (a sessionAdapter) Units(s config.Source) ([]unit, error) {
	files, e := paths(s, []string{".jsonl"})
	if e != nil {
		return nil, e
	}
	out := []unit{}
	for _, p := range files {
		info, e := os.Stat(p)
		if e != nil {
			return nil, e
		}
		session, e := Parse(p, a.provider)
		if e != nil {
			return nil, e
		}
		if session == nil {
			continue
		}
		info, e = os.Stat(p)
		if e != nil {
			return nil, e
		}
		out = append(out, unit{ID: session.ID, Path: p, Folder: session.CWD, Activity: info.ModTime(), data: session})
	}
	return out, nil
}
func (a sessionAdapter) Cursor(history []inputlog.Source) (cursor, error) {
	// Preserve today's union of entry_ids, including logs written by the retired importer.
	ids := map[string]bool{}
	for _, src := range history {
		switch list := src.Metadata["entry_ids"].(type) {
		case []any:
			for _, v := range list {
				if id, ok := v.(string); ok {
					ids[id] = true
				}
			}
		case []string:
			for _, id := range list {
				ids[id] = true
			}
		}
	}
	return json.Marshal(ids)
}
func (a sessionAdapter) Read(u unit, c cursor, after time.Time) (increment, error) {
	ids := map[string]bool{}
	if len(c) > 0 {
		if e := json.Unmarshal(c, &ids); e != nil {
			return increment{}, e
		}
	}
	s := u.data.(*Session)
	out := increment{Metadata: map[string]any{"source": a.provider, "cwd": s.CWD, "primary_jsonl": u.Path}}
	imported := []string{}
	user := false
	for _, m := range s.Messages {
		at, _ := time.Parse(time.RFC3339Nano, m.Timestamp)
		if ids[m.ID] || (!after.IsZero() && at.Before(after)) {
			continue
		}
		out.Messages = append(out.Messages, m)
		imported = append(imported, m.ID)
		ids[m.ID] = true
		user = user || m.Role == "user"
	}
	if !user {
		return increment{}, nil
	}
	out.Happened = out.Messages[0].Timestamp
	out.LastID = out.Messages[len(out.Messages)-1].ID
	out.Metadata["entry_ids"] = imported
	out.Metadata["first_entry_id"] = out.Messages[0].ID
	out.Metadata["through_entry_id"] = out.LastID
	var e error
	out.Cursor, e = json.Marshal(ids)
	return out, e
}
