package memory

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Fields struct {
	Line       string   `json:"line" jsonschema:"minLength=1"`
	Body       *string  `json:"body" jsonschema:"nullable"`
	Happened   *string  `json:"happened" jsonschema:"nullable"`
	Claimant   *string  `json:"claimant" jsonschema:"nullable"`
	Authority  string   `json:"authority" jsonschema:"enum=user,enum=agent,enum=third_party,enum=unknown"`
	Kind       string   `json:"kind" jsonschema:"enum=fact,enum=question,enum=commitment"`
	Confidence float64  `json:"confidence" jsonschema:"minimum=0,maximum=1"`
	Weight     float64  `json:"weight" jsonschema:"minimum=0,maximum=1"`
	Durability *float64 `json:"durability" jsonschema:"nullable,exclusiveMinimum=0"`
}

func (f Fields) Validate() error {
	if strings.TrimSpace(f.Line) == "" {
		return fmt.Errorf("line must not be empty")
	}
	if f.Body != nil && strings.TrimSpace(*f.Body) == "" {
		return fmt.Errorf("body must not be empty")
	}
	if !member([]string{"user", "agent", "third_party", "unknown"}, f.Authority) || !member([]string{"fact", "question", "commitment"}, f.Kind) {
		return fmt.Errorf("invalid authority or kind")
	}
	if f.Confidence < 0 || f.Confidence > 1 || f.Weight < 0 || f.Weight > 1 || (f.Durability != nil && *f.Durability <= 0) {
		return fmt.Errorf("invalid scores")
	}
	return Date(f.Happened)
}
func Date(s *string) error {
	if s == nil {
		return nil
	}
	if _, e := time.Parse("2006-01-02", *s); e == nil {
		return nil
	}
	_, e := time.Parse(time.RFC3339Nano, *s)
	return e
}
func member(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

type Observation struct {
	Fields
	ID            string      `json:"id"`
	Seq           int64       `json:"seq"`
	Entered       string      `json:"entered"`
	ReplacedBy    *string     `json:"replaced_by" jsonschema:"nullable"`
	Forgotten     *int64      `json:"forgotten" jsonschema:"nullable"`
	Pages         []string    `json:"pages"`
	Contributions []int64     `json:"contributions"`
	Evidence      []string    `json:"evidence"`
	Sources       []SourceRef `json:"sources"`
}
type SourceRef struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
}
type Page struct {
	Slug       string   `json:"slug"`
	Category   string   `json:"category"`
	Line       *string  `json:"line" jsonschema:"nullable"`
	Aliases    []string `json:"aliases"`
	Parent     *string  `json:"parent" jsonschema:"nullable"`
	ReplacedBy *string  `json:"replaced_by" jsonschema:"nullable"`
	Weight     float64  `json:"weight"`
	Seq        int64    `json:"seq"`
}
type Chunk struct {
	ID        string `json:"id"`
	SourceSeq int64  `json:"source_seq"`
	Index     int    `json:"index"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Context   string `json:"context"`
	Text      string `json:"text"`
}
type View struct {
	ID        string   `json:"id"`
	Kind      string   `json:"kind"`
	Scope     string   `json:"scope"`
	Text      string   `json:"text"`
	Headline  string   `json:"headline"`
	InputIDs  []string `json:"input_ids"`
	Citations []string `json:"citations"`
	Seq       int64    `json:"seq"`
	Created   string   `json:"created"`
}
type Store struct {
	DB  *sql.DB
	Dir string
}

func Open(dir string, write bool) (*Store, error) {
	mode := "ro"
	if write {
		mode = "rwc"
	}
	db, e := sql.Open("sqlite", "file:"+filepath.Join(dir, "memory.db")+"?mode="+mode+"&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if e != nil {
		return nil, e
	}
	db.SetMaxOpenConns(1)
	s := &Store{db, dir}
	if write {
		if _, e = db.Exec("PRAGMA journal_mode=WAL;" + schema); e != nil {
			db.Close()
			return nil, e
		}
	}
	if _, e = db.Exec("ATTACH DATABASE ? AS inputlog", "file:"+filepath.Join(dir, "log.db")+"?mode=ro"); e != nil {
		db.Close()
		return nil, e
	}
	if !write {
		if _, e = db.Exec("PRAGMA query_only=ON"); e != nil {
			db.Close()
			return nil, e
		}
	}
	return s, nil
}
func (s *Store) Close() { s.DB.Close() }
func (s *Store) Tx(fn func(*sql.Conn) error) error {
	c, e := s.DB.Conn(context.Background())
	if e != nil {
		return e
	}
	defer c.Close()
	if _, e = c.ExecContext(context.Background(), "BEGIN IMMEDIATE"); e != nil {
		return e
	}
	defer c.ExecContext(context.Background(), "ROLLBACK")
	if e = fn(c); e != nil {
		return e
	}
	_, e = c.ExecContext(context.Background(), "COMMIT")
	return e
}
func Exec(c *sql.Conn, q string, args ...any) error {
	_, e := c.ExecContext(context.Background(), q, args...)
	return e
}
func JSON(v any) string {
	b, e := json.Marshal(v)
	if e != nil {
		panic(e)
	}
	return string(b)
}
func ID() string {
	var b [4]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b[:])
}
func (s *Store) Seq() (int64, error) {
	var n int64
	e := s.DB.QueryRow("SELECT coalesce(max(seq),0) FROM fold_state WHERE status='done'").Scan(&n)
	return n, e
}
func (s *Store) Pages() ([]Page, error) {
	rs, e := s.DB.Query("SELECT slug,category,line,aliases,parent,replaced_by,weight,seq FROM pages WHERE replaced_by IS NULL ORDER BY slug")
	if e != nil {
		return nil, e
	}
	defer rs.Close()
	out := []Page{}
	for rs.Next() {
		var p Page
		var a string
		if e = rs.Scan(&p.Slug, &p.Category, &p.Line, &a, &p.Parent, &p.ReplacedBy, &p.Weight, &p.Seq); e != nil {
			return nil, e
		}
		if e = json.Unmarshal([]byte(a), &p.Aliases); e != nil {
			return nil, e
		}
		out = append(out, p)
	}
	return out, rs.Err()
}
func (s *Store) Observations(current bool) ([]Observation, error) {
	where := ""
	if current {
		where = " WHERE replaced_by IS NULL AND forgotten IS NULL"
	}
	return s.observations(where)
}
func (s *Store) ObservationsByIDs(ids []string) ([]Observation, error) {
	return s.observations(" WHERE id IN (SELECT value FROM json_each(?))", JSON(ids))
}
func (s *Store) observations(where string, args ...any) ([]Observation, error) {
	q := "SELECT id,value,seq,entered,replaced_by,forgotten FROM observations" + where + " ORDER BY entered,id"
	rs, e := s.DB.Query(q, args...)
	if e != nil {
		return nil, e
	}
	out := []Observation{}
	for rs.Next() {
		var o Observation
		var v string
		if e = rs.Scan(&o.ID, &v, &o.Seq, &o.Entered, &o.ReplacedBy, &o.Forgotten); e != nil {
			rs.Close()
			return nil, e
		}
		if e = json.Unmarshal([]byte(v), &o.Fields); e != nil {
			rs.Close()
			return nil, e
		}
		out = append(out, o)
	}
	e = rs.Err()
	rs.Close()
	if e != nil {
		return nil, e
	}
	if len(out) == 0 {
		return out, nil
	}
	byID := map[string]*Observation{}
	ids := []string{}
	for i := range out {
		o := &out[i]
		byID[o.ID] = o
		ids = append(ids, o.ID)
		o.Pages = []string{}
		o.Contributions = []int64{}
		o.Evidence = []string{}
		o.Sources = []SourceRef{}
	}
	selected := JSON(ids)
	links, e := s.DB.Query("SELECT observation,page FROM observation_pages WHERE observation IN (SELECT value FROM json_each(?)) ORDER BY page", selected)
	if e != nil {
		return nil, e
	}
	for links.Next() {
		var id, page string
		if e = links.Scan(&id, &page); e != nil {
			links.Close()
			return nil, e
		}
		byID[id].Pages = append(byID[id].Pages, page)
	}
	e = links.Err()
	links.Close()
	if e != nil {
		return nil, e
	}
	links, e = s.DB.Query("SELECT c.observation,c.seq,CASE WHEN l.kind='source' THEN json_extract(l.payload,'$.kind') WHEN l.actor='user' THEN 'note_user' ELSE 'note_agent' END FROM contributions c JOIN inputlog.entries l ON l.seq=c.seq WHERE c.observation IN (SELECT value FROM json_each(?)) ORDER BY c.seq", selected)
	if e != nil {
		return nil, e
	}
	for links.Next() {
		var id, kind string
		var seq int64
		if e = links.Scan(&id, &seq, &kind); e != nil {
			links.Close()
			return nil, e
		}
		o := byID[id]
		o.Contributions = append(o.Contributions, seq)
		o.Sources = append(o.Sources, SourceRef{fmt.Sprintf("src:%d", seq), kind})
	}
	e = links.Err()
	links.Close()
	if e != nil {
		return nil, e
	}
	links, e = s.DB.Query("SELECT observation,chunk FROM evidence WHERE observation IN (SELECT value FROM json_each(?)) ORDER BY chunk", selected)
	if e != nil {
		return nil, e
	}
	for links.Next() {
		var id, ch string
		if e = links.Scan(&id, &ch); e != nil {
			links.Close()
			return nil, e
		}
		byID[id].Evidence = append(byID[id].Evidence, ch)
	}
	e = links.Err()
	links.Close()
	return out, e
}
func (s *Store) Observation(id string) (Observation, error) {
	rows, e := s.ObservationsByIDs([]string{id})
	if e != nil {
		return Observation{}, e
	}
	if len(rows) == 0 {
		return Observation{}, fmt.Errorf("unknown observation %s: %w", id, sql.ErrNoRows)
	}
	return rows[0], nil
}
func (s *Store) Page(id string) (Page, error) {
	var p Page
	var a string
	e := s.DB.QueryRow("SELECT slug,category,line,aliases,parent,replaced_by,weight,seq FROM pages WHERE slug=?", id).Scan(&p.Slug, &p.Category, &p.Line, &a, &p.Parent, &p.ReplacedBy, &p.Weight, &p.Seq)
	if e != nil {
		return p, e
	}
	e = json.Unmarshal([]byte(a), &p.Aliases)
	return p, e
}
func (s *Store) Descendants(root string) ([]string, error) {
	if _, e := s.Page(root); e != nil {
		return nil, e
	}
	rs, e := s.DB.Query("WITH RECURSIVE tree(slug) AS (SELECT slug FROM pages WHERE slug=? AND replaced_by IS NULL UNION ALL SELECT p.slug FROM pages p JOIN tree t ON p.parent=t.slug WHERE p.replaced_by IS NULL) SELECT slug FROM tree", root)
	if e != nil {
		return nil, e
	}
	defer rs.Close()
	out := []string{}
	for rs.Next() {
		var x string
		if e = rs.Scan(&x); e != nil {
			return nil, e
		}
		out = append(out, x)
	}
	return out, rs.Err()
}
func (s *Store) Chunks(seq int64) ([]Chunk, error) {
	var payload string
	if e := s.DB.QueryRow("SELECT payload FROM inputlog.entries WHERE seq=?", seq).Scan(&payload); e != nil {
		return nil, e
	}
	var v struct {
		Text string `json:"text"`
	}
	if e := json.Unmarshal([]byte(payload), &v); e != nil {
		return nil, e
	}
	lines := strings.Split(v.Text, "\n")
	rs, e := s.DB.Query("SELECT id,source_seq,chunk_index,start_line,end_line,context FROM chunks WHERE source_seq=? ORDER BY chunk_index", seq)
	if e != nil {
		return nil, e
	}
	defer rs.Close()
	out := []Chunk{}
	for rs.Next() {
		var c Chunk
		if e = rs.Scan(&c.ID, &c.SourceSeq, &c.Index, &c.StartLine, &c.EndLine, &c.Context); e != nil {
			return nil, e
		}
		if c.StartLine < 1 || c.EndLine < c.StartLine || c.EndLine > len(lines) {
			return nil, fmt.Errorf("invalid stored chunk range: %s", c.ID)
		}
		c.Text = strings.Join(lines[c.StartLine-1:c.EndLine], "\n")
		out = append(out, c)
	}
	return out, rs.Err()
}
func (s *Store) Views() ([]View, error) {
	rs, e := s.DB.Query("SELECT id,kind,scope,text,headline,input_ids,citations,seq,created FROM views ORDER BY id")
	if e != nil {
		return nil, e
	}
	defer rs.Close()
	out := []View{}
	for rs.Next() {
		var v View
		var ids, cites string
		if e = rs.Scan(&v.ID, &v.Kind, &v.Scope, &v.Text, &v.Headline, &ids, &cites, &v.Seq, &v.Created); e != nil {
			return nil, e
		}
		if e = json.Unmarshal([]byte(ids), &v.InputIDs); e != nil {
			return nil, e
		}
		if e = json.Unmarshal([]byte(cites), &v.Citations); e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rs.Err()
}
func (s *Store) Valid(v View) (bool, error) {
	if v.Kind != "composition" {
		return true, nil
	}
	for _, id := range v.InputIDs {
		var seq int64
		var replaced *string
		var forgotten *int64
		e := s.DB.QueryRow("SELECT seq,replaced_by,forgotten FROM observations WHERE id=?", id).Scan(&seq, &replaced, &forgotten)
		if e == sql.ErrNoRows {
			return false, nil
		}
		if e != nil {
			return false, e
		}
		if (replaced != nil || forgotten != nil) && seq > v.Seq {
			return false, nil
		}
	}
	return true, nil
}
func (s *Store) Query(q string, args ...any) ([]map[string]any, error) {
	rs, e := s.DB.Query(q, args...)
	if e != nil {
		return nil, e
	}
	defer rs.Close()
	cols, e := rs.Columns()
	if e != nil {
		return nil, e
	}
	out := []map[string]any{}
	for rs.Next() {
		v := make([]any, len(cols))
		p := make([]any, len(cols))
		for i := range v {
			p[i] = &v[i]
		}
		if e = rs.Scan(p...); e != nil {
			return nil, e
		}
		r := map[string]any{}
		for i, k := range cols {
			if b, ok := v[i].([]byte); ok {
				v[i] = string(b)
			}
			r[k] = v[i]
		}
		out = append(out, r)
	}
	return out, rs.Err()
}
func (s *Store) Cost() (float64, error) {
	var n float64
	e := s.DB.QueryRow("SELECT coalesce(sum(cost_usd),0) FROM model_calls").Scan(&n)
	return n, e
}
func (o Observation) Text() string {
	if o.Body != nil {
		return o.Line + "\n\n" + *o.Body
	}
	return o.Line
}

const schema = `
CREATE TABLE IF NOT EXISTS fold_state(seq INTEGER PRIMARY KEY,status TEXT NOT NULL CHECK(status IN ('done','failed')),attempts INTEGER NOT NULL,error TEXT);
CREATE TABLE IF NOT EXISTS extraction(seq INTEGER PRIMARY KEY,value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS pages(slug TEXT PRIMARY KEY,category TEXT NOT NULL,line TEXT,aliases TEXT NOT NULL,parent TEXT REFERENCES pages(slug),replaced_by TEXT REFERENCES pages(slug),weight REAL NOT NULL,seq INTEGER NOT NULL);
INSERT OR IGNORE INTO pages VALUES('root','topic','Memory root','[]',NULL,NULL,1,0);
CREATE TABLE IF NOT EXISTS observations(id TEXT PRIMARY KEY,value TEXT NOT NULL,seq INTEGER NOT NULL,entered TEXT NOT NULL,replaced_by TEXT REFERENCES observations(id),forgotten INTEGER);
CREATE TABLE IF NOT EXISTS observation_pages(observation TEXT REFERENCES observations(id),page TEXT REFERENCES pages(slug),PRIMARY KEY(observation,page));
CREATE INDEX IF NOT EXISTS page_observations ON observation_pages(page);
CREATE TABLE IF NOT EXISTS contributions(observation TEXT REFERENCES observations(id),seq INTEGER,PRIMARY KEY(observation,seq));
CREATE TABLE IF NOT EXISTS chunks(id TEXT PRIMARY KEY,source_seq INTEGER NOT NULL,chunk_index INTEGER NOT NULL,start_line INTEGER NOT NULL,end_line INTEGER NOT NULL,context TEXT NOT NULL,UNIQUE(source_seq,chunk_index));
CREATE TABLE IF NOT EXISTS evidence(observation TEXT REFERENCES observations(id),chunk TEXT REFERENCES chunks(id),PRIMARY KEY(observation,chunk));
CREATE INDEX IF NOT EXISTS chunk_evidence ON evidence(chunk);
CREATE TABLE IF NOT EXISTS embeddings(entry_id TEXT NOT NULL,model TEXT NOT NULL,dimension INTEGER NOT NULL,vector BLOB NOT NULL,PRIMARY KEY(entry_id,model));
CREATE TABLE IF NOT EXISTS views(id TEXT PRIMARY KEY,kind TEXT NOT NULL,scope TEXT NOT NULL,text TEXT NOT NULL,headline TEXT NOT NULL,input_ids TEXT NOT NULL,citations TEXT NOT NULL,seq INTEGER NOT NULL,created TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS focus(folder TEXT NOT NULL,entry TEXT NOT NULL,seq INTEGER NOT NULL,PRIMARY KEY(folder,entry));
CREATE TABLE IF NOT EXISTS maintenance_errors(id TEXT PRIMARY KEY,error TEXT NOT NULL,created TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS model_calls(id INTEGER PRIMARY KEY,seq INTEGER,phase TEXT NOT NULL,request_id TEXT,model TEXT NOT NULL,tier TEXT,usage TEXT NOT NULL,cost_usd REAL NOT NULL,repaired INTEGER NOT NULL,error TEXT,created TEXT NOT NULL);
CREATE VIRTUAL TABLE IF NOT EXISTS observations_fts USING fts5(id UNINDEXED,line,body);
CREATE VIRTUAL TABLE IF NOT EXISTS chunks_fts USING fts5(id UNINDEXED,context,text);
CREATE TRIGGER IF NOT EXISTS observation_insert AFTER INSERT ON observations BEGIN INSERT INTO observations_fts VALUES(new.id,json_extract(new.value,'$.line'),coalesce(json_extract(new.value,'$.body'),'')); END;
CREATE TRIGGER IF NOT EXISTS observation_update AFTER UPDATE OF value ON observations BEGIN DELETE FROM observations_fts WHERE id=old.id; INSERT INTO observations_fts VALUES(new.id,json_extract(new.value,'$.line'),coalesce(json_extract(new.value,'$.body'),'')); END;
`
