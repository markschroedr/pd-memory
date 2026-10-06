package log

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/markschroedr/pd-memory/internal/config"
	"github.com/markschroedr/pd-memory/internal/memory"
)

type Entry struct {
	Seq     int64           `json:"seq"`
	Kind    string          `json:"kind"`
	Actor   string          `json:"actor"`
	Created string          `json:"created"`
	Payload json.RawMessage `json:"payload"`
}
type Source struct {
	Kind         string         `json:"kind"`
	Text         string         `json:"text"`
	Label        string         `json:"label"`
	Happened     *string        `json:"happened"`
	Session      *string        `json:"session"`
	Home         *string        `json:"home"`
	ExternalID   *string        `json:"external_id"`
	Metadata     map[string]any `json:"metadata"`
	Participants []string       `json:"participants"`
}

func (s Source) Validate() error {
	if !config.Contains(config.SourceKinds, s.Kind) || strings.TrimSpace(s.Text) == "" || strings.TrimSpace(s.Label) == "" {
		return fmt.Errorf("source needs valid kind, text, label")
	}
	return memory.Date(s.Happened)
}

type Note struct {
	memory.Fields
	Pages []string `json:"pages"`
	Text  string   `json:"text"`
}
type Edit struct {
	ID        string                     `json:"id"`
	Stamp     int64                      `json:"stamp"`
	Text      string                     `json:"text"`
	Reason    string                     `json:"reason"`
	Patch     map[string]json.RawMessage `json:"patch"`
	Pages     []string                   `json:"pages,omitempty"`
	Supersede bool                       `json:"supersede"`
	MergeInto string                     `json:"merge_into"`
	Parent    string                     `json:"parent"`
}
type Forget struct {
	ID     string `json:"id"`
	Text   string `json:"text"`
	Reason string `json:"reason"`
}
type Focus struct {
	ID     string `json:"id"`
	Text   string `json:"text"`
	Folder string `json:"folder"`
	Action string `json:"action"`
}
type Conflict struct{ Message string }

func (c *Conflict) Error() string { return c.Message }

type Submitted struct {
	Seq    int64  `json:"seq"`
	Reused bool   `json:"reused"`
	Status string `json:"status"`
}
type Store struct{ DB *sql.DB }

func Open(dir string, write bool) (*Store, error) {
	mode := "ro"
	if write {
		if e := os.MkdirAll(dir, 0700); e != nil {
			return nil, e
		}
		mode = "rwc"
	} else if _, e := os.Stat(filepath.Join(dir, "log.db")); e != nil {
		return nil, e
	}
	db, e := sql.Open("sqlite", "file:"+filepath.Join(dir, "log.db")+"?mode="+mode+"&_pragma=busy_timeout(5000)")
	if e != nil {
		return nil, e
	}
	db.SetMaxOpenConns(1)
	s := &Store{db}
	if write {
		_, e = db.Exec(`PRAGMA journal_mode=WAL;
 CREATE TABLE IF NOT EXISTS entries(seq INTEGER PRIMARY KEY AUTOINCREMENT,kind TEXT NOT NULL,actor TEXT NOT NULL,created TEXT NOT NULL,payload TEXT NOT NULL,hash TEXT UNIQUE);
 CREATE TRIGGER IF NOT EXISTS immutable_update BEFORE UPDATE ON entries BEGIN SELECT RAISE(ABORT,'log is append-only'); END;
 CREATE TRIGGER IF NOT EXISTS immutable_delete BEFORE DELETE ON entries BEGIN SELECT RAISE(ABORT,'log is append-only'); END;`)
	}
	if e != nil {
		db.Close()
		return nil, e
	}
	return s, nil
}
func (s *Store) Close() { s.DB.Close() }
func (s *Store) Append(kind, actor string, payload any) (Submitted, error) {
	b, e := json.Marshal(payload)
	if e != nil {
		return Submitted{}, e
	}
	var hash any
	if kind == "source" {
		v := payload.(Source)
		if e = v.Validate(); e != nil {
			return Submitted{}, e
		}
		identity := v.Kind + "\x00" + v.Text
		// Sync imports independent units, even when their new text is identical.
		// The adapter cursor makes a repeated publication of the same increment idempotent.
		if name, ok := v.Metadata["source_name"].(string); ok && v.Session != nil && v.Metadata["cursor"] != nil {
			identity += "\x00" + name + "\x00" + *v.Session + "\x00" + memory.JSON(v.Metadata["cursor"])
		}
		h := sha256.Sum256([]byte(identity))
		hash = hex.EncodeToString(h[:])
	}
	if !config.Contains([]string{"source", "note", "edit", "forget", "focus"}, kind) || !config.Contains([]string{"user", "agent", "system"}, actor) {
		return Submitted{}, fmt.Errorf("invalid entry kind or actor")
	}
	conn, e := s.DB.Conn(context.Background())
	if e != nil {
		return Submitted{}, e
	}
	defer conn.Close()
	if e = memory.Exec(conn, "BEGIN IMMEDIATE"); e != nil {
		return Submitted{}, e
	}
	defer memory.Exec(conn, "ROLLBACK")
	if hash != nil {
		var seq int64
		var existing string
		e = conn.QueryRowContext(context.Background(), "SELECT seq,payload FROM entries WHERE hash=?", hash).Scan(&seq, &existing)
		if e == nil {
			var a, c any
			if e = json.Unmarshal([]byte(existing), &a); e != nil {
				return Submitted{}, e
			}
			if e = json.Unmarshal(b, &c); e != nil {
				return Submitted{}, e
			}
			if memory.JSON(a) != memory.JSON(c) {
				return Submitted{}, &Conflict{"identical source content has different metadata"}
			}
			return Submitted{seq, true, "queued"}, nil
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return Submitted{}, e
		}
	}
	r, e := conn.ExecContext(context.Background(), "INSERT INTO entries(kind,actor,created,payload,hash) VALUES(?,?,?,?,?)", kind, actor, time.Now().UTC().Format(time.RFC3339Nano), string(b), hash)
	if e != nil {
		return Submitted{}, e
	}
	seq, e := r.LastInsertId()
	if e != nil {
		return Submitted{}, e
	}
	e = memory.Exec(conn, "COMMIT")
	return Submitted{seq, false, "queued"}, e
}
func (s *Store) Entries() ([]Entry, error) {
	rs, e := s.DB.Query("SELECT seq,kind,actor,created,payload FROM entries ORDER BY seq")
	if e != nil {
		return nil, e
	}
	defer rs.Close()
	out := []Entry{}
	for rs.Next() {
		var v Entry
		var p string
		if e = rs.Scan(&v.Seq, &v.Kind, &v.Actor, &v.Created, &p); e != nil {
			return nil, e
		}
		v.Payload = json.RawMessage(p)
		out = append(out, v)
	}
	return out, rs.Err()
}
