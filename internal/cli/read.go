package cli

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/markschroedr/pd-memory/internal/brief"
	"github.com/markschroedr/pd-memory/internal/config"
	"github.com/markschroedr/pd-memory/internal/memory"
)

func reflectFields() reflect.Type                     { return reflect.TypeOf(memory.Fields{}) }
func hash(b []byte) string                            { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func open(s *memory.Store, in OpenInput) (any, error) { return openEntry(s, in, map[string]bool{}) }
func openEntry(s *memory.Store, in OpenInput, seen map[string]bool) (any, error) {
	if in.ID == "" {
		return nil, fmt.Errorf("id required")
	}
	if seen[in.ID] {
		return map[string]any{"id": in.ID}, nil
	}
	seen[in.ID] = true
	if strings.HasPrefix(in.ID, "src:") {
		seq, idx := int64(0), 0
		if strings.Contains(in.ID, "/") {
			if _, e := fmt.Sscanf(in.ID, "src:%d/%d", &seq, &idx); e != nil {
				return nil, e
			}
			chunks, e := s.Chunks(seq)
			if e != nil {
				return nil, e
			}
			for i, ch := range chunks {
				if ch.Index == idx {
					out := map[string]any{"id": ch.ID, "kind": "chunk", "context": ch.Context, "text": ch.Text, "source": fmt.Sprintf("src:%d", seq)}
					if i > 0 {
						out["previous"] = chunks[i-1].ID
					}
					if i+1 < len(chunks) {
						out["next"] = chunks[i+1].ID
					}
					return out, nil
				}
			}
			return nil, fmt.Errorf("unknown chunk: %w", sql.ErrNoRows)
		}
		if _, e := fmt.Sscanf(in.ID, "src:%d", &seq); e != nil {
			return nil, e
		}
		var kind, actor, created, payload string
		if e := s.DB.QueryRow("SELECT kind,actor,created,payload FROM inputlog.entries WHERE seq=?", seq).Scan(&kind, &actor, &created, &payload); e != nil {
			return nil, e
		}
		var fields map[string]any
		if e := json.Unmarshal([]byte(payload), &fields); e != nil {
			return nil, e
		}
		if !in.Full {
			delete(fields, "text")
		}
		out := map[string]any{"id": in.ID, "kind": "source", "entry_kind": kind, "actor": actor, "created": created, "source": fields}
		var digest, participants string
		err := s.DB.QueryRow("SELECT json_extract(x.value,'$.source_digest'),json_extract(x.value,'$.participants') FROM extraction x JOIN fold_state f ON f.seq=x.seq WHERE f.status='done' AND x.seq=?", seq).Scan(&digest, &participants)
		if err != nil && err != sql.ErrNoRows {
			return nil, err
		}
		if err == nil {
			out["digest"] = digest
			var ps []string
			if e := json.Unmarshal([]byte(participants), &ps); e != nil {
				return nil, e
			}
			out["participants"] = ps
		}
		chunks, e := s.Chunks(seq)
		if e != nil {
			return nil, e
		}
		previews := []any{}
		for _, ch := range chunks {
			previews = append(previews, map[string]any{"id": ch.ID, "line": ch.Context})
		}
		out["chunks"] = previews
		if session, ok := fields["session"].(string); ok && session != "" {
			rows, e := s.Query("SELECT seq FROM inputlog.entries WHERE kind='source' AND json_extract(payload,'$.session')=? ORDER BY coalesce(json_extract(payload,'$.happened'),created),seq", session)
			if e != nil {
				return nil, e
			}
			for i, r := range rows {
				if r["seq"].(int64) == seq {
					if i > 0 {
						out["previous"] = fmt.Sprintf("src:%d", rows[i-1]["seq"])
					}
					if i+1 < len(rows) {
						out["next"] = fmt.Sprintf("src:%d", rows[i+1]["seq"])
					}
				}
			}
		}
		return out, nil
	}
	if strings.Contains(in.ID, ":") {
		views, e := s.Views()
		if e != nil {
			return nil, e
		}
		for _, v := range views {
			if v.ID != in.ID {
				continue
			}
			out := map[string]any{"id": v.ID, "kind": "timeline", "node_kind": v.Kind, "text": v.Text, "headline": v.Headline, "seq": v.Seq, "created": v.Created, "citations": v.Citations, "parent": brief.Parent(v.ID)}
			children := []string{}
			for _, child := range views {
				if brief.Parent(child.ID) == v.ID {
					children = append(children, child.ID)
				}
			}
			out["children"] = children
			parentAvailable := false
			for _, p := range views {
				if p.ID == brief.Parent(v.ID) {
					parentAvailable = true
				}
			}
			out["parent_available"] = parentAvailable
			if in.Evidence == nil || *in.Evidence {
				cited := []any{}
				no := false
				for _, id := range v.Citations {
					r, e := openEntry(s, OpenInput{ID: id, Evidence: &no}, seen)
					if e != nil && !errors.Is(e, sql.ErrNoRows) {
						return nil, e
					}
					if e == nil {
						cited = append(cited, r)
					}
				}
				out["cited"] = cited
			}
			return out, nil
		}
		return nil, fmt.Errorf("unknown view %s: %w", in.ID, sql.ErrNoRows)
	}
	o, e := s.Observation(in.ID)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return nil, e
	}
	if e == nil {
		var out map[string]any
		if e := json.Unmarshal([]byte(memory.JSON(o)), &out); e != nil {
			return nil, e
		}
		out["kind"] = "observation"
		out["observation_kind"] = o.Kind
		if o.Forgotten != nil {
			var p string
			if e = s.DB.QueryRow("SELECT payload FROM inputlog.entries WHERE seq=?", *o.Forgotten).Scan(&p); e != nil {
				return nil, e
			}
			var f map[string]any
			if e := json.Unmarshal([]byte(p), &f); e != nil {
				return nil, e
			}
			out["forgotten_reason"] = f["reason"]
		}
		if in.History {
			history := []any{}
			chain := []string{}
			current := o
			for {
				chain = append(chain, current.ID)
				for _, seq := range current.Contributions {
					var kind, actor, created, payload string
					if e = s.DB.QueryRow("SELECT kind,actor,created,payload FROM inputlog.entries WHERE seq=?", seq).Scan(&kind, &actor, &created, &payload); e != nil {
						return nil, e
					}
					var meaning map[string]any
					if e := json.Unmarshal([]byte(payload), &meaning); e != nil {
						return nil, e
					}
					if !in.Full {
						delete(meaning, "text")
					}
					history = append(history, map[string]any{"seq": seq, "kind": kind, "actor": actor, "created": created, "payload": meaning})
				}
				if current.ReplacedBy == nil {
					break
				}
				current, e = s.Observation(*current.ReplacedBy)
				if e != nil {
					return nil, e
				}
			}
			predecessors, e := s.Query("WITH RECURSIVE previous(id) AS (SELECT id FROM observations WHERE replaced_by=? UNION SELECT o.id FROM observations o JOIN previous p ON o.replaced_by=p.id) SELECT id FROM previous", o.ID)
			if e != nil {
				return nil, e
			}
			out["history"] = history
			out["replacement_chain"] = chain
			out["predecessors"] = predecessors
		}
		return out, nil
	}
	p, e := s.Page(in.ID)
	if e != nil {
		if !errors.Is(e, sql.ErrNoRows) {
			return nil, e
		}
		return nil, fmt.Errorf("unknown id %s: %w", in.ID, sql.ErrNoRows)
	}
	obs, e := s.Observations(true)
	if e != nil {
		return nil, e
	}
	pages, e := s.Pages()
	if e != nil {
		return nil, e
	}
	items, children := []any{}, []any{}
	for _, o := range obs {
		if config.Contains(o.Pages, p.Slug) {
			items = append(items, map[string]any{"id": o.ID, "line": o.Line, "has_body": o.Body != nil, "happened": o.Happened, "seq": o.Seq})
		}
	}
	for _, child := range pages {
		if child.Parent != nil && *child.Parent == p.Slug {
			children = append(children, map[string]any{"slug": child.Slug, "line": child.Line})
		}
	}
	var out map[string]any
	if e := json.Unmarshal([]byte(memory.JSON(p)), &out); e != nil {
		return nil, e
	}
	out["id"] = p.Slug
	out["kind"] = "page"
	out["subpages"] = children
	out["observations"] = items
	return out, nil
}
func browse(s *memory.Store, in BrowseInput) (any, error) {
	if in.View == "" {
		in.View = "pages"
	}
	if in.Period == "" {
		in.Period = "month"
	}
	if in.Limit == 0 {
		in.Limit = 40
	}
	if in.Limit < 1 || in.Limit > 100 || in.Offset < 0 || !config.Contains([]string{"pages", "page", "history"}, in.View) {
		return nil, fmt.Errorf("invalid browse input")
	}
	items := []any{}
	out := map[string]any{}
	if in.View == "history" {
		if !config.Contains([]string{"day", "week", "month", "year"}, in.Period) {
			return nil, fmt.Errorf("invalid period")
		}
		vs, e := s.Views()
		if e != nil {
			return nil, e
		}
		sort.Slice(vs, func(i, j int) bool { return vs[i].ID > vs[j].ID })
		for _, v := range vs {
			if v.Kind == in.Period {
				start, _ := brief.Period(v.ID)
				items = append(items, map[string]any{"id": v.ID, "kind": "timeline", "line": v.Headline, "happened": start})
			}
		}
	} else {
		pages, e := s.Pages()
		if e != nil {
			return nil, e
		}
		obs, e := s.Observations(true)
		if e != nil {
			return nil, e
		}
		visible := map[string]bool{"root": true}
		parents := map[string]string{}
		for _, p := range pages {
			if p.Parent != nil {
				parents[p.Slug] = *p.Parent
			}
		}
		for _, o := range obs {
			for _, p := range o.Pages {
				for x := p; x != ""; x = parents[x] {
					visible[x] = true
				}
			}
		}
		if in.View == "page" {
			page, e := s.Page(in.ID)
			if e != nil {
				return nil, e
			}
			out["page"] = map[string]any{"id": page.Slug, "kind": "page", "line": page.Line, "parent": page.Parent, "category": page.Category}
			children := []any{}
			for _, p := range pages {
				if p.Parent != nil && *p.Parent == in.ID && visible[p.Slug] {
					children = append(children, map[string]any{"id": p.Slug, "kind": "page", "line": p.Line, "parent": p.Parent, "category": p.Category})
				}
			}
			out["children"] = children
			sort.Slice(obs, func(i, j int) bool {
				if obs[i].Rated().Reach == obs[j].Rated().Reach {
					return obs[i].Entered > obs[j].Entered
				}
				return obs[i].Rated().Reach > obs[j].Rated().Reach
			})
			for _, o := range obs {
				if config.Contains(o.Pages, in.ID) {
					items = append(items, map[string]any{"id": o.ID, "kind": "observation", "line": o.Line, "happened": o.Happened})
				}
			}
		} else {
			var scores map[string]float64
			if in.Folder != "" {
				scores, _, e = brief.FolderSelection(s, in.Folder)
				if e != nil {
					return nil, e
				}
				sort.Slice(pages, func(i, j int) bool {
					if scores[pages[i].Slug] == scores[pages[j].Slug] {
						return pages[i].Slug < pages[j].Slug
					}
					return scores[pages[i].Slug] > scores[pages[j].Slug]
				})
			}
			for _, p := range pages {
				if p.Slug != "root" && visible[p.Slug] && (scores == nil || scores[p.Slug] > 0) {
					items = append(items, map[string]any{"id": p.Slug, "kind": "page", "line": p.Line, "parent": p.Parent, "category": p.Category})
				}
			}
		}
	}
	out["more"] = len(items) > in.Offset+in.Limit
	start := min(in.Offset, len(items))
	end := min(start+in.Limit, len(items))
	out["items"] = items[start:end]
	return out, nil
}
