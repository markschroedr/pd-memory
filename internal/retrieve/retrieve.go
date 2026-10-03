package retrieve

import (
	"container/heap"
	"encoding/binary"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/markschroedr/pd-memory/internal/config"
	"github.com/markschroedr/pd-memory/internal/memory"
	"github.com/markschroedr/pd-memory/internal/model"
)

type Match struct {
	ID    string  `json:"id"`
	Score float64 `json:"similarity"`
}
type worst []Match

func (h worst) Len() int { return len(h) }
func (h worst) Less(i, j int) bool {
	if h[i].Score == h[j].Score {
		return h[i].ID > h[j].ID
	}
	return h[i].Score < h[j].Score
}
func (h worst) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *worst) Push(x any)   { *h = append(*h, x.(Match)) }
func (h *worst) Pop() any     { old := *h; x := old[len(old)-1]; *h = old[:len(old)-1]; return x }
func Blob(v []float64) []byte {
	b := make([]byte, len(v)*4)
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(float32(x)))
	}
	return b
}
func Vector(b []byte) []float64 {
	out := make([]float64, len(b)/4)
	for i := range out {
		out[i] = float64(math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:])))
	}
	return out
}
func scope(layer string, pages []string) (string, []any) {
	if len(pages) == 0 {
		return "", nil
	}
	ps := make([]string, len(pages))
	args := make([]any, len(pages))
	for i, p := range pages {
		ps[i] = "?"
		args[i] = p
	}
	if layer == "observations" {
		return " AND EXISTS(SELECT 1 FROM observation_pages op WHERE op.observation=o.id AND op.page IN (" + strings.Join(ps, ",") + "))", args
	}
	return " AND EXISTS(SELECT 1 FROM evidence ev JOIN observations o ON o.id=ev.observation JOIN observation_pages op ON op.observation=o.id WHERE ev.chunk=c.id AND o.replaced_by IS NULL AND o.forgotten IS NULL AND op.page IN (" + strings.Join(ps, ",") + "))", args
}
func Nearest(s *memory.Store, queries [][]float64, c *config.Config, layer string, pages []string, limit int) ([][]Match, error) {
	cond, args := scope(layer, pages)
	join := "JOIN observations o ON o.id=e.entry_id"
	eligible := " AND o.replaced_by IS NULL AND o.forgotten IS NULL"
	if layer == "chunks" {
		join = "JOIN chunks c ON c.id=e.entry_id"
		eligible = ""
	}
	params := []any{c.Embeddings.Model, c.Embeddings.Dimension}
	params = append(params, args...)
	rs, e := s.DB.Query("SELECT e.entry_id,e.vector FROM embeddings e "+join+" WHERE e.model=? AND e.dimension=?"+eligible+cond+" ORDER BY e.entry_id", params...)
	if e != nil {
		return nil, e
	}
	defer rs.Close()
	heaps := make([]worst, len(queries))
	norms := make([]float64, len(queries))
	for i, q := range queries {
		for _, x := range q {
			norms[i] += x * x
		}
		norms[i] = math.Sqrt(norms[i])
		if len(q) != c.Embeddings.Dimension || norms[i] == 0 {
			return nil, fmt.Errorf("query embedding mismatch")
		}
	}
	for rs.Next() {
		var id string
		var blob []byte
		if e = rs.Scan(&id, &blob); e != nil {
			return nil, e
		}
		v := Vector(blob)
		if len(v) != c.Embeddings.Dimension {
			return nil, fmt.Errorf("stored embedding mismatch")
		}
		norm := 0.
		for _, x := range v {
			norm += x * x
		}
		norm = math.Sqrt(norm)
		if norm == 0 {
			return nil, fmt.Errorf("zero vector")
		}
		for i, q := range queries {
			dot := 0.
			for j, x := range q {
				dot += x * v[j]
			}
			m := Match{id, dot / (norm * norms[i])}
			h := &heaps[i]
			if h.Len() < limit {
				heap.Push(h, m)
			} else if m.Score > (*h)[0].Score || (m.Score == (*h)[0].Score && m.ID < (*h)[0].ID) {
				heap.Pop(h)
				heap.Push(h, m)
			}
		}
	}
	if e = rs.Err(); e != nil {
		return nil, e
	}
	out := make([][]Match, len(queries))
	for i, h := range heaps {
		sort.Slice(h, func(i, j int) bool {
			if h[i].Score == h[j].Score {
				return h[i].ID < h[j].ID
			}
			return h[i].Score > h[j].Score
		})
		out[i] = h
	}
	return out, nil
}

var terms = regexp.MustCompile(`[\pL\pN]+`)

func Lexical(s *memory.Store, query, layer string, pages []string, phrase bool) ([]string, error) {
	words := terms.FindAllString(query, -1)
	if len(words) == 0 {
		return []string{}, nil
	}
	match := ""
	if phrase {
		match = "\"" + strings.Join(words, " ") + "\""
	} else {
		for i, w := range words {
			words[i] = "\"" + w + "\""
		}
		match = strings.Join(words, " OR ")
	}
	cond, args := scope(layer, pages)
	join := "JOIN observations o ON o.id=f.id"
	eligible := " AND o.replaced_by IS NULL AND o.forgotten IS NULL"
	if layer == "chunks" {
		join = "JOIN chunks c ON c.id=f.id"
		eligible = ""
	}
	params := append([]any{match}, args...)
	table := layer + "_fts"
	rs, e := s.DB.Query("SELECT f.id FROM "+table+" f "+join+" WHERE "+table+" MATCH ?"+eligible+cond+" ORDER BY bm25("+table+") LIMIT 100", params...)
	if e != nil {
		return nil, e
	}
	defer rs.Close()
	ids := []string{}
	for rs.Next() {
		var id string
		if e = rs.Scan(&id); e != nil {
			return nil, e
		}
		ids = append(ids, id)
	}
	return ids, rs.Err()
}
func Freshness(happened *string, entered string, durability *float64) float64 {
	if durability == nil {
		return 1
	}
	date := entered
	if happened != nil {
		date = *happened
	}
	t, e := time.Parse(time.RFC3339Nano, date)
	if e != nil {
		t, _ = time.Parse("2006-01-02", date)
	}
	return math.Pow(2, -math.Max(0, time.Since(t).Hours()/24) / *durability)
}
func Variables(o memory.Observation, c *config.Config, relevance float64) map[string]float64 {
	v := config.Variables()
	v["weight"] = o.Weight
	v["confidence"] = o.Confidence
	v["freshness"] = Freshness(o.Happened, o.Entered, o.Durability)
	v["source_prior"] = 0
	v["sources"] = float64(len(o.Sources))
	v["relevance"] = relevance
	v["observations"] = 0
	v["inbound"] = 0
	for _, s := range o.Sources {
		v["source_prior"] = math.Max(v["source_prior"], c.SourcePriors[s.Kind])
	}
	return v
}

type Hit struct {
	ID          string              `json:"id"`
	Kind        string              `json:"kind"`
	Bucket      int                 `json:"bucket"`
	Line        string              `json:"line"`
	Body        bool                `json:"body,omitempty"`
	Sources     int                 `json:"sources,omitempty"`
	Score       float64             `json:"-"`
	Observation *memory.Observation `json:"-"`
}
type Result struct {
	Hits []Hit
	Cost float64
}

// Search ranks observations and source chunks together and returns the best max_limit hits.
func Search(s *memory.Store, m *model.Client, c *config.Config, queries, pages []string) (Result, error) {
	out := Result{Hits: []Hit{}}
	if len(queries) == 0 {
		return out, fmt.Errorf("queries required")
	}
	for _, q := range queries {
		if strings.TrimSpace(q) == "" {
			return out, fmt.Errorf("empty query")
		}
	}
	before := m.Cost
	vectors, e := m.Embed(queries)
	if e != nil {
		return out, e
	}
	scores := map[string]float64{}
	for _, kind := range []string{"observations", "chunks"} {
		semantic, e := Nearest(s, vectors, c, kind, pages, 100)
		if e != nil {
			return out, e
		}
		for i, q := range queries {
			lex, e := Lexical(s, q, kind, pages, false)
			if e != nil {
				return out, e
			}
			for rank, id := range lex {
				scores[id] += 1 / float64(c.Search.RRFK+rank+1)
			}
			for rank, v := range semantic[i] {
				scores[v.ID] += 1 / float64(c.Search.RRFK+rank+1)
			}
		}
	}
	rows, e := s.Observations(true)
	if e != nil {
		return out, e
	}
	allowed := func(o memory.Observation) bool {
		if len(pages) == 0 {
			return true
		}
		for _, p := range o.Pages {
			if config.Contains(pages, p) {
				return true
			}
		}
		return false
	}
	// Named-page recall is structural matching, not semantic query expansion.
	allPages, e := s.Pages()
	if e != nil {
		return out, e
	}
	for _, p := range allPages {
		named := false
		for _, name := range append([]string{p.Slug}, p.Aliases...) {
			n := Normalize(name)
			if len(n) < 3 {
				continue
			}
			for _, q := range queries {
				if strings.Contains(" "+Normalize(q)+" ", " "+n+" ") {
					named = true
				}
			}
		}
		if named {
			for _, o := range rows {
				if allowed(o) && config.Contains(o.Pages, p.Slug) {
					scores[o.ID] += 1 / float64(c.Search.RRFK+1)
				}
			}
		}
	}
	max := 0.
	for _, v := range scores {
		max = math.Max(max, v)
	}
	for _, o := range rows {
		r := scores[o.ID]
		if r == 0 || !allowed(o) {
			continue
		}
		relevance := r / max
		score, e := c.Evaluate(c.CallSites.Search, Variables(o, c, relevance*1.05))
		if e != nil {
			return out, e
		}
		copy := o
		out.Hits = append(out.Hits, Hit{o.ID, "observation", c.Bucket(score), o.Line, o.Body != nil, len(o.Sources), score, &copy})
	}
	for id, r := range scores {
		if !strings.HasPrefix(id, "src:") || !strings.Contains(id, "/") {
			continue
		}
		rs, e := s.Query(`SELECT c.context,CASE WHEN l.kind='source' THEN json_extract(l.payload,'$.kind') WHEN l.actor='user' THEN 'note_user' ELSE 'note_agent' END source_kind,coalesce(max(json_extract(o.value,'$.weight')),0.3) weight,coalesce(max(json_extract(o.value,'$.confidence')),0.5) confidence,count(o.id) sources FROM chunks c JOIN inputlog.entries l ON l.seq=c.source_seq LEFT JOIN evidence ev ON ev.chunk=c.id LEFT JOIN observations o ON o.id=ev.observation AND o.replaced_by IS NULL AND o.forgotten IS NULL WHERE c.id=? GROUP BY c.id`, id)
		if e != nil {
			return out, e
		}
		if len(rs) == 0 {
			continue
		}
		v := config.Variables()
		v["relevance"] = r / max
		v["weight"] = number(rs[0]["weight"])
		v["confidence"] = number(rs[0]["confidence"])
		v["sources"] = number(rs[0]["sources"])
		v["source_prior"] = c.SourcePriors[rs[0]["source_kind"].(string)]
		score, e := c.Evaluate(c.CallSites.SearchChunk, v)
		if e != nil {
			return out, e
		}
		out.Hits = append(out.Hits, Hit{ID: id, Kind: "chunk", Bucket: c.Bucket(score), Line: rs[0]["context"].(string), Score: score})
	}
	sort.Slice(out.Hits, func(i, j int) bool {
		if out.Hits[i].Score == out.Hits[j].Score {
			return out.Hits[i].ID < out.Hits[j].ID
		}
		return out.Hits[i].Score > out.Hits[j].Score
	})
	out.Hits = out.Hits[:min(len(out.Hits), c.Search.MaxLimit)]
	out.Cost = m.Cost - before
	return out, nil
}
func Normalize(s string) string {
	return strings.Join(terms.FindAllString(strings.ToLower(s), -1), " ")
}
func number(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int64:
		return float64(n)
	}
	return 0
}
