package fold

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/markschroedr/pd-memory/internal/config"
	inputlog "github.com/markschroedr/pd-memory/internal/log"
	"github.com/markschroedr/pd-memory/internal/memory"
	"github.com/markschroedr/pd-memory/internal/model"
	"github.com/markschroedr/pd-memory/internal/retrieve"
)

//go:embed chunk_system.txt
var chunkSystem string

//go:embed extraction_system.txt
var extractionSystem string

//go:embed integration_system.txt
var integrationSystem string

type candidatePage struct {
	Name     string   `json:"name"`
	Category string   `json:"category" jsonschema:"enum=actor,enum=artifact,enum=place,enum=event,enum=project,enum=topic"`
	Aliases  []string `json:"aliases"`
}
type Candidate struct {
	memory.Fields
	CandidateID string          `json:"candidate_id"`
	Evidence    []string        `json:"evidence"`
	Pages       []candidatePage `json:"pages"`
}
type Extraction struct {
	Participants []string    `json:"participants"`
	Observations []Candidate `json:"observations"`
	Digest       string      `json:"source_digest"`
}
type PageProposal struct {
	Key       string   `json:"key"`
	Action    string   `json:"action" jsonschema:"enum=reuse,enum=create"`
	Slug      string   `json:"slug"`
	Category  string   `json:"category" jsonschema:"enum=actor,enum=artifact,enum=place,enum=event,enum=project,enum=topic"`
	Line      *string  `json:"line" jsonschema:"nullable"`
	Aliases   []string `json:"aliases"`
	Parent    *string  `json:"parent" jsonschema:"nullable"`
	MergeFrom []string `json:"merge_from"`
}
type Operation struct {
	memory.Fields
	CandidateIDs   []string `json:"candidate_ids"`
	ObservationIDs []string `json:"observation_ids"`
	Reason         string   `json:"reason"`
	Op             string   `json:"op" jsonschema:"enum=discard,enum=create,enum=attach_source,enum=update,enum=merge,enum=supersede"`
	PageKeys       []string `json:"page_keys"`
}
type Proposal struct {
	Pages        []PageProposal `json:"pages"`
	Observations []Operation    `json:"observations"`
}
type chunkStart struct {
	Start   int    `json:"start_line"`
	Context string `json:"context"`
}
type chunkOutput struct {
	Chunks []chunkStart `json:"chunks"`
}

func tokens(s string) int { return (len(strings.Fields(s))*4 + 2) / 3 }
func (w *Worker) identity() string {
	return fmt.Sprintf(" Authority identity: user claimants=%s; known third-party claimants=%s.", memory.JSON(w.Config.Identity.UserNames), memory.JSON(w.Config.Identity.ThirdPartyNames))
}
func (w *Worker) chunks(e inputlog.Entry, src inputlog.Source) ([]memory.Chunk, error) {
	existing, err := w.Memory.Chunks(e.Seq)
	if err != nil {
		return nil, err
	}
	if len(existing) > 0 {
		return existing, nil
	}
	lines := strings.Split(src.Text, "\n")
	starts := []chunkStart{{1, src.Label}}
	if tokens(src.Text) > w.Config.Chunk.SingleChunkTokens && src.Kind != "note_user" && src.Kind != "note_agent" {
		numbered := make([]string, len(lines))
		for i, l := range lines {
			numbered[i] = fmt.Sprintf("%d\t%s", i+1, l)
		}
		result, err := model.Structured[chunkOutput](w.Model, "chunks", chunkSystem+fmt.Sprintf(" Aim for about %d estimated tokens per passage.", w.Config.Chunk.TargetTokens), "<source>\n"+strings.Join(numbered, "\n")+"\n</source>", func(out chunkOutput) error {
			last := 0
			for i, c := range out.Chunks {
				switch {
				case i == 0 && c.Start != 1:
					return fmt.Errorf("chunks[0].start_line is %d; the first chunk must start at line 1", c.Start)
				case c.Start < 1 || c.Start > len(lines):
					return fmt.Errorf("chunks[%d].start_line %d is outside lines 1 to %d", i, c.Start, len(lines))
				case c.Start <= last:
					return fmt.Errorf("chunks[%d].start_line %d does not increase after %d; chunk starts must strictly increase", i, c.Start, last)
				case strings.TrimSpace(c.Context) == "":
					return fmt.Errorf("chunks[%d].context is empty", i)
				}
				last = c.Start
			}
			if len(out.Chunks) == 0 {
				return fmt.Errorf("chunks empty")
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		starts = result.Chunks
	}
	err = w.Memory.Tx(func(c *sql.Conn) error {
		for i, start := range starts {
			end := len(lines)
			if i+1 < len(starts) {
				end = starts[i+1].Start - 1
			}
			id := fmt.Sprintf("src:%d/%d", e.Seq, i+1)
			if err := memory.Exec(c, "INSERT INTO chunks VALUES(?,?,?,?,?,?)", id, e.Seq, i+1, start.Start, end, start.Context); err != nil {
				return err
			}
			if err := memory.Exec(c, "INSERT INTO chunks_fts VALUES(?,?,?)", id, start.Context, strings.Join(lines[start.Start-1:end], "\n")); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return w.Memory.Chunks(e.Seq)
}
func (w *Worker) extract(e inputlog.Entry, src inputlog.Source) (Extraction, error) {
	var cached string
	err := w.Memory.DB.QueryRow("SELECT value FROM extraction WHERE seq=?", e.Seq).Scan(&cached)
	if err == nil {
		var ex Extraction
		err = model.Decode([]byte(cached), &ex)
		return ex, err
	}
	if err != sql.ErrNoRows {
		return Extraction{}, err
	}
	// A failed early extraction is this entry's attempt; do not silently spend a second one.
	if early, ok := w.extracted[e.Seq]; ok {
		delete(w.extracted, e.Seq)
		if early != nil {
			return Extraction{}, early
		}
	}
	chunks, err := w.chunks(e, src)
	if err != nil {
		return Extraction{}, err
	}
	windows := [][]memory.Chunk{}
	current := []memory.Chunk{}
	size := 0
	for _, ch := range chunks {
		n := tokens(ch.Text)
		if len(current) > 0 && size+n > w.Config.Chunk.ExtractWindowTokens {
			windows = append(windows, current)
			current = []memory.Chunk{}
			size = 0
		}
		current = append(current, ch)
		size += n
	}
	if len(current) > 0 {
		windows = append(windows, current)
	}
	ex := Extraction{Participants: []string{}, Observations: []Candidate{}}
	digests := []string{}
	for i, window := range windows {
		allowed := map[string]bool{}
		render := []string{}
		for _, ch := range window {
			allowed[ch.ID] = true
			render = append(render, "["+ch.ID+"] "+ch.Text)
		}
		meta := map[string]any{"source": fmt.Sprintf("src:%d", e.Seq), "kind": src.Kind, "label": src.Label, "home": src.Home, "happened": src.Happened, "external_id": src.ExternalID, "metadata": src.Metadata, "window": i + 1, "windows": len(windows)}
		out, err := model.Structured[Extraction](w.Model, "extraction", extractionSystem+w.identity(), memory.JSON(meta)+"\n\n<source>\n"+strings.Join(render, "\n\n")+"\n</source>", func(x Extraction) error {
			if strings.TrimSpace(x.Digest) == "" {
				return fmt.Errorf("digest required")
			}
			seen := map[string]bool{}
			for _, o := range x.Observations {
				if err := o.Fields.Validate(); err != nil {
					return fmt.Errorf("observation %s: %w", o.CandidateID, err)
				}
				switch {
				case o.CandidateID == "":
					return fmt.Errorf("observation %q has an empty candidate_id", o.Line)
				case seen[o.CandidateID]:
					return fmt.Errorf("duplicate candidate_id %s", o.CandidateID)
				case len(o.Pages) == 0:
					return fmt.Errorf("observation %s needs at least one page", o.CandidateID)
				case len(o.Evidence) == 0:
					return fmt.Errorf("observation %s needs evidence", o.CandidateID)
				}
				seen[o.CandidateID] = true
				for j, id := range o.Evidence {
					// Models sometimes copy the rendered [id] brackets. The slice shares the decoded result.
					id = strings.TrimSuffix(strings.TrimPrefix(id, "["), "]")
					o.Evidence[j] = id
					if !allowed[id] {
						return fmt.Errorf("observation %s cites %s; valid ids are %s to %s", o.CandidateID, id, window[0].ID, window[len(window)-1].ID)
					}
				}
				for _, p := range o.Pages {
					if p.Name == "" || !config.Contains([]string{"actor", "artifact", "place", "event", "project", "topic"}, p.Category) {
						return fmt.Errorf("observation %s has page %q with category %q; categories are actor, artifact, place, event, project, topic", o.CandidateID, p.Name, p.Category)
					}
				}
			}
			return nil
		})
		if err != nil {
			return ex, err
		}
		for _, p := range out.Participants {
			if !config.Contains(ex.Participants, p) {
				ex.Participants = append(ex.Participants, p)
			}
		}
		for _, o := range out.Observations {
			if len(windows) > 1 {
				o.CandidateID = fmt.Sprintf("w%d-%s", i+1, o.CandidateID)
			}
			ex.Observations = append(ex.Observations, o)
		}
		digests = append(digests, out.Digest)
	}
	ex.Digest = strings.Join(digests, "\n")
	err = w.Memory.Tx(func(c *sql.Conn) error {
		return memory.Exec(c, "INSERT INTO extraction VALUES(?,?)", e.Seq, memory.JSON(ex))
	})
	return ex, err
}
func (w *Worker) embedChunks(seq int64) error {
	chunks, e := w.Memory.Chunks(seq)
	if e != nil {
		return e
	}
	inputs := []string{}
	ids := []string{}
	for _, ch := range chunks {
		var found int
		e = w.Memory.DB.QueryRow("SELECT 1 FROM embeddings WHERE entry_id=? AND model=? AND dimension=?", ch.ID, w.Config.Embeddings.Model, w.Config.Embeddings.Dimension).Scan(&found)
		if e == sql.ErrNoRows {
			ids = append(ids, ch.ID)
			inputs = append(inputs, ch.Context+"\n"+ch.Text)
		} else if e != nil {
			return e
		}
	}
	v, e := w.Model.Embed(inputs)
	if e != nil {
		return e
	}
	return w.Memory.Tx(func(c *sql.Conn) error {
		for i, id := range ids {
			if e = putVector(c, id, w.Config, v[i]); e != nil {
				return e
			}
		}
		return nil
	})
}
func putVector(c *sql.Conn, id string, cfg *config.Config, v []float64) error {
	return memory.Exec(c, "INSERT OR REPLACE INTO embeddings VALUES(?,?,?,?)", id, cfg.Embeddings.Model, len(v), retrieve.Blob(v))
}
func (w *Worker) context(ex Extraction) ([]memory.Page, []memory.Observation, []any, error) {
	pages, e := w.Memory.Pages()
	if e != nil {
		return nil, nil, nil, e
	}
	facets := []string{}
	for _, o := range ex.Observations {
		facets = append(facets, o.Line)
		if o.Body != nil {
			facets = append(facets, *o.Body)
		}
	}
	vectors, e := w.Model.Embed(facets)
	if e != nil {
		return nil, nil, nil, e
	}
	matches, e := retrieve.Nearest(w.Memory, vectors, w.Config, "observations", nil, 100)
	if e != nil {
		return nil, nil, nil, e
	}
	selected := map[string]bool{}
	candidates := []any{}
	offset := 0
	for _, o := range ex.Observations {
		scores := map[string]float64{}
		similarity := map[string]float64{}
		n := 1
		if o.Body != nil {
			n = 2
		}
		for _, ms := range matches[offset : offset+n] {
			for rank, m := range ms {
				scores[m.ID] += 1 / float64(w.Config.Search.RRFK+rank+1)
				if m.Score > similarity[m.ID] {
					similarity[m.ID] = m.Score
				}
			}
		}
		offset += n
		ids := []string{}
		for id := range scores {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool {
			if scores[ids[i]] == scores[ids[j]] {
				return ids[i] < ids[j]
			}
			return scores[ids[i]] > scores[ids[j]]
		})
		if len(ids) > w.Config.Ingest.MatchesPerCandidate {
			ids = ids[:w.Config.Ingest.MatchesPerCandidate]
		}
		for _, p := range o.Pages {
			lex, e := retrieve.Lexical(w.Memory, p.Name, "observations", nil, true)
			if e != nil {
				return nil, nil, nil, e
			}
			if len(lex) > 0 && !config.Contains(ids, lex[0]) {
				ids = append(ids, lex[0])
				break
			}
		}
		ms := []retrieve.Match{}
		for _, id := range ids {
			selected[id] = true
			ms = append(ms, retrieve.Match{ID: id, Score: similarity[id]})
		}
		var item map[string]any
		if e := json.Unmarshal([]byte(memory.JSON(o)), &item); e != nil {
			return nil, nil, nil, e
		}
		item["matches"] = ms
		candidates = append(candidates, item)
	}
	ids := []string{}
	for id := range selected {
		ids = append(ids, id)
	}
	rows, e := w.Memory.ObservationsByIDs(ids)
	return pages, rows, candidates, e
}

var slug = regexp.MustCompile(`^[a-z][a-z0-9-]{1,63}$`)

func validateProposal(p Proposal, ex Extraction, pages []memory.Page, observations []memory.Observation) error {
	existing := map[string]memory.Page{}
	parents := map[string]string{}
	for _, page := range pages {
		existing[page.Slug] = page
		if page.Parent != nil {
			parents[page.Slug] = *page.Parent
		} else {
			parents[page.Slug] = ""
		}
	}
	keys := map[string]string{}
	slugs := map[string]bool{}
	redirects := map[string]string{}
	for _, page := range p.Pages {
		_, exists := existing[page.Slug]
		switch {
		case page.Key == "" || keys[page.Key] != "":
			return fmt.Errorf("page %s has an empty or repeated key %q", page.Slug, page.Key)
		case slugs[page.Slug]:
			return fmt.Errorf("page %s is proposed twice", page.Slug)
		case !slug.MatchString(page.Slug):
			return fmt.Errorf("page slug %q must match %s", page.Slug, slug.String())
		case page.Action == "reuse" && !exists:
			return fmt.Errorf("page %s uses reuse but does not exist; use create", page.Slug)
		case page.Action == "create" && exists:
			return fmt.Errorf("page %s uses create but already exists; use reuse", page.Slug)
		case page.Action != "reuse" && page.Action != "create":
			return fmt.Errorf("page %s has action %q; use reuse or create", page.Slug, page.Action)
		case !config.Contains([]string{"actor", "artifact", "place", "event", "project", "topic"}, page.Category):
			return fmt.Errorf("page %s has category %q; categories are actor, artifact, place, event, project, topic", page.Slug, page.Category)
		}
		keys[page.Key] = page.Slug
		slugs[page.Slug] = true
		if page.Line != nil && retrieve.Normalize(*page.Line) == retrieve.Normalize(page.Slug) {
			return fmt.Errorf("page %s line only repeats its slug; describe the subject or use null", page.Slug)
		}
		if page.Action == "create" {
			parents[page.Slug] = "root"
		}
		if page.Parent != nil {
			if page.Slug == "root" {
				return fmt.Errorf("page root cannot have a parent")
			}
			parents[page.Slug] = *page.Parent
		}
		if len(page.MergeFrom) > 0 && page.Action != "reuse" {
			return fmt.Errorf("page %s has merge_from, so its action must be reuse", page.Slug)
		}
		for _, from := range page.MergeFrom {
			if from == "root" || from == page.Slug || redirects[from] != "" {
				return fmt.Errorf("page %s cannot merge from %s (root, itself, or already merged)", page.Slug, from)
			}
			if _, ok := existing[from]; !ok {
				return fmt.Errorf("page %s merges from unknown page %s", page.Slug, from)
			}
			redirects[from] = page.Slug
		}
	}
	for from, into := range redirects {
		if slugs[from] {
			return fmt.Errorf("page %s is merged into %s and also proposed itself", from, into)
		}
		delete(parents, from)
		for id, parent := range parents {
			if parent == from {
				parents[id] = into
			}
		}
	}
	for id := range parents {
		seen := map[string]bool{}
		for x := id; x != ""; x = parents[x] {
			if seen[x] {
				return fmt.Errorf("page %s has a parent cycle through %s", id, x)
			}
			if _, ok := parents[x]; !ok {
				return fmt.Errorf("page %s has unknown parent %s", id, x)
			}
			seen[x] = true
		}
	}
	names := map[string]string{}
	nameCheck := func(owner string, aliases []string) error {
		if r := redirects[owner]; r != "" {
			owner = r
		}
		for _, name := range aliases {
			n := retrieve.Normalize(name)
			if n == "" {
				return fmt.Errorf("page %s has an empty alias", owner)
			}
			if old := names[n]; old != "" && old != owner {
				return fmt.Errorf("page %s name or alias %q already belongs to page %s; reuse that page or merge", owner, name, old)
			}
			names[n] = owner
		}
		return nil
	}
	for _, page := range pages {
		if err := nameCheck(page.Slug, append([]string{page.Slug}, page.Aliases...)); err != nil {
			return err
		}
	}
	for _, page := range p.Pages {
		if err := nameCheck(page.Slug, append([]string{page.Slug}, page.Aliases...)); err != nil {
			return err
		}
	}
	expected := map[string]bool{}
	for _, o := range ex.Observations {
		expected[o.CandidateID] = true
	}
	seen := map[string]bool{}
	used := map[string]bool{}
	allowed := map[string]bool{}
	for _, o := range observations {
		allowed[o.ID] = true
	}
	for i, op := range p.Observations {
		where := fmt.Sprintf("observations[%d] (%s)", i, op.Op)
		if !config.Contains([]string{"discard", "create", "attach_source", "update", "merge", "supersede"}, op.Op) {
			return fmt.Errorf("%s: op must be discard, create, attach_source, update, merge, or supersede", where)
		}
		if op.Reason == "" {
			return fmt.Errorf("%s needs a reason", where)
		}
		if len(op.CandidateIDs) == 0 && op.Op != "merge" {
			return fmt.Errorf("%s needs candidate_ids", where)
		}
		for _, id := range op.CandidateIDs {
			if !expected[id] {
				return fmt.Errorf("%s uses unknown candidate %s", where, id)
			}
			if seen[id] {
				return fmt.Errorf("%s repeats candidate %s; every candidate belongs to exactly one operation", where, id)
			}
			seen[id] = true
		}
		for _, id := range op.ObservationIDs {
			if !allowed[id] {
				return fmt.Errorf("%s uses unknown existing observation %s", where, id)
			}
			if used[id] {
				return fmt.Errorf("%s repeats existing observation %s; each may be used at most once", where, id)
			}
			used[id] = true
		}
		n := len(op.ObservationIDs)
		if ((op.Op == "discard" || op.Op == "create") && n != 0) || ((op.Op == "attach_source" || op.Op == "update") && n != 1) || (op.Op == "merge" && (n < 1 || n+len(op.CandidateIDs) < 2)) || (op.Op == "supersede" && n < 1) {
			return fmt.Errorf("%s has %d observation_ids; discard and create take none, attach_source and update exactly one, supersede one or more, merge one or more with at least two items in total", where, n)
		}
		if op.Op == "discard" || op.Op == "attach_source" {
			if op.Line != "" || op.Body != nil || op.Happened != nil || op.Claimant != nil || op.Authority != "" || op.Kind != "" || op.Confidence != 0 || op.Weight != 0 || op.Durability != nil || len(op.PageKeys) != 0 {
				return fmt.Errorf("%s must leave line, body, scores, and page_keys null or empty", where)
			}
			continue
		}
		if err := op.Fields.Validate(); err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
		if len(op.PageKeys) == 0 {
			return fmt.Errorf("%s needs page_keys", where)
		}
		for _, k := range op.PageKeys {
			if keys[k] == "" {
				return fmt.Errorf("%s uses unknown page key %q; use a key from pages", where, k)
			}
		}
	}
	missing := []string{}
	for id := range expected {
		if !seen[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("candidates %s appear in no operation; every candidate must appear exactly once", strings.Join(missing, ", "))
	}
	return nil
}
func integrationSchema() map[string]any {
	s := model.Schema(new(Proposal))
	props := s["properties"].(map[string]any)
	obs := props["observations"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	for _, key := range []string{"line", "authority", "kind", "confidence", "weight"} {
		field := obs[key].(map[string]any)
		obs[key] = map[string]any{"anyOf": []any{field, map[string]any{"type": "null"}}}
	}
	return s
}
func (w *Worker) source(e inputlog.Entry, src inputlog.Source) error {
	ex, err := w.extract(e, src)
	if err != nil {
		return err
	}
	if err = w.embedChunks(e.Seq); err != nil {
		return err
	}
	if err = w.pendingStructured(); err != nil {
		return err
	}
	return w.integrate(e, src, ex)
}
func (w *Worker) integrate(e inputlog.Entry, src inputlog.Source, ex Extraction) error {
	proposal := Proposal{Pages: []PageProposal{}, Observations: []Operation{}}
	if len(ex.Observations) > 0 {
		pages, rows, candidates, err := w.context(ex)
		if err != nil {
			return err
		}
		sources := []any{}
		seen := map[int64]bool{}
		for _, o := range rows {
			for _, seq := range o.Contributions {
				if seen[seq] {
					continue
				}
				seen[seq] = true
				var p string
				if err = w.Memory.DB.QueryRow("SELECT payload FROM inputlog.entries WHERE seq=?", seq).Scan(&p); err != nil {
					return err
				}
				var old inputlog.Source
				if err = json.Unmarshal([]byte(p), &old); err != nil {
					return err
				}
				sources = append(sources, map[string]any{"id": fmt.Sprintf("src:%d", seq), "home": old.Home, "cwd": old.Metadata["cwd"]})
			}
		}
		input := map[string]any{"source": map[string]any{"id": fmt.Sprintf("src:%d", e.Seq), "kind": src.Kind, "label": src.Label, "home": src.Home, "happened": src.Happened, "metadata": src.Metadata, "digest": ex.Digest}, "candidates": candidates, "existing": map[string]any{"directory": pages, "observations": rows, "sources": sources}}
		proposal, err = model.StructuredSchema[Proposal](w.Model, "integration", integrationSystem+w.identity(), memory.JSON(input), integrationSchema(), func(p Proposal) error { return validateProposal(p, ex, pages, rows) })
		if err != nil {
			return err
		}
	}
	usedKeys, parents := map[string]bool{}, map[string]bool{}
	for _, op := range proposal.Observations {
		for _, key := range op.PageKeys {
			usedKeys[key] = true
		}
	}
	for _, p := range proposal.Pages {
		if p.Parent != nil {
			parents[*p.Parent] = true
		}
	}
	kept := []PageProposal{}
	for _, p := range proposal.Pages {
		if p.Action == "reuse" || usedKeys[p.Key] || parents[p.Slug] {
			kept = append(kept, p)
		}
	}
	proposal.Pages = kept
	inputs := []string{}
	indexes := []int{}
	for i, o := range proposal.Observations {
		if o.Op != "discard" && o.Op != "attach_source" {
			inputs = append(inputs, (memory.Observation{Fields: o.Fields}).Text())
			indexes = append(indexes, i)
		}
	}
	vectors, err := w.Model.Embed(inputs)
	if err != nil {
		return err
	}
	by := map[int][]float64{}
	for i, idx := range indexes {
		by[idx] = vectors[i]
	}
	return w.Memory.Tx(func(c *sql.Conn) error {
		if err := w.applyProposal(c, e, ex, proposal, by); err != nil {
			return err
		}
		if e.Kind == "note" {
			if err := memory.Exec(c, "INSERT OR REPLACE INTO extraction VALUES(?,?)", e.Seq, memory.JSON(ex)); err != nil {
				return err
			}
		}
		return w.done(c, e.Seq, "")
	})
}
func (w *Worker) applyProposal(c *sql.Conn, e inputlog.Entry, ex Extraction, p Proposal, vectors map[int][]float64) error {
	pageKeys := map[string]string{}
	pending := map[string]PageProposal{}
	for _, page := range p.Pages {
		pageKeys[page.Key] = page.Slug
		if page.Action == "create" {
			pending[page.Slug] = page
		}
	}
	for len(pending) > 0 {
		progress := false
		for id, page := range pending {
			parent := "root"
			if page.Parent != nil {
				parent = *page.Parent
			}
			if _, wait := pending[parent]; wait {
				continue
			}
			if err := memory.Exec(c, "INSERT INTO pages VALUES(?,?,?,?,?,NULL,0.5,?)", id, page.Category, page.Line, memory.JSON(page.Aliases), parent, e.Seq); err != nil {
				return err
			}
			delete(pending, id)
			progress = true
		}
		if !progress {
			return fmt.Errorf("page hierarchy cycle")
		}
	}
	for _, page := range p.Pages {
		if page.Action != "reuse" {
			continue
		}
		for _, from := range page.MergeFrom {
			if err := mergePage(c, from, page.Slug, e.Seq); err != nil {
				return err
			}
		}
		var aliases string
		var oldLine, oldParent *string
		if err := c.QueryRowContext(context.Background(), "SELECT aliases,line,parent FROM pages WHERE slug=?", page.Slug).Scan(&aliases, &oldLine, &oldParent); err != nil {
			return err
		}
		var a []string
		if err := json.Unmarshal([]byte(aliases), &a); err != nil {
			return err
		}
		for _, x := range page.Aliases {
			if !config.Contains(a, x) {
				a = append(a, x)
			}
		}
		lineSame := page.Line == nil || (oldLine != nil && *page.Line == *oldLine)
		parentSame := page.Parent == nil || (oldParent != nil && *page.Parent == *oldParent)
		if lineSame && parentSame && memory.JSON(a) == aliases {
			continue
		}
		if err := memory.Exec(c, "UPDATE pages SET line=coalesce(?,line),aliases=?,parent=coalesce(?,parent),seq=? WHERE slug=?", page.Line, memory.JSON(a), page.Parent, e.Seq, page.Slug); err != nil {
			return err
		}
	}
	evidence := map[string][]string{}
	for _, o := range ex.Observations {
		evidence[o.CandidateID] = o.Evidence
	}
	for i, op := range p.Observations {
		if op.Op == "discard" {
			continue
		}
		old := op.ObservationIDs
		chunks := []string{}
		for _, id := range op.CandidateIDs {
			chunks = append(chunks, evidence[id]...)
		}
		pages := []string{}
		for _, key := range op.PageKeys {
			pages = append(pages, pageKeys[key])
		}
		id := ""
		if op.Op == "attach_source" {
			id = old[0]
			if err := memory.Exec(c, "UPDATE observations SET seq=? WHERE id=?", e.Seq, id); err != nil {
				return err
			}
		} else if op.Op == "update" {
			id = old[0]
			if err := memory.Exec(c, "UPDATE observations SET value=?,seq=? WHERE id=?", memory.JSON(op.Fields), e.Seq, id); err != nil {
				return err
			}
			if err := memory.Exec(c, "DELETE FROM observation_pages WHERE observation=?", id); err != nil {
				return err
			}
		} else {
			id = memory.ID()
			if err := memory.Exec(c, "INSERT INTO observations VALUES(?,?,?,?,NULL,NULL)", id, memory.JSON(op.Fields), e.Seq, e.Created); err != nil {
				return err
			}
			for _, from := range old {
				if err := memory.Exec(c, "INSERT OR IGNORE INTO contributions SELECT ?,seq FROM contributions WHERE observation=?", id, from); err != nil {
					return err
				}
				if err := memory.Exec(c, "INSERT OR IGNORE INTO evidence SELECT ?,chunk FROM evidence WHERE observation=?", id, from); err != nil {
					return err
				}
				if op.Op == "merge" {
					if err := memory.Exec(c, "INSERT OR IGNORE INTO observation_pages SELECT ?,page FROM observation_pages WHERE observation=?", id, from); err != nil {
						return err
					}
				}
				if err := memory.Exec(c, "UPDATE observations SET replaced_by=?,seq=? WHERE id=?", id, e.Seq, from); err != nil {
					return err
				}
			}
		}
		if err := memory.Exec(c, "INSERT OR IGNORE INTO contributions VALUES(?,?)", id, e.Seq); err != nil {
			return err
		}
		for _, page := range pages {
			if err := memory.Exec(c, "INSERT OR IGNORE INTO observation_pages VALUES(?,?)", id, page); err != nil {
				return err
			}
		}
		for _, ch := range chunks {
			if err := memory.Exec(c, "INSERT OR IGNORE INTO evidence VALUES(?,?)", id, ch); err != nil {
				return err
			}
		}
		if v, ok := vectors[i]; ok {
			if err := putVector(c, id, w.Config, v); err != nil {
				return err
			}
		}
	}
	return nil
}
func mergePage(c *sql.Conn, from, into string, seq int64) error {
	if from == "root" || from == into {
		return fmt.Errorf("invalid page merge")
	}
	var a, b string
	if err := c.QueryRowContext(context.Background(), "SELECT aliases FROM pages WHERE slug=? AND replaced_by IS NULL", from).Scan(&a); err != nil {
		return err
	}
	if err := c.QueryRowContext(context.Background(), "SELECT aliases FROM pages WHERE slug=? AND replaced_by IS NULL", into).Scan(&b); err != nil {
		return err
	}
	var aliases, other []string
	if err := json.Unmarshal([]byte(a), &aliases); err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(b), &other); err != nil {
		return err
	}
	for _, name := range append([]string{from}, aliases...) {
		if !config.Contains(other, name) {
			other = append(other, name)
		}
	}
	for _, stmt := range []struct {
		q    string
		args []any
	}{{"INSERT OR IGNORE INTO observation_pages SELECT observation,? FROM observation_pages WHERE page=?", []any{into, from}}, {"UPDATE observations SET seq=? WHERE id IN (SELECT observation FROM observation_pages WHERE page=?)", []any{seq, from}}, {"DELETE FROM observation_pages WHERE page=?", []any{from}}, {"UPDATE pages SET parent=?,seq=? WHERE parent=?", []any{into, seq, from}}, {"UPDATE pages SET replaced_by=?,seq=? WHERE slug=?", []any{into, seq, from}}, {"UPDATE pages SET aliases=?,seq=? WHERE slug=?", []any{memory.JSON(other), seq, into}}} {
		if e := memory.Exec(c, stmt.q, stmt.args...); e != nil {
			return e
		}
	}
	return nil
}
