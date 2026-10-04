package cli

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/markschroedr/pd-memory/internal/brief"
	"github.com/markschroedr/pd-memory/internal/config"
	"github.com/markschroedr/pd-memory/internal/fold"
	"github.com/markschroedr/pd-memory/internal/importer"
	inputlog "github.com/markschroedr/pd-memory/internal/log"
	"github.com/markschroedr/pd-memory/internal/memory"
	"github.com/markschroedr/pd-memory/internal/model"
)

const Help = `pd-memory — progressive-disclosure memory

Usage: pd-memory COMMAND [options] [--config FILE] [--json]

Agent commands:
  recall --query TEXT [--query FACET] [--page PAGE] [--budget N]
  brief [--page PAGE | --folder PATH | --since SEQ] [--budget N] [--compose]
  open ID [--history] [--full]
  browse [--view pages|page|history] [--id PAGE] [--period month] [--limit N] [--offset N]
  note --line TEXT --page PAGE --actor user|agent [observation fields] [--wait]
  edit ID --reason TEXT --actor user|agent [observation fields | --parent PAGE | --merge-into PAGE] [--supersede] [--wait]
  forget ID --reason TEXT --actor user|agent [--wait]
  focus hide|show ID --folder PATH [--wait]

Operator commands:
  ingest (--path FILE | --stdin | --dialogue-json | --manifest FILE) --kind KIND --label TEXT [--wait]
  import sessions [--pi PATH] [--claude-code PATH] [--codex PATH] [--routes FILE] [--after ISO] [--dry-run | --wait]
  worker, status, retry [SEQ], captured-session-entries --session KEY
  maintain, rebuild (paid), reindex, doctor [--live], stats, catalog

Observation fields: --line, --body, --happened, --claimant, --kind,
--confidence. Edit also accepts --authority and --clear-body.
All command inputs also accept --input-json JSON. catalog exposes agent JSON schemas.
Config: PD_MEMORY_CONFIG or ./pd-memory.toml. Writes return queued unless --wait.
Only ingestion wakes maintenance. Separate workspace directories are privacy boundaries.
`

func Main(args []string) int {
	code, e := run(args)
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		var conflict *inputlog.Conflict
		if errors.As(e, &conflict) {
			return 2
		}
		return 1
	}
	return code
}
func flag(args *[]string, name string) (bool, error) {
	found := false
	for i := 0; i < len(*args); i++ {
		if (*args)[i] == name {
			if found {
				return false, fmt.Errorf("repeated %s", name)
			}
			found = true
			*args = append((*args)[:i], (*args)[i+1:]...)
			i--
		}
	}
	return found, nil
}
func configPath(args *[]string) (string, error) {
	path := os.Getenv("PD_MEMORY_CONFIG")
	if path == "" {
		path = "./pd-memory.toml"
	}
	seen := false
	for i := 0; i < len(*args); i++ {
		if (*args)[i] == "--config" {
			if seen || i+1 >= len(*args) {
				return "", fmt.Errorf("invalid --config")
			}
			seen = true
			path = (*args)[i+1]
			*args = append((*args)[:i], (*args)[i+2:]...)
			i--
		}
	}
	return path, nil
}
func print(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}
func run(args []string) (code int, err error) {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Print(Help)
		return 0, nil
	}
	command := args[0]
	args = args[1:]
	help, e := flag(&args, "--help")
	if e != nil {
		return 0, e
	}
	if help {
		fmt.Print(Help)
		return 0, nil
	}
	machine, e := flag(&args, "--json")
	if e != nil {
		return 0, e
	}
	path, e := configPath(&args)
	if e != nil {
		return 0, e
	}
	if command == "catalog" {
		typescript, e := flag(&args, "--typescript")
		if e != nil {
			return 0, e
		}
		if len(args) > 0 {
			if _, e = Parse(new(EmptyInput), args); e != nil {
				return 0, e
			}
		}
		catalog := FullCatalog()
		if typescript {
			_, e = fmt.Print(TypeScript(catalog))
			return 0, e
		}
		return 0, print(catalog)
	}
	if command == "import" {
		if len(args) == 0 || args[0] != "sessions" {
			return 0, fmt.Errorf("usage: import sessions")
		}
		args = args[1:]
	}
	input, e := find(command)
	if e != nil {
		return 0, e
	}
	if len(args) > 0 && args[0] == "--input-json" {
		machine = true
	}
	raw, e := Parse(input, args)
	if e != nil {
		return 0, e
	}
	c, e := config.Load(path)
	if e != nil {
		return 0, e
	}
	if command == "doctor" {
		in := input.(*DoctorInput)
		local := map[string]any{"workspace": c.Workspace.Dir, "provider": c.OpenAI.Provider, "base_url": c.OpenAI.BaseURL, "credential_env": c.OpenAI.APIKeyEnv, "credential_set": os.Getenv(c.OpenAI.APIKeyEnv) != "", "model": c.OpenAI.Model, "embedding_provider": c.Embeddings.Provider, "embedding_model": c.Embeddings.Model, "embedding_credential_set": c.Embeddings.Provider == "local_pplx" || os.Getenv(c.Embeddings.APIKeyEnv) != "", "service_tier": c.OpenAI.ServiceTier, "store": false, "retention_policy": c.OpenAI.RetentionPolicy, "retention_verified": c.OpenAI.RetentionVerified}
		if in.Live {
			lock, e := fold.Lock(c.Workspace.Dir, true)
			if e != nil {
				return 0, e
			}
			defer func() { err = errors.Join(err, fold.Release(c, lock)) }()
			w, e := fold.New(c)
			if e != nil {
				return 0, e
			}
			defer w.Close()
			out, e := model.Structured[model.ProbeOutput](w.Model, "probe", "Return the requested JSON only.", "Set ok to true.", func(v model.ProbeOutput) error {
				if !v.OK {
					return fmt.Errorf("probe not ok")
				}
				return nil
			})
			if e != nil {
				return 0, e
			}
			generation := w.Model.Last
			vectors, e := w.Model.Embed([]string{"pd-memory capability probe"})
			if e != nil {
				return 0, e
			}
			local["live"] = map[string]any{"generation": map[string]any{"ok": out.OK, "returned_model": generation.Model, "returned_service_tier": generation.Tier, "strict_structured_output": true, "store": false}, "embeddings": map[string]any{"returned_model": w.Model.Last.Model, "dimensions": len(vectors[0]), "provider": c.Embeddings.Provider, "zdr": c.Embeddings.ZDR, "allow_fallbacks": c.Embeddings.AllowFallbacks, "only": c.Embeddings.Only}}
			local["cost_usd"] = w.Model.Cost
		}
		return 0, print(local)
	}
	switch command {
	case "worker", "retry", "rebuild", "reindex":
		var out fold.Outcome
		switch command {
		case "worker":
			out, e = fold.Run(c, false, false)
		case "retry":
			out, e = fold.Retry(c, input.(*RetryInput).Seq)
		case "rebuild":
			fmt.Fprintln(os.Stderr, "Rebuild makes paid model calls; log.db remains unchanged.")
			out, e = fold.Rebuild(c)
		case "reindex":
			out, e = fold.Reindex(c)
		}
		if err := print(out); err != nil {
			return 0, err
		}
		if e != nil {
			return 0, e
		}
		if len(out.Failed) > 0 {
			return 1, nil
		}
		return 0, nil
	case "maintain":
		out, e := fold.Maintenance(c, "", 0, false)
		if err := print(out); err != nil {
			return 0, err
		}
		if e != nil {
			return 0, e
		}
		if len(out.Failed) > 0 {
			return 1, nil
		}
		return 0, nil
	case "captured-session-entries":
		in := input.(*CapturedInput)
		if in.Session == "" {
			return 0, fmt.Errorf("session required")
		}
		l, e := inputlog.Open(c.Workspace.Dir, false)
		if e != nil {
			if os.IsNotExist(e) {
				return 0, print(map[string]any{"session": in.Session, "entry_ids": []string{}})
			}
			return 0, e
		}
		defer l.Close()
		ids, e := l.Captured(in.Session)
		if e != nil {
			return 0, e
		}
		return 0, print(map[string]any{"session": in.Session, "entry_ids": ids})
	case "import":
		in := input.(*importer.Options)
		var l *inputlog.Store
		if !in.DryRun {
			l, e = inputlog.Open(c.Workspace.Dir, true)
			if e != nil {
				return 0, e
			}
			defer l.Close()
		}
		out, e := importer.Run(c, l, *in)
		if e != nil {
			return 0, e
		}
		result := map[string]any{"import": out}
		if !in.DryRun {
			if in.Wait {
				processed, e := fold.Wait(c, out.Seqs, true)
				result["fold"] = processed
				if err := print(result); err != nil {
					return 0, err
				}
				return 0, e
			}
			if e = fold.Wake(c); e != nil {
				return 0, e
			}
		}
		return 0, print(result)
	case "ingest":
		in := input.(*IngestInput)
		l, e := inputlog.Open(c.Workspace.Dir, true)
		if e != nil {
			return 0, e
		}
		defer l.Close()
		sources, e := ingest(*in)
		if e != nil {
			return 0, e
		}
		submitted := []inputlog.Submitted{}
		seqs := []int64{}
		actor := in.Actor
		if actor == "" {
			actor = "system"
		}
		for _, src := range sources {
			r, e := l.Append("source", actor, src)
			if e != nil {
				return 0, e
			}
			submitted = append(submitted, r)
			seqs = append(seqs, r.Seq)
		}
		return finish(c, map[string]any{"submitted": submitted}, seqs, in.Wait)
	case "note", "edit", "forget", "focus":
		return mutate(c, command, input, raw)
	}
	if command == "brief" && input.(*brief.Args).Compose {
		a := input.(*brief.Args)
		if a.Page != "" || a.Since != nil {
			return 0, fmt.Errorf("composition supports global or folder only")
		}
		folder := a.Folder
		if folder != "" {
			folder, e = filepath.Abs(folder)
			if e != nil {
				return 0, e
			}
		}
		budget := a.Budget
		if folder != "" && budget != 0 {
			budget /= 2
		}
		out, e := fold.Maintenance(c, folder, budget, true)
		if e != nil {
			return 0, e
		}
		fmt.Fprintf(os.Stderr, "Composition cost_usd=%.8f\n", out.Cost)
		// A manual composition is served even when automatic composition is off.
		copy := *c
		copy.Compose.Mode = "projects"
		c = &copy
	}
	s, e := memory.Open(c.Workspace.Dir, false)
	if e != nil {
		return 0, e
	}
	defer s.Close()
	switch command {
	case "recall":
		r, e := brief.Recall(s, &model.Client{Config: c}, c, *input.(*brief.RecallArgs))
		if e != nil {
			return 0, e
		}
		if machine {
			return 0, print(r)
		}
		fmt.Println(r.Text)
		fmt.Fprintf(os.Stderr, "cost_usd=%.8f\n", r.Cost)
		return 0, nil
	case "brief":
		r, e := brief.Run(s, &model.Client{Config: c}, c, *input.(*brief.Args))
		if e != nil {
			return 0, e
		}
		if machine {
			return 0, print(r)
		}
		fmt.Println(r.Text)
		return 0, nil
	case "open":
		r, e := open(s, *input.(*OpenInput))
		if e != nil {
			return 0, e
		}
		return 0, print(r)
	case "browse":
		r, e := browse(s, *input.(*BrowseInput))
		if e != nil {
			return 0, e
		}
		return 0, print(r)
	case "status":
		r, e := s.Query("SELECT l.seq,l.kind,coalesce(f.status,'queued') status,coalesce(f.attempts,0) attempts,f.error FROM inputlog.entries l LEFT JOIN fold_state f ON f.seq=l.seq ORDER BY l.seq")
		if e != nil {
			return 0, e
		}
		maintenance, e := s.Query("SELECT * FROM maintenance_errors ORDER BY id")
		if e != nil {
			return 0, e
		}
		cost, e := s.Cost()
		if e != nil {
			return 0, e
		}
		return 0, print(map[string]any{"entries": r, "maintenance_errors": maintenance, "cost_usd": cost})
	case "stats":
		out := map[string]any{}
		for _, table := range []string{"pages", "observations", "chunks", "contributions", "evidence", "views"} {
			var n int64
			if e = s.DB.QueryRow("SELECT count(*) FROM " + table).Scan(&n); e != nil {
				return 0, e
			}
			out[table] = n
		}
		r, e := s.Query("SELECT phase,model,tier,count(*) calls,sum(cost_usd) cost_usd FROM model_calls GROUP BY phase,model,tier")
		if e != nil {
			return 0, e
		}
		out["model_calls"] = r
		cost, e := s.Cost()
		if e != nil {
			return 0, e
		}
		out["cost_usd"] = cost
		return 0, print(out)
	}
	return 0, fmt.Errorf("unhandled command")
}
func finish(c *config.Config, result map[string]any, seqs []int64, wait bool) (int, error) {
	if wait {
		out, e := fold.Wait(c, seqs, true)
		result["fold"] = out
		result["status"] = "done"
		if e != nil {
			result["status"] = "failed"
		}
		if err := print(result); err != nil {
			return 0, err
		}
		return 0, e
	}
	if e := fold.Wake(c); e != nil {
		return 0, e
	}
	result["status"] = "queued"
	return 0, print(result)
}
func ingest(in IngestInput) ([]inputlog.Source, error) {
	if in.Manifest != "" {
		if in.Path != "" || in.Stdin || in.DialogueJSON {
			return nil, fmt.Errorf("manifest excludes other inputs")
		}
		var m struct {
			Sources []struct {
				Path       string  `json:"path"`
				Kind       string  `json:"kind"`
				Title      string  `json:"title"`
				Date       *string `json:"date"`
				Home       *string `json:"home"`
				ExternalID *string `json:"external_id"`
				SHA256     string  `json:"sha256"`
				ListedTime *string `json:"listed_time"`
				Selector   *int    `json:"selector"`
			} `json:"sources"`
		}
		b, e := os.ReadFile(in.Manifest)
		if e != nil {
			return nil, e
		}
		if e = model.Decode(b, &m); e != nil {
			return nil, e
		}
		sources := []inputlog.Source{}
		for _, item := range m.Sources {
			path := item.Path
			if !filepath.IsAbs(path) {
				path = filepath.Join(filepath.Dir(in.Manifest), path)
			}
			b, e := os.ReadFile(path)
			if e != nil {
				return nil, e
			}
			if item.SHA256 != "" && hash(b) != item.SHA256 {
				return nil, fmt.Errorf("hash mismatch for %s", path)
			}
			src := inputlog.Source{Kind: item.Kind, Text: string(b), Label: item.Title, Happened: item.Date, Home: item.Home, ExternalID: item.ExternalID, Metadata: map[string]any{"listed_time": item.ListedTime, "selector": item.Selector, "selector_scope": item.Date}, Participants: importer.Speakers(string(b))}
			if e = src.Validate(); e != nil {
				return nil, e
			}
			sources = append(sources, src)
		}
		sort.SliceStable(sources, func(i, j int) bool {
			a, b := "", ""
			if sources[i].Happened != nil {
				a = *sources[i].Happened
			}
			if sources[j].Happened != nil {
				b = *sources[j].Happened
			}
			return a < b
		})
		return sources, nil
	}
	count := 0
	for _, yes := range []bool{in.Path != "", in.Stdin, in.DialogueJSON} {
		if yes {
			count++
		}
	}
	if count != 1 {
		return nil, fmt.Errorf("choose path, stdin or dialogue-json")
	}
	var b []byte
	var e error
	if in.Path != "" {
		b, e = os.ReadFile(in.Path)
	} else {
		b, e = io.ReadAll(os.Stdin)
	}
	if e != nil {
		return nil, e
	}
	src := inputlog.Source{Kind: in.Kind, Text: string(b), Label: in.Label, Happened: in.Happened, Session: in.Session, Home: in.Home, ExternalID: in.ExternalID, Metadata: map[string]any{}, Participants: []string{}}
	if in.MetadataJSON != "" {
		if e = json.Unmarshal([]byte(in.MetadataJSON), &src.Metadata); e != nil || src.Metadata == nil {
			return nil, fmt.Errorf("metadata-json must be an object")
		}
	}
	if in.Kind == "meeting" || in.Kind == "conversation" {
		src.Participants = importer.Speakers(src.Text)
	}
	if in.DialogueJSON {
		var messages []importer.Message
		if strings.HasPrefix(strings.TrimSpace(string(b)), "[") {
			e = model.Decode(b, &messages)
		} else {
			var wrapped struct {
				Messages []importer.Message `json:"messages"`
			}
			e = model.Decode(b, &wrapped)
			messages = wrapped.Messages
		}
		if e != nil {
			return nil, e
		}
		d, e := importer.Prepare(messages)
		if e != nil {
			return nil, e
		}
		src.Text = d.Text
		src.Participants = d.Participants
		src.Metadata["dialogue"] = d.Stats
	}
	if e = src.Validate(); e != nil {
		return nil, e
	}
	return []inputlog.Source{src}, nil
}
func actor(s string) error {
	if s != "user" && s != "agent" {
		return fmt.Errorf("actor must be user or agent")
	}
	return nil
}
func mutate(c *config.Config, command string, input any, raw map[string]json.RawMessage) (int, error) {
	l, e := inputlog.Open(c.Workspace.Dir, true)
	if e != nil {
		return 0, e
	}
	defer l.Close()
	var payload any
	by := "agent"
	wait := false
	if command == "note" {
		in := input.(*NoteInput)
		by = in.Actor
		wait = in.Wait
		if e = actor(by); e != nil {
			return 0, e
		}
		if _, ok := raw["confidence"]; !ok {
			in.Confidence = 1
		}
		if _, ok := raw["kind"]; !ok {
			in.Kind = "fact"
		}
		in.Authority = by
		if e = in.Fields.Validate(); e != nil {
			return 0, e
		}
		if len(in.Pages) == 0 {
			return 0, fmt.Errorf("pages required")
		}
		text := (memory.Observation{Fields: in.Fields}).Text()
		payload = inputlog.Note{Fields: in.Fields, Pages: in.Pages, Text: text}
	} else {
		s, e := memory.Open(c.Workspace.Dir, false)
		if e != nil {
			return 0, e
		}
		defer s.Close()
		switch command {
		case "edit":
			in := input.(*EditInput)
			by = in.Actor
			wait = in.Wait
			if e = actor(by); e != nil {
				return 0, e
			}
			if in.Reason == "" {
				return 0, fmt.Errorf("reason required")
			}
			edit := inputlog.Edit{ID: in.ID, Reason: in.Reason, Pages: in.Pages, Supersede: in.Supersede, MergeInto: in.MergeInto, Parent: in.Parent, Patch: map[string]json.RawMessage{}}
			if in.MergeInto != "" || in.Parent != "" {
				if in.MergeInto != "" && in.Parent != "" {
					return 0, fmt.Errorf("choose merge-into or parent")
				}
				for _, f := range fields(reflectFields()) {
					if _, ok := raw[f.Name]; ok {
						return 0, fmt.Errorf("page edit cannot include observation fields")
					}
				}
				p, e := s.Page(in.ID)
				if e != nil {
					return 0, e
				}
				edit.Stamp = p.Seq
				target := in.MergeInto
				if target == "" {
					target = in.Parent
				}
				other, e := s.Page(target)
				if e != nil {
					return 0, e
				}
				edit.Text = fmt.Sprintf("%s page %s (%s) with target %s (%s). Reason: %s.", command, p.Slug, value(p.Line), other.Slug, value(other.Line), in.Reason)
			} else {
				o, e := s.Observation(in.ID)
				if e != nil {
					return 0, e
				}
				if o.ReplacedBy != nil || o.Forgotten != nil {
					return 0, fmt.Errorf("target is not current")
				}
				edit.Stamp = o.Seq
				for _, f := range fields(reflectFields()) {
					if v, ok := raw[f.Name]; ok {
						edit.Patch[f.Name] = v
					}
				}
				if len(edit.Patch) == 0 && in.Pages == nil && !in.Supersede {
					return 0, fmt.Errorf("edit needs changed fields")
				}
				edit.Text = fmt.Sprintf("Correct observation '%s' (body: %s) with fields %s and pages %s. Supersede: %t. Reason: %s.", o.Line, value(o.Body), memory.JSON(edit.Patch), memory.JSON(in.Pages), in.Supersede, in.Reason)
			}
			payload = edit
		case "forget":
			in := input.(*ForgetInput)
			by = in.Actor
			wait = in.Wait
			if e = actor(by); e != nil {
				return 0, e
			}
			if in.Reason == "" {
				return 0, fmt.Errorf("reason required")
			}
			meaning := ""
			if strings.Contains(in.ID, ":") && !strings.HasPrefix(in.ID, "src:") {
				start, end := brief.Period(in.ID)
				if start == "" || end == "" {
					return 0, fmt.Errorf("invalid timeline ID")
				}
				meaning = "Forget historical view " + in.ID + " for rebuilding"
			} else {
				o, e := s.Observation(in.ID)
				if e != nil {
					return 0, e
				}
				meaning = "Forget observation: " + o.Text()
			}
			payload = inputlog.Forget{ID: in.ID, Text: meaning + ". Reason: " + in.Reason, Reason: in.Reason}
		case "focus":
			in := input.(*FocusInput)
			wait = in.Wait
			if in.Actor != "" {
				by = in.Actor
			}
			if e = actor(by); e != nil {
				return 0, e
			}
			if !config.Contains([]string{"hide", "show"}, in.Action) || in.Folder == "" {
				return 0, fmt.Errorf("focus needs hide/show and folder")
			}
			folder, e := filepath.Abs(in.Folder)
			if e != nil {
				return 0, e
			}
			meaning := ""
			o, e := s.Observation(in.ID)
			if e != nil && !errors.Is(e, sql.ErrNoRows) {
				return 0, e
			}
			if e == nil {
				meaning = o.Text()
			} else {
				p, e := s.Page(in.ID)
				if e != nil {
					return 0, e
				}
				meaning = p.Slug + " — " + value(p.Line)
			}
			payload = inputlog.Focus{ID: in.ID, Text: in.Action + " " + meaning + " in project brief for " + folder, Folder: folder, Action: in.Action}
		}
	}
	r, e := l.Append(command, by, payload)
	if e != nil {
		return 0, e
	}
	return finish(c, map[string]any{"submitted": r}, []int64{r.Seq}, wait)
}
func value(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
