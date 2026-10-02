package view

import (
	"database/sql"
	_ "embed"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/markschroedr/pd-memory/internal/brief"
	"github.com/markschroedr/pd-memory/internal/config"
	inputlog "github.com/markschroedr/pd-memory/internal/log"
	"github.com/markschroedr/pd-memory/internal/memory"
	"github.com/markschroedr/pd-memory/internal/model"
)

//go:embed system.txt
var timelineSystem string

//go:embed compose_system.txt
var composeSystem string
var groups = regexp.MustCompile(`\[([^\[\]]*)\]`)

func Citations(text string) []string {
	set := map[string]bool{}
	out := []string{}
	for _, g := range groups.FindAllStringSubmatch(text, -1) {
		for _, id := range strings.Split(g[1], ",") {
			id = strings.TrimSpace(id)
			if id != "" && !set[id] {
				set[id] = true
				out = append(out, id)
			}
		}
	}
	return out
}
func clean(text string, allowed map[string]bool) (string, error) {
	total, dropped := 0, 0
	text = groups.ReplaceAllStringFunc(strings.TrimSpace(text), func(group string) string {
		g := groups.FindStringSubmatch(group)
		valid := []string{}
		for _, id := range strings.Split(g[1], ",") {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			total++
			if allowed[id] {
				valid = append(valid, id)
			} else {
				dropped++
			}
		}
		if len(valid) == 0 {
			return ""
		}
		return "\x01" + strings.Join(valid, ", ") + "\x02"
	})
	text = strings.NewReplacer("[", "", "]", "", "\x01", "[", "\x02", "]").Replace(text)
	if total > 0 && float64(dropped)/float64(total) > .2 {
		return "", fmt.Errorf("invalid citations: %d/%d exceed 20%%", dropped, total)
	}
	return strings.TrimSpace(text), nil
}

type Output struct {
	Text     string `json:"text"`
	Headline string `json:"headline"`
}
type Engine struct {
	Store  *memory.Store
	Config *config.Config
	Model  *model.Client
}
type Result struct {
	Built  []string  `json:"built"`
	Failed []Failure `json:"failed"`
	Cost   float64   `json:"cost_usd"`
}
type Failure struct {
	ID    string `json:"id"`
	Error string `json:"error"`
}

// All views share validation and publication. Generation happens before the short write transaction.
func (e *Engine) generate(id, kind, scope, system, input string, ids []string, seq int64) (memory.View, error) {
	allowed := map[string]bool{}
	for _, id := range ids {
		allowed[id] = true
	}
	var text, headline string
	if len(ids) > 0 {
		if kind == "composition" {
			var prior string
			var last error
			for attempt := 0; attempt < 2; attempt++ {
				in := input
				if attempt > 0 {
					in += "\n\nReturn a corrected complete composition.\nValidation error: " + last.Error() + "\nPrior composition:\n" + prior
				}
				raw, err := e.Model.Generate("compose", system, in, nil, attempt > 0)
				if err != nil {
					return memory.View{}, err
				}
				prior = raw
				text, err = clean(raw, allowed)
				if err == nil {
					break
				}
				last = err
				if attempt == 1 {
					return memory.View{}, err
				}
			}
		} else {
			out, err := model.Structured[Output](e.Model, "timeline_"+kind, system, input, func(o Output) error { _, err := clean(o.Text+"\n\n# HEADLINE\n\n"+o.Headline, allowed); return err })
			if err != nil {
				return memory.View{}, err
			}
			joined, _ := clean(out.Text+"\n\n# HEADLINE\n\n"+out.Headline, allowed)
			parts := strings.SplitN(joined, "# HEADLINE", 2)
			text = strings.TrimSpace(parts[0])
			if len(parts) > 1 {
				headline = strings.TrimSpace(parts[1])
			}
		}
	}
	v := memory.View{ID: id, Kind: kind, Scope: scope, Text: text, Headline: headline, InputIDs: ids, Citations: Citations(text + "\n" + headline), Seq: seq, Created: time.Now().UTC().Format(time.RFC3339Nano)}
	err := e.Store.Tx(func(c *sql.Conn) error {
		if err := memory.Exec(c, "INSERT INTO views VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET text=excluded.text,headline=excluded.headline,input_ids=excluded.input_ids,citations=excluded.citations,seq=excluded.seq,created=excluded.created", v.ID, v.Kind, v.Scope, v.Text, v.Headline, memory.JSON(v.InputIDs), memory.JSON(v.Citations), v.Seq, v.Created); err != nil {
			return err
		}
		return memory.Exec(c, "DELETE FROM maintenance_errors WHERE id=?", id)
	})
	return v, err
}
func (e *Engine) Composition(folder string, target int) (memory.View, error) {
	scope := "global"
	if folder != "" {
		scope = "project:" + folder
	}
	r, ids, err := brief.Current(e.Store, e.Config, folder, target*e.Config.Brief.ComposeInputFactor)
	if err != nil {
		return memory.View{}, err
	}
	input := fmt.Sprintf("Current date: %s\nTarget length: about %d tokens.\nAllowed observation IDs: %s\n\n--- BEGIN SECTION ---\n%s\n--- END SECTION ---", time.Now().Format("2006-01-02"), target, strings.Join(ids, ", "), r.Text)
	return e.generate(scope, "composition", scope, composeSystem, input, ids, r.Seq)
}
func (e *Engine) failed(out *Result, id string, err error) error {
	fmt.Fprintf(os.Stderr, "Maintenance %s failed: %v\n", id, err)
	out.Failed = append(out.Failed, Failure{id, err.Error()})
	return e.Store.Tx(func(c *sql.Conn) error {
		return memory.Exec(c, "INSERT OR REPLACE INTO maintenance_errors VALUES(?,?,?)", id, err.Error(), time.Now().UTC().Format(time.RFC3339Nano))
	})
}
func (e *Engine) Maintain(entries []inputlog.Entry) (Result, error) {
	out := Result{Built: []string{}, Failed: []Failure{}}
	before := e.Model.Cost
	today := brief.Day(time.Now().UTC().Format(time.RFC3339Nano), e.Config.Timeline.Timezone)
	views, err := e.Store.Views()
	if err != nil {
		return out, err
	}
	nodes := map[string]memory.View{}
	for _, v := range views {
		nodes[v.ID] = v
	}
	periods := map[string]bool{}
	pendingDays := map[string]bool{}
	byDay := map[string][]inputlog.Entry{}
	for _, entry := range entries {
		var state string
		err = e.Store.DB.QueryRow("SELECT status FROM fold_state WHERE seq=?", entry.Seq).Scan(&state)
		if err != nil && err != sql.ErrNoRows {
			return out, err
		}
		pending := err == sql.ErrNoRows || state != "done"
		var payload map[string]any
		if err = model.Decode(entry.Payload, &payload); err != nil {
			return out, err
		}
		date := entry.Created
		if value, ok := payload["happened"].(string); ok {
			date = value
		}
		day := brief.Day(date, e.Config.Timeline.Timezone)
		if day == "" {
			continue
		}
		if pending {
			pendingDays[day] = true
			continue
		}
		byDay[day] = append(byDay[day], entry)
		for id := "day:" + day; id != ""; id = brief.Parent(id) {
			periods[id] = true
		}
	}
	ids := []string{}
	for id := range periods {
		ids = append(ids, id)
	}
	order := map[string]int{"day": 0, "week": 1, "month": 2, "year": 3}
	sort.Slice(ids, func(i, j int) bool {
		a, _, _ := strings.Cut(ids[i], ":")
		b, _, _ := strings.Cut(ids[j], ":")
		if order[a] == order[b] {
			return ids[i] < ids[j]
		}
		return order[a] < order[b]
	})
	current, _, err := brief.Current(e.Store, e.Config, "", e.Config.Brief.Budget)
	if err != nil {
		return out, err
	}
	for _, id := range ids {
		if _, ok := nodes[id]; ok {
			continue
		}
		start, end := brief.Period(id)
		if end >= today {
			continue
		}
		blocked := false
		for day := range pendingDays {
			if day >= start && day <= end {
				blocked = true
			}
		}
		if blocked {
			continue
		}
		kind, _, _ := strings.Cut(id, ":")
		children := []memory.View{}
		for _, child := range ids {
			if brief.Parent(child) != id {
				continue
			}
			v, ok := nodes[child]
			if !ok {
				blocked = true
				break
			}
			if v.Text != "" {
				children = append(children, v)
			}
		}
		if blocked {
			continue
		}
		allowed := []string{}
		digests := []any{}
		changes := []any{}
		sourceCount := 0
		weight := 0.
		periodSeq := int64(0)
		topByID := map[string]map[string]any{}
		for day, periodEntries := range byDay {
			if day < start || day > end {
				continue
			}
			for _, entry := range periodEntries {
				periodSeq = max(periodSeq, entry.Seq)
				var digest string
				err := e.Store.DB.QueryRow("SELECT json_extract(x.value,'$.source_digest') FROM extraction x JOIN fold_state f ON f.seq=x.seq WHERE f.status='done' AND x.seq=?", entry.Seq).Scan(&digest)
				if err != nil && err != sql.ErrNoRows {
					return out, err
				}
				if err == nil {
					sid := fmt.Sprintf("src:%d", entry.Seq)
					if kind == "day" {
						digests = append(digests, map[string]any{"id": sid, "digest": digest})
						allowed = append(allowed, sid)
					}
					sourceCount++
				}
				records, err := e.Store.Query("SELECT o.id,json_extract(o.value,'$.line') line,json_extract(o.value,'$.weight') weight FROM observations o JOIN contributions c ON c.observation=o.id WHERE c.seq=?", entry.Seq)
				if err != nil {
					return out, err
				}
				for _, r := range records {
					if kind == "day" {
						allowed = append(allowed, r["id"].(string))
					} else if kind == "month" || kind == "year" {
						topByID[r["id"].(string)] = r
					}
					if x, ok := r["weight"].(float64); ok {
						weight += x
					}
				}
				var intent map[string]any
				if err := model.Decode(entry.Payload, &intent); err != nil {
					return out, err
				}
				delete(intent, "text")
				delete(intent, "metadata")
				if kind == "day" {
					changes = append(changes, map[string]any{"seq": entry.Seq, "op": entry.Kind, "actor": entry.Actor, "meaning": intent, "observations": records})
				}
			}
		}
		top := []map[string]any{}
		for _, row := range topByID {
			top = append(top, row)
		}
		sort.Slice(top, func(i, j int) bool {
			a, _ := top[i]["weight"].(float64)
			b, _ := top[j]["weight"].(float64)
			if a == b {
				return top[i]["id"].(string) < top[j]["id"].(string)
			}
			return a > b
		})
		cap := 10
		if kind == "year" {
			cap = 20
		}
		if len(top) > cap {
			top = top[:cap]
		}
		for _, row := range top {
			allowed = append(allowed, row["id"].(string))
		}
		childContent := []any{}
		for _, v := range children {
			childContent = append(childContent, map[string]any{"id": v.ID, "text": v.Text})
			allowed = append(allowed, v.ID)
			periodSeq = max(periodSeq, v.Seq)
		}
		var previous any
		if kind == "day" {
			t, _ := time.Parse("2006-01-02", start)
			priorID := "day:" + t.AddDate(0, 0, -1).Format("2006-01-02")
			if v, ok := nodes[priorID]; ok {
				periodSeq = max(periodSeq, v.Seq)
				previous = map[string]any{"id": v.ID, "text": v.Text}
				allowed = append(allowed, v.ID)
			}
		}
		earlier := []any{}
		existing := []memory.View{}
		for _, v := range nodes {
			existing = append(existing, v)
		}
		for _, v := range brief.Cover(existing, start) {
			earlier = append(earlier, map[string]any{"id": v.ID, "text": v.Text})
		}
		input := map[string]any{"content_input": map[string]any{"period": map[string]any{"id": id, "kind": kind, "starts": start, "ends": end}, "source_count": sourceCount, "summed_change_weight": weight, "sources": digests, "children": childContent, "observations": top, "changes": changes, "previous_day": previous}, "allowed_citation_ids": allowed, "context": map[string]any{"earlier_nodes": earlier, "current_state": current.Text}}
		v, err := e.generate(id, kind, "global", timelineSystem, memory.JSON(input), allowed, periodSeq)
		if err != nil {
			if err = e.failed(&out, id, err); err != nil {
				return out, err
			}
			continue
		}
		nodes[id] = v
		out.Built = append(out.Built, id)
	}
	if e.Config.Compose.Mode != "off" {
		folders := []string{""}
		if e.Config.Compose.Mode == "projects" {
			folders = append(folders, e.Config.Compose.Folders...)
		}
		for _, folder := range folders {
			scope := "global"
			target := e.Config.Brief.Budget
			if folder != "" {
				scope = "project:" + folder
				target = 4000
			}
			v, exists := nodes[scope]
			valid := false
			if exists {
				valid, err = e.Store.Valid(v)
				if err != nil {
					return out, err
				}
			}
			due := !exists || !valid || strings.TrimSpace(v.Text) == ""
			if !due {
				rows, err := e.Store.Observations(false)
				if err != nil {
					return out, err
				}
				pageSet := map[string]float64{}
				if folder != "" {
					pageSet, _, err = brief.FolderSelection(e.Store, folder)
					if err != nil {
						return out, err
					}
				}
				n := 0
				for _, o := range rows {
					if o.Seq <= v.Seq {
						continue
					}
					eligible := folder == ""
					for _, p := range o.Pages {
						if pageSet[p] > 0 {
							eligible = true
						}
					}
					if eligible {
						n++
					}
				}
				due = n >= e.Config.Compose.MinChanges
			}
			if !due {
				continue
			}
			v, err := e.Composition(folder, target)
			if err != nil {
				if err = e.failed(&out, scope, err); err != nil {
					return out, err
				}
				continue
			}
			nodes[scope] = v
			out.Built = append(out.Built, scope)
		}
	}
	out.Cost = e.Model.Cost - before
	return out, nil
}
