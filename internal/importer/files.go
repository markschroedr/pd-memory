package importer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/markschroedr/pd-memory/internal/config"
	inputlog "github.com/markschroedr/pd-memory/internal/log"
)

type prefixCursor struct {
	Length int    `json:"length"`
	Hash   string `json:"hash"`
}

func prefixHash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func textCursor(history []inputlog.Source) (cursor, error) {
	if len(history) == 0 {
		return nil, nil
	}
	return json.Marshal(history[len(history)-1].Metadata["cursor"])
}
func readPrefix(b []byte, c cursor) (increment, error) {
	previous := prefixCursor{}
	if len(c) > 0 {
		if e := json.Unmarshal(c, &previous); e != nil {
			return increment{}, e
		}
	}
	if previous.Length < 0 {
		return increment{}, fmt.Errorf("negative prefix length")
	}
	if previous.Length > len(b) || (previous.Length > 0 && prefixHash(b[:previous.Length]) != previous.Hash) {
		return increment{Rewritten: true, Metadata: map[string]any{}}, nil
	}
	next := prefixCursor{len(b), prefixHash(b)}
	encoded, e := json.Marshal(next)
	return increment{Text: string(b[previous.Length:]), Cursor: encoded, LastID: next.Hash, Metadata: map[string]any{}}, e
}

type fileAdapter struct{}

func (fileAdapter) Units(s config.Source) ([]unit, error) {
	files, e := paths(s, []string{".txt", ".md"})
	if e != nil {
		return nil, e
	}
	out := []unit{}
	for _, p := range files {
		info, e := os.Stat(p)
		if e != nil {
			return nil, e
		}
		b, e := os.ReadFile(p)
		if e != nil {
			return nil, e
		}
		info, e = os.Stat(p)
		if e != nil {
			return nil, e
		}
		out = append(out, unit{ID: "files:" + p, Path: p, Folder: filepath.Dir(p), Title: filepath.Base(p), Activity: info.ModTime(), data: b})
	}
	return out, nil
}
func (fileAdapter) Cursor(h []inputlog.Source) (cursor, error) { return textCursor(h) }
func (fileAdapter) Read(u unit, c cursor, after time.Time) (increment, error) {
	if !after.IsZero() && u.Activity.Before(after) {
		return increment{}, nil
	}
	out, e := readPrefix(u.data.([]byte), c)
	if e != nil {
		return out, e
	}
	out.Happened = u.Activity.UTC().Format(time.RFC3339Nano)
	out.Metadata["path"] = u.Path
	return out, e
}
