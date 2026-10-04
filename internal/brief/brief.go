package brief

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/markschroedr/pd-memory/internal/config"
	"github.com/markschroedr/pd-memory/internal/memory"
	"github.com/markschroedr/pd-memory/internal/model"
	"github.com/markschroedr/pd-memory/internal/retrieve"
)

type Args struct {
	Page          string `json:"page,omitempty"`
	Folder        string `json:"folder,omitempty"`
	IncludeGlobal *bool  `json:"include_global,omitempty"`
	Budget        int    `json:"budget,omitempty"`
	Since         *int64 `json:"since,omitempty"`
	Compose       bool   `json:"compose,omitempty"`
}
type Result struct {
	Text      string   `json:"text"`
	Seq       int64    `json:"seq"`
	Truncated bool     `json:"truncated"`
	Next      []string `json:"next"`
	Tokens    int      `json:"tokens"`
	Cost      float64  `json:"cost_usd,omitempty"`
}
type ranked struct {
	memory.Observation
	Rank float64
}
type Builder struct {
	Text   string
	Tokens int
	Budget int
}

func Estimate(s string) int { return (len(strings.Fields(s))*4 + 2) / 3 }
func (b *Builder) Add(s string) bool {
	n := Estimate(s)
	if b.Tokens+n > b.Budget {
		return false
	}
	b.Text += s
	b.Tokens += n
	return true
}
func Date(s *string) string {
	if s == nil || len(*s) < 10 {
		return ""
	}
	t, e := time.Parse("2006-01-02", (*s)[:10])
	if e != nil {
		return ""
	}
	format := "2 Jan"
	if t.Year() != time.Now().Year() {
		format += " 2006"
	}
	return t.Format(format)
}
func line(o ranked, c *config.Config) string {
	marker := ""
	if o.Body != nil {
		marker = "+"
	}
	date := Date(o.Happened)
	if date != "" {
		date = "; " + date
	}
	return fmt.Sprintf("\n- %s [%d%s] %s (%d%s)", o.ID, c.Bucket(o.Rank), marker, o.Line, len(o.Sources), date)
}
func rows(s *memory.Store, c *config.Config) ([]ranked, error) {
	obs, e := s.Observations(true)
	if e != nil {
		return nil, e
	}
	now, e := s.Present()
	if e != nil {
		return nil, e
	}
	out := []ranked{}
	for _, o := range obs {
		rank, e := c.Evaluate(c.CallSites.BriefStanding, retrieve.Variables(o, c, 0, now))
		if e != nil {
			return nil, e
		}
		out = append(out, ranked{o, rank * 1.15})
	}
	return out, nil
}
func matchName(folder, name string) float64 {
	clean := func(s string) string {
		return strings.Map(func(r rune) rune {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				return unicode.ToLower(r)
			}
			return -1
		}, s)
	}
	a, b := []rune(clean(folder)), []rune(clean(name))
	if len(a) < 3 || len(b) < 3 {
		return 0
	}
	cost := make([]int, len(b)+1)
	for i := range cost {
		cost[i] = i
	}
	for i, x := range a {
		prev := cost[0]
		cost[0] = i + 1
		for j, y := range b {
			old := cost[j+1]
			delta := 0
			if x != y {
				delta = 1
			}
			cost[j+1] = min(cost[j+1]+1, cost[j]+1, prev+delta)
			prev = old
		}
	}
	score := 1 - float64(cost[len(b)])/float64(max(len(a), len(b)))
	prefix := 0
	for prefix < min(len(a), len(b)) && a[prefix] == b[prefix] {
		prefix++
	}
	if prefix >= 5 && float64(prefix)/float64(min(len(a), len(b))) >= .6 {
		score = math.Max(score, .8)
	}
	return score
}
func FolderSelection(s *memory.Store, folder string) (map[string]float64, map[string]bool, error) {
	folder, e := filepath.Abs(folder)
	if e != nil {
		return nil, nil, e
	}
	pages, e := s.Pages()
	if e != nil {
		return nil, nil, e
	}
	parts := strings.Split(filepath.Clean(folder), string(filepath.Separator))
	scores := map[string]float64{}
	parent := map[string]string{}
	for _, p := range pages {
		if p.Parent != nil {
			parent[p.Slug] = *p.Parent
		}
	}
	for _, p := range pages {
		if p.Slug == "root" {
			continue
		}
		score := 0.
		for i := len(parts) - 1; i >= 0; i-- {
			for _, n := range append([]string{p.Slug}, p.Aliases...) {
				score = math.Max(score, matchName(parts[i], n)*math.Pow(.85, float64(len(parts)-1-i)))
			}
		}
		if score < .7 {
			continue
		}
		desc, e := s.Descendants(p.Slug)
		if e != nil {
			return nil, nil, e
		}
		for _, id := range desc {
			depth := 0
			for x := id; x != p.Slug && x != ""; x = parent[x] {
				depth++
			}
			scores[id] = math.Max(scores[id], score/float64(depth+1))
		}
	}
	hidden := map[string]bool{}
	excluded, e := s.Query("SELECT entry FROM focus WHERE folder=?", folder)
	if e != nil {
		return nil, nil, e
	}
	for _, r := range excluded {
		id := r["entry"].(string)
		hidden[id] = true
		_, e := s.Page(id)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return nil, nil, e
		}
		if e == nil {
			desc, e := s.Descendants(id)
			if e != nil {
				return nil, nil, e
			}
			for _, p := range desc {
				hidden[p] = true
				delete(scores, p)
			}
		}
	}
	return scores, hidden, nil
}
func projectRows(all []ranked, scores map[string]float64, hidden map[string]bool) []ranked {
	out := []ranked{}
	for _, o := range all {
		if hidden[o.ID] {
			continue
		}
		affinity := 0.
		ps := []string{}
		hide := false
		for _, p := range o.Pages {
			if hidden[p] {
				hide = true
			}
			if score := scores[p]; score > 0 {
				affinity = math.Max(affinity, score)
				ps = append(ps, p)
			}
		}
		if hide || affinity == 0 {
			continue
		}
		o.Rank *= 1 + affinity
		o.Pages = ps
		out = append(out, o)
	}
	return out
}
func Current(s *memory.Store, c *config.Config, folder string, budget int) (Result, []string, error) {
	all, e := rows(s, c)
	if e != nil {
		return Result{}, nil, e
	}
	var scores map[string]float64
	if folder != "" {
		var hidden map[string]bool
		scores, hidden, e = FolderSelection(s, folder)
		if e != nil {
			return Result{}, nil, e
		}
		all = projectRows(all, scores, hidden)
	}
	shown := map[string]bool{}
	r, e := assemble(s, c, all, budget, "", len(all), scores, false, shown)
	ids := []string{}
	for id := range shown {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return r, ids, e
}
func stored(s *memory.Store, c *config.Config, scope string, exclude map[string]bool) (*memory.View, error) {
	if c.Compose.Mode == "off" {
		return nil, nil
	}
	vs, e := s.Views()
	if e != nil {
		return nil, e
	}
	for _, v := range vs {
		if v.Kind != "composition" || v.Scope != scope || strings.TrimSpace(v.Text) == "" {
			continue
		}
		valid, e := s.Valid(v)
		if e != nil {
			return nil, e
		}
		if !valid {
			continue
		}
		for _, id := range v.InputIDs {
			if exclude[id] {
				return nil, nil
			}
		}
		return &v, nil
	}
	return nil, nil
}
func Run(s *memory.Store, m *model.Client, c *config.Config, args Args) (Result, error) {
	budget := args.Budget
	if budget == 0 {
		budget = c.Brief.Budget
		if args.Folder != "" {
			budget = 8000
		}
	}
	if budget < 200 {
		return Result{}, fmt.Errorf("brief budget must be at least 200")
	}
	all, e := rows(s, c)
	if e != nil {
		return Result{}, e
	}
	seq, e := s.Seq()
	if e != nil {
		return Result{}, e
	}
	if args.Since != nil {
		if *args.Since < 0 {
			return Result{}, fmt.Errorf("since must be nonnegative")
		}
		b := Builder{Budget: budget}
		b.Add(fmt.Sprintf("# Memory changed since %d", *args.Since))
		obs, e := s.Observations(false)
		if e != nil {
			return Result{}, e
		}
		type change struct {
			seq      int64
			text, id string
		}
		changes := []change{}
		for _, o := range obs {
			if o.Seq > *args.Since {
				tail := ""
				if o.ReplacedBy != nil {
					tail = " → " + *o.ReplacedBy
				}
				if o.Forgotten != nil {
					tail += " (forgotten)"
				}
				changes = append(changes, change{o.Seq, "\n- " + o.ID + tail + " " + o.Line, o.ID})
			}
		}
		pages, e := s.Pages()
		if e != nil {
			return Result{}, e
		}
		for _, p := range pages {
			if p.Seq > *args.Since {
				txt := "\n- " + p.Slug
				if p.Line != nil {
					txt += " — " + *p.Line
				}
				changes = append(changes, change{p.Seq, txt, p.Slug})
			}
		}
		sort.Slice(changes, func(i, j int) bool {
			if changes[i].seq == changes[j].seq {
				return changes[i].id < changes[j].id
			}
			return changes[i].seq < changes[j].seq
		})
		r := Result{Seq: seq, Next: []string{}}
		for _, ch := range changes {
			if !b.Add(ch.text) {
				r.Truncated = true
				r.Seq = min(r.Seq, max(*args.Since, ch.seq-1))
				r.Next = append(r.Next, ch.id)
			}
		}
		r.Text = b.Text
		r.Tokens = b.Tokens
		return r, nil
	}
	if args.Page != "" {
		desc, e := s.Descendants(args.Page)
		if e != nil {
			return Result{}, e
		}
		set := map[string]bool{}
		for _, p := range desc {
			set[p] = true
		}
		rs := []ranked{}
		for _, o := range all {
			ps := []string{}
			for _, p := range o.Pages {
				if set[p] {
					ps = append(ps, p)
				}
			}
			if len(ps) > 0 {
				o.Pages = ps
				rs = append(rs, o)
			}
		}
		return assemble(s, c, rs, budget, "# "+args.Page, len(rs), nil, false, map[string]bool{})
	}
	if args.Folder != "" {
		folder, e := filepath.Abs(args.Folder)
		if e != nil {
			return Result{}, e
		}
		scores, hidden, e := FolderSelection(s, folder)
		if e != nil {
			return Result{}, e
		}
		rs := projectRows(all, scores, hidden)
		shown := map[string]bool{}
		projectBudget := budget
		if args.IncludeGlobal == nil || *args.IncludeGlobal {
			projectBudget = budget / 2
		}
		v, e := stored(s, c, "project:"+folder, map[string]bool{})
		if e != nil {
			return Result{}, e
		}
		var project Result
		if v != nil {
			project = Result{Text: "# Project: " + filepath.Base(folder) + "\n\n" + v.Text, Seq: seq, Tokens: Estimate(v.Text), Next: []string{}}
			for _, id := range v.Citations {
				shown[id] = true
			}
		} else {
			project, e = assemble(s, c, rs, projectBudget, "# Project: "+filepath.Base(folder), len(rs), scores, false, shown)
			if e != nil {
				return Result{}, e
			}
		}
		if args.IncludeGlobal != nil && !*args.IncludeGlobal {
			return project, nil
		}
		global, e := global(s, c, all, budget-projectBudget, shown)
		if e != nil {
			return Result{}, e
		}
		global.Text += "\n\n" + project.Text
		global.Tokens = Estimate(global.Text)
		global.Truncated = global.Truncated || project.Truncated
		global.Next = append(global.Next, project.Next...)
		if len(global.Next) > c.Brief.NextCap {
			global.Next = global.Next[:c.Brief.NextCap]
		}
		return global, nil
	}
	return global(s, c, all, budget, map[string]bool{})
}

// RecallArgs asks for memory about one request or topic. Folder names the host's standing brief.
type RecallArgs struct {
	Queries []string `json:"queries"`
	Pages   []string `json:"pages,omitempty"`
	Budget  int      `json:"budget,omitempty"`
	Folder  string   `json:"folder,omitempty"`
}

var cited = regexp.MustCompile(`\b[0-9a-f]{8}\b`)

// Recall ranks observations and source passages for a request within a budget. Observations the
// standing brief already shows are named by id only, so a recall next to that brief adds new facts.
func Recall(s *memory.Store, m *model.Client, c *config.Config, args RecallArgs) (Result, error) {
	queries := args.Queries
	if len(queries) == 0 {
		return Result{}, fmt.Errorf("recall needs at least one query")
	}
	budget := args.Budget
	if budget == 0 {
		budget = 1500
	}
	if budget < 200 {
		return Result{}, fmt.Errorf("recall budget must be at least 200")
	}
	standing, e := Run(s, m, c, Args{Folder: args.Folder})
	if e != nil {
		return Result{}, e
	}
	shown := map[string]bool{}
	for _, id := range cited.FindAllString(standing.Text, -1) {
		shown[id] = true
	}
	hits, e := retrieve.Search(s, m, c, queries, args.Pages)
	if e != nil {
		return Result{}, e
	}
	rs, already, passages := []ranked{}, []string{}, []retrieve.Hit{}
	for _, h := range hits.Hits {
		switch {
		case h.Observation == nil:
			passages = append(passages, h)
		case shown[h.ID]:
			already = append(already, h.ID)
		default:
			rs = append(rs, ranked{*h.Observation, h.Score})
		}
	}
	// Pages follow their best hit, so the most relevant subject leads and gets the larger share.
	relevance := map[string]float64{}
	for _, o := range rs {
		for _, p := range o.Pages {
			relevance[p] = math.Max(relevance[p], o.Rank)
		}
	}
	r, e := assemble(s, c, rs, budget, "# Recall: "+strings.Join(queries, "; "), len(rs), relevance, false, map[string]bool{})
	if e != nil {
		return Result{}, e
	}
	b := Builder{Text: r.Text, Tokens: r.Tokens, Budget: budget}
	if len(already) > 0 {
		b.Add("\n\nAlso matched, already in the standing brief: " + strings.Join(already, ", "))
	}
	// Source passages catch facts that never became observations; they fill the remaining budget.
	for i, h := range passages {
		line := "\n- " + h.ID + " " + h.Line
		if i == 0 {
			line = "\n\n## Source passages" + line
		}
		if !b.Add(line) {
			r.Truncated = true
			break
		}
	}
	r.Text, r.Tokens, r.Cost = b.Text, b.Tokens, hits.Cost
	return r, nil
}
func global(s *memory.Store, c *config.Config, all []ranked, budget int, excluded map[string]bool) (Result, error) {
	v, e := stored(s, c, "global", excluded)
	if e != nil {
		return Result{}, e
	}
	timelineExcluded := map[string]bool{}
	for id := range excluded {
		timelineExcluded[id] = true
	}
	// Only observations the composition cites count as shown; inputs it dropped stay eligible elsewhere.
	if v != nil {
		for _, id := range v.Citations {
			timelineExcluded[id] = true
		}
	}
	history, recent, open, truncated, e := Timeline(s, c, timelineExcluded)
	if e != nil {
		return Result{}, e
	}
	shown := map[string]bool{}
	for id := range excluded {
		shown[id] = true
	}
	for id := range open {
		shown[id] = true
	}
	var standing Result
	if v != nil {
		seq, e := s.Seq()
		if e != nil {
			return Result{}, e
		}
		standing = Result{Text: v.Text, Seq: seq, Next: []string{}}
		for _, id := range v.Citations {
			shown[id] = true
		}
		directory, e := directory(s, c, all, shown, int(float64(budget)*c.Brief.DirectoryShare))
		if e != nil {
			return Result{}, e
		}
		standing.Text += directory
	} else {
		rs := []ranked{}
		for _, o := range all {
			if !shown[o.ID] {
				rs = append(rs, o)
			}
		}
		standing, e = assemble(s, c, rs, budget, "", c.Brief.PerPageCap, nil, true, shown)
		if e != nil {
			return Result{}, e
		}
	}
	// Readers need memory's own present to interpret dated and stable-form facts, such as a birth year.
	present, e := s.Present()
	if e != nil {
		return Result{}, e
	}
	parts := []string{"# Memory (as of " + present.Format("2006-01-02") + ")"}
	for _, txt := range []string{history, standing.Text, recent} {
		if strings.TrimSpace(txt) != "" {
			parts = append(parts, strings.TrimSpace(txt))
		}
	}
	standing.Text = strings.Join(parts, "\n\n")
	standing.Tokens = Estimate(standing.Text)
	standing.Truncated = standing.Truncated || truncated
	return standing, nil
}
func assemble(s *memory.Store, c *config.Config, rs []ranked, budget int, title string, cap int, scores map[string]float64, dir bool, shown map[string]bool) (Result, error) {
	pages, e := s.Pages()
	if e != nil {
		return Result{}, e
	}
	now, e := s.Present()
	if e != nil {
		return Result{}, e
	}
	type pageRows struct {
		Page  memory.Page
		Rows  []ranked
		Score float64
	}
	items := []pageRows{}
	parent := map[string]string{}
	for _, p := range pages {
		if p.Parent != nil {
			parent[p.Slug] = *p.Parent
		}
		obs := []ranked{}
		freshness, weight := 0., 0.
		inbound := map[string]bool{}
		for _, o := range rs {
			if config.Contains(o.Pages, p.Slug) {
				obs = append(obs, o)
				freshness = math.Max(freshness, retrieve.Freshness(o.Happened, o.Entered, o.Durability, now))
				weight = math.Max(weight, o.Weight)
				for _, other := range o.Pages {
					if other != p.Slug {
						inbound[other] = true
					}
				}
			}
		}
		if len(obs) == 0 {
			continue
		}
		sort.Slice(obs, func(i, j int) bool {
			if obs[i].Rank == obs[j].Rank {
				return obs[i].Entered > obs[j].Entered
			}
			return obs[i].Rank > obs[j].Rank
		})
		v := config.Variables()
		v["weight"] = weight
		v["freshness"] = freshness
		v["observations"] = float64(len(obs))
		v["inbound"] = float64(len(inbound))
		score, e := c.Evaluate(c.CallSites.PageRank, v)
		if e != nil {
			return Result{}, e
		}
		if scores != nil {
			score *= scores[p.Slug]
		}
		items = append(items, pageRows{p, obs, score})
	}
	sort.Slice(items, func(i, j int) bool {
		if scores != nil && scores[items[i].Page.Slug] != scores[items[j].Page.Slug] {
			return scores[items[i].Page.Slug] > scores[items[j].Page.Slug]
		}
		if items[i].Score == items[j].Score {
			return items[i].Page.Slug < items[j].Page.Slug
		}
		return items[i].Score > items[j].Score
	})
	reserve := 0
	if dir {
		reserve = int(float64(budget) * c.Brief.DirectoryShare)
	}
	main := budget - reserve
	spineLimit := int(float64(main) * c.Brief.SpineShare)
	spine := Estimate(title)
	included := []pageRows{}
	headings := map[string]string{}
	for _, item := range items {
		heading := fmt.Sprintf(" %s (%s)", item.Page.Slug, item.Page.Category)
		if item.Page.Line != nil {
			heading += " — " + *item.Page.Line
		}
		heading += fmt.Sprintf(" (%d)", len(item.Rows))
		n := Estimate("\n##" + heading)
		if (spine+n > spineLimit && len(included) > 0) || spine+n > main {
			break
		}
		included = append(included, item)
		headings[item.Page.Slug] = heading
		spine += n
	}
	covered := map[string]bool{}
	for _, item := range included {
		for _, o := range item.Rows {
			covered[o.ID] = true
		}
	}
	directoryText := ""
	if dir {
		directoryText, e = directory(s, c, rs, covered, reserve)
		if e != nil {
			return Result{}, e
		}
	}
	spent := spine
	dirTokens := Estimate(directoryText)
	selected := map[string][]ranked{}
	rendered := map[string]bool{}
	pageSpent := map[string]int{}
	totalAffinity := 0.
	for _, item := range included {
		if scores != nil {
			totalAffinity += math.Pow(scores[item.Page.Slug], float64(c.Brief.ProjectAffinityExponent))
		}
	}
	// Skip duplicate rows without spending a page slot, so shared claims refill rather than leaving holes.
	positions := map[string]int{}
	for round := 0; round < cap; round++ {
		progress := false
		for _, item := range included {
			id := item.Page.Slug
			pos := positions[id]
			for pos < len(item.Rows) && rendered[item.Rows[pos].ID] {
				pos++
			}
			positions[id] = pos + 1
			if pos >= len(item.Rows) {
				continue
			}
			o := item.Rows[pos]
			n := Estimate(line(o, c))
			if spent+n > budget-dirTokens {
				continue
			}
			if scores != nil && totalAffinity > 0 && float64(pageSpent[id]+n) > float64(budget-spine)*math.Pow(scores[id], float64(c.Brief.ProjectAffinityExponent))/totalAffinity {
				continue
			}
			selected[id] = append(selected[id], o)
			pageSpent[id] += n
			spent += n
			rendered[o.ID] = true
			shown[o.ID] = true
			progress = true
		}
		if !progress {
			break
		}
	}
	b := Builder{Budget: budget}
	b.Add(title)
	includedSet := map[string]bool{}
	children := map[string][]pageRows{}
	for _, item := range included {
		includedSet[item.Page.Slug] = true
	}
	for _, item := range included {
		p := parent[item.Page.Slug]
		for p != "" && !includedSet[p] {
			p = parent[p]
		}
		children[p] = append(children[p], item)
	}
	var emit func(string, int)
	emit = func(p string, depth int) {
		for _, item := range children[p] {
			b.Add("\n" + strings.Repeat("#", min(6, depth+2)) + headings[item.Page.Slug])
			for _, o := range selected[item.Page.Slug] {
				b.Add(line(o, c))
			}
			emit(item.Page.Slug, depth+1)
		}
	}
	emit("", 0)
	b.Add(directoryText)
	omitted := []ranked{}
	for _, o := range rs {
		if !rendered[o.ID] {
			omitted = append(omitted, o)
		}
	}
	sort.Slice(omitted, func(i, j int) bool { return omitted[i].Rank > omitted[j].Rank })
	next := []string{}
	for _, o := range omitted {
		if len(next) == c.Brief.NextCap {
			break
		}
		next = append(next, o.ID)
	}
	seq, e := s.Seq()
	if e != nil {
		return Result{}, e
	}
	return Result{Text: b.Text, Seq: seq, Tokens: b.Tokens, Truncated: len(omitted) > 0 || len(included) < len(items), Next: next}, nil
}
func directory(s *memory.Store, c *config.Config, rs []ranked, covered map[string]bool, budget int) (string, error) {
	pages, e := s.Pages()
	if e != nil {
		return "", e
	}
	parent := map[string]string{}
	lines := map[string]*string{}
	for _, p := range pages {
		if p.Parent != nil {
			parent[p.Slug] = *p.Parent
		}
		lines[p.Slug] = p.Line
	}
	top := func(slug string) string {
		for slug != "" && parent[slug] != "root" {
			slug = parent[slug]
		}
		return slug
	}
	type subject struct {
		IDs     map[string]bool
		Score   float64
		Covered bool
	}
	subjects := map[string]*subject{}
	for _, o := range rs {
		for _, p := range o.Pages {
			t := top(p)
			if t == "" {
				continue
			}
			if subjects[t] == nil {
				subjects[t] = &subject{IDs: map[string]bool{}}
			}
			x := subjects[t]
			x.IDs[o.ID] = true
			x.Score = math.Max(x.Score, o.Rank)
			x.Covered = x.Covered || covered[o.ID]
		}
	}
	ids := []string{}
	for id, x := range subjects {
		if !x.Covered && len(x.IDs) >= c.Brief.DirectoryMinObservations {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		if subjects[ids[i]].Score == subjects[ids[j]].Score {
			return ids[i] < ids[j]
		}
		return subjects[ids[i]].Score > subjects[ids[j]].Score
	})
	b := Builder{Budget: budget}
	if len(ids) > 0 {
		b.Add("\n## Other subjects")
	}
	added := false
	for _, id := range ids {
		line := "\n- " + id
		if lines[id] != nil {
			line += " — " + *lines[id]
		}
		line += fmt.Sprintf(" (%d)", len(subjects[id].IDs))
		if b.Add(line) {
			added = true
		}
	}
	if !added {
		return "", nil
	}
	return b.Text, nil
}

var brackets = regexp.MustCompile(`\s*\[[^\[\]]*\]`)

func Day(date, zone string) string {
	if len(date) == 10 {
		return date
	}
	t, e := time.Parse(time.RFC3339Nano, date)
	if e != nil {
		return ""
	}
	loc, _ := time.LoadLocation(zone)
	return t.In(loc).Format("2006-01-02")
}
func Parent(id string) string {
	if start, _ := Period(id); start == "" {
		return ""
	}
	if strings.HasPrefix(id, "day:") {
		day := id[4:]
		n := 0
		fmt.Sscanf(day[8:], "%d", &n)
		return fmt.Sprintf("week:%s-%d", day[:7], min(4, (n+6)/7))
	}
	if strings.HasPrefix(id, "week:") {
		return "month:" + id[5:12]
	}
	if strings.HasPrefix(id, "month:") {
		return "year:" + id[6:10]
	}
	return ""
}
func Period(id string) (string, string) {
	kind, key, _ := strings.Cut(id, ":")
	switch kind {
	case "day":
		if _, e := time.Parse("2006-01-02", key); e != nil {
			return "", ""
		}
		return key, key
	case "year":
		if _, e := time.Parse("2006", key); e != nil {
			return "", ""
		}
		return key + "-01-01", key + "-12-31"
	case "month":
		t, e := time.Parse("2006-01", key)
		if e != nil {
			return "", ""
		}
		return key + "-01", t.AddDate(0, 1, -1).Format("2006-01-02")
	case "week":
		if len(key) != 9 || key[7] != '-' || key[8] < '1' || key[8] > '4' {
			return "", ""
		}
		month := key[:7]
		if _, e := time.Parse("2006-01", month); e != nil {
			return "", ""
		}
		n := int(key[8] - '0')
		end := fmt.Sprintf("%s-%02d", month, n*7)
		if n == 4 {
			_, end = Period("month:" + month)
		}
		return fmt.Sprintf("%s-%02d", month, (n-1)*7+1), end
	}
	return "", ""
}
func Cover(views []memory.View, before string) []memory.View {
	available := map[string]memory.View{}
	for _, v := range views {
		_, end := Period(v.ID)
		if v.Kind != "composition" && end < before {
			available[v.ID] = v
		}
	}
	year := 0
	fmt.Sscanf(before[:4], "%d", &year)
	monthStart, _ := time.Parse("2006-01-02", before[:7]+"-01")
	previousMonth := monthStart.AddDate(0, 0, -1).Format("2006-01")
	weekStart, _ := Period(Parent("day:" + before))
	t, _ := time.Parse("2006-01-02", weekStart)
	recentStart, _ := Period(Parent("day:" + t.AddDate(0, 0, -1).Format("2006-01-02")))
	selected := map[string]memory.View{}
	for _, v := range views {
		if v.Kind != "day" || v.ID[4:] >= before {
			continue
		}
		day := v.ID[4:]
		desired := v.ID
		y := 0
		fmt.Sscanf(day[:4], "%d", &y)
		if y < year-1 {
			desired = "year:" + day[:4]
		} else if day[:7] < previousMonth {
			desired = "month:" + day[:7]
		} else if day < recentStart {
			desired = Parent(v.ID)
		}
		chosen := v
		for id := v.ID; id != ""; id = Parent(id) {
			if x, ok := available[id]; ok {
				chosen = x
			}
			if id == desired {
				break
			}
		}
		selected[chosen.ID] = chosen
	}
	out := []memory.View{}
	for _, v := range selected {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].ID[strings.IndexByte(out[i].ID, ':')+1:] < out[j].ID[strings.IndexByte(out[j].ID, ':')+1:]
	})
	return out
}
func Timeline(s *memory.Store, c *config.Config, excluded map[string]bool) (string, string, map[string]bool, bool, error) {
	views, e := s.Views()
	if e != nil {
		return "", "", nil, false, e
	}
	today := Day(time.Now().UTC().Format(time.RFC3339Nano), c.Timeline.Timezone)
	cover := Cover(views, today)
	historyNodes := []memory.View{}
	recentNodes := []memory.View{}
	latestSeq := int64(0)
	for _, v := range views {
		if v.Kind == "day" {
			latestSeq = max(latestSeq, v.Seq)
		}
	}
	for _, v := range cover {
		if v.Text == "" {
			continue
		}
		if v.Kind == "month" || v.Kind == "year" {
			historyNodes = append(historyNodes, v)
		} else {
			recentNodes = append(recentNodes, v)
		}
	}
	truncated := false
	historyLines := []string{}
	for _, v := range historyNodes {
		historyLines = append(historyLines, "- "+v.ID+" "+brackets.ReplaceAllString(v.Text, ""))
	}
	historySize := func() int { return Estimate("## History\n" + strings.Join(historyLines, "\n")) }
	for i, v := range historyNodes {
		if historySize() <= c.Timeline.HistoryBudget {
			break
		}
		historyLines[i] = "- " + v.ID + " " + brackets.ReplaceAllString(v.Headline, "")
	}
	for len(historyLines) > 0 && historySize() > c.Timeline.HistoryBudget {
		historyLines = historyLines[1:]
		truncated = true
	}
	historyText := ""
	if len(historyLines) > 0 {
		historyText = "## History\n" + strings.Join(historyLines, "\n")
	}
	obs, e := s.Observations(true)
	if e != nil {
		return "", "", nil, false, e
	}
	sort.Slice(obs, func(i, j int) bool { return obs[i].Seq > obs[j].Seq })
	open := Builder{Budget: c.Timeline.OpenBudget}
	open.Add("### Open days")
	openIDs := map[string]bool{}
	for _, o := range obs {
		if excluded[o.ID] || o.Seq <= latestSeq {
			continue
		}
		date := Date(o.Happened)
		if date != "" {
			date = " (" + date + ")"
		}
		line := "\n- " + o.ID + " " + o.Line + date
		if Estimate("## Recent\n"+open.Text+line) <= c.Timeline.RecentBudget && open.Add(line) {
			openIDs[o.ID] = true
		} else {
			truncated = true
		}
	}
	recentLines := []string{}
	for _, v := range recentNodes {
		recentLines = append(recentLines, "- "+v.ID+" "+brackets.ReplaceAllString(v.Headline, ""))
	}
	for len(recentLines) > 0 && Estimate(strings.Join(append([]string{"## Recent"}, append(recentLines, open.Text)...), "\n")) > c.Timeline.RecentBudget {
		recentLines = recentLines[1:]
		truncated = true
	}
	recent := "## Recent\n" + strings.Join(recentLines, "\n")
	if len(openIDs) > 0 {
		recent += "\n" + open.Text
	}
	return historyText, recent, openIDs, truncated, nil
}
