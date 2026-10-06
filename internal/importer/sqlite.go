package importer

import (
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"time"

	"github.com/markschroedr/pd-memory/internal/config"
	inputlog "github.com/markschroedr/pd-memory/internal/log"
	_ "modernc.org/sqlite"
)

type sqliteAdapter struct{}
type sqliteText struct{ text, speaker string }

func (sqliteAdapter) Units(s config.Source) ([]unit, error) {
	path, e := filepath.EvalSymlinks(s.Path)
	if e != nil {
		return nil, e
	}
	path, e = filepath.Abs(path)
	if e != nil {
		return nil, e
	}
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	db, e := sql.Open("sqlite", uri.String()+"?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)")
	if e != nil {
		return nil, e
	}
	defer db.Close()
	rows, e := db.Query(s.Query)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	columns, e := rows.Columns()
	if e != nil {
		return nil, e
	}
	seen := map[string]bool{}
	for _, col := range columns {
		if !config.Contains([]string{"unit_id", "time", "text", "speaker", "title"}, col) {
			return nil, fmt.Errorf("unsupported query column %s", col)
		}
		seen[col] = true
	}
	for _, col := range []string{"unit_id", "time", "text"} {
		if !seen[col] {
			return nil, fmt.Errorf("query must return %s", col)
		}
	}
	out := []unit{}
	ids := map[string]bool{}
	for rows.Next() {
		values := make([]any, len(columns))
		ptrs := make([]any, len(columns))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if e = rows.Scan(ptrs...); e != nil {
			return nil, e
		}
		v := map[string]string{}
		for i, col := range columns {
			switch x := values[i].(type) {
			case nil:
			case []byte:
				v[col] = string(x)
			default:
				v[col] = fmt.Sprint(x)
			}
		}
		if v["unit_id"] == "" || ids[v["unit_id"]] {
			return nil, fmt.Errorf("query unit_id must be nonempty and unique")
		}
		ids[v["unit_id"]] = true
		excluded := false
		for _, pattern := range s.Exclude {
			excluded = excluded || glob(pattern, v["unit_id"])
		}
		if excluded {
			continue
		}
		at, e := time.Parse(time.RFC3339Nano, v["time"])
		if e != nil {
			return nil, fmt.Errorf("unit %s time must be RFC3339: %w", v["unit_id"], e)
		}
		title := v["title"]
		if title == "" {
			title = v["unit_id"]
		}
		out = append(out, unit{ID: "sqlite:" + path + ":" + v["unit_id"], Path: path + "#" + v["unit_id"], Folder: filepath.Dir(path), Title: title, Activity: at, data: sqliteText{v["text"], v["speaker"]}})
	}
	return out, rows.Err()
}
func (sqliteAdapter) Cursor(h []inputlog.Source) (cursor, error) { return textCursor(h) }
func (sqliteAdapter) Read(u unit, c cursor, after time.Time) (increment, error) {
	if !after.IsZero() && u.Activity.Before(after) {
		return increment{}, nil
	}
	v := u.data.(sqliteText)
	out, e := readPrefix([]byte(v.text), c)
	if e != nil {
		return out, e
	}
	out.Speaker = v.speaker
	out.Happened = u.Activity.UTC().Format(time.RFC3339Nano)
	out.Metadata["path"] = u.Path
	return out, e
}
