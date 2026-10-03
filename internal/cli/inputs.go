package cli

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/markschroedr/pd-memory/internal/brief"
	"github.com/markschroedr/pd-memory/internal/importer"
	"github.com/markschroedr/pd-memory/internal/memory"
	"github.com/markschroedr/pd-memory/internal/model"
)

type OpenInput struct {
	ID       string `json:"id" cli:"pos"`
	History  bool   `json:"history,omitempty"`
	Full     bool   `json:"full,omitempty"`
	Evidence *bool  `json:"evidence,omitempty"`
}
type BrowseInput struct {
	View   string `json:"view,omitempty" jsonschema:"enum=pages,enum=page,enum=history"`
	ID     string `json:"id,omitempty"`
	Folder string `json:"folder,omitempty"`
	Period string `json:"period,omitempty" jsonschema:"enum=day,enum=week,enum=month,enum=year"`
	Limit  int    `json:"limit,omitempty"`
	Offset int    `json:"offset,omitempty"`
}
type NoteInput struct {
	memory.Fields
	Pages []string `json:"pages"`
	Actor string   `json:"actor" jsonschema:"enum=user,enum=agent"`
	Wait  bool     `json:"wait,omitempty"`
}
type EditInput struct {
	memory.Fields
	ID        string   `json:"id" cli:"pos"`
	Pages     []string `json:"pages,omitempty"`
	Actor     string   `json:"actor" jsonschema:"enum=user,enum=agent"`
	Reason    string   `json:"reason"`
	Supersede bool     `json:"supersede,omitempty"`
	MergeInto string   `json:"merge_into,omitempty"`
	Parent    string   `json:"parent,omitempty"`
	Wait      bool     `json:"wait,omitempty"`
}
type ForgetInput struct {
	ID     string `json:"id" cli:"pos"`
	Reason string `json:"reason"`
	Actor  string `json:"actor" jsonschema:"enum=user,enum=agent"`
	Wait   bool   `json:"wait,omitempty"`
}
type FocusInput struct {
	Action string `json:"action" cli:"pos" jsonschema:"enum=hide,enum=show"`
	ID     string `json:"id" cli:"pos"`
	Folder string `json:"folder"`
	Actor  string `json:"actor,omitempty" jsonschema:"enum=user,enum=agent"`
	Wait   bool   `json:"wait,omitempty"`
}
type IngestInput struct {
	Path         string  `json:"path,omitempty"`
	Stdin        bool    `json:"stdin,omitempty"`
	DialogueJSON bool    `json:"dialogue_json,omitempty"`
	Manifest     string  `json:"manifest,omitempty"`
	Kind         string  `json:"kind,omitempty"`
	Label        string  `json:"label,omitempty"`
	Happened     *string `json:"happened,omitempty"`
	Session      *string `json:"session,omitempty"`
	Home         *string `json:"home,omitempty"`
	ExternalID   *string `json:"external_id,omitempty"`
	MetadataJSON string  `json:"metadata_json,omitempty"`
	Actor        string  `json:"actor,omitempty"`
	Wait         bool    `json:"wait,omitempty"`
}
type DoctorInput struct {
	Live bool `json:"live,omitempty"`
}
type RetryInput struct {
	Seq *int64 `json:"seq,omitempty" cli:"pos"`
}
type CapturedInput struct {
	Session string `json:"session"`
}
type EmptyInput struct{}
type Definition struct {
	Name         string         `json:"name"`
	Description  string         `json:"description"`
	InputSchema  map[string]any `json:"inputSchema"`
	ResultSchema map[string]any `json:"resultSchema"`
	New          func() any     `json:"-"`
}

func definitions() []Definition {
	return []Definition{
		{Name: "recall", Description: "Recall memory for a request or topic: observations grouped by page, then source passages, within a token budget. Pass for with the request in plain words, queries with short facets, or both. Recall whenever prior knowledge could change your work, and again when the conversation turns to a new topic. Observations already in the standing brief are listed by id only. When the person's circumstances could matter, consider also recalling related facts the request itself does not mention.", New: func() any { return new(brief.RecallArgs) }},
		{Name: "brief", Description: "Read the standing, folder, page, or since brief. Plain reads never generate text. compose regenerates the current-state scope.", New: func() any { return new(brief.Args) }},
		{Name: "open", Description: "Open a page, observation, source, chunk, or timeline view. history shows contributions and replacements; full includes raw source text.", New: func() any { return new(OpenInput) }},
		{Name: "browse", Description: "Browse pages, page observations, or history headlines with pagination. No model calls.", New: func() any { return new(BrowseInput) }},
		{Name: "note", Description: "Append a direct observation with user or agent authority. Returns queued unless wait is set.", New: func() any { return new(NoteInput) }},
		{Name: "edit", Description: "Correct or supersede an observation, or merge or reparent a page. Preserves history. Returns queued unless wait is set.", New: func() any { return new(EditInput) }},
		{Name: "forget", Description: "Remove an observation from current memory, preserving evidence. A timeline view is removed with its ancestors for rebuilding.", New: func() any { return new(ForgetInput) }},
		{Name: "focus", Description: "Hide or show a page and its descendants, or an observation, in one folder's project brief only.", New: func() any { return new(FocusInput) }},
		{Name: "ingest", New: func() any { return new(IngestInput) }}, {Name: "import", New: func() any { return new(importer.Options) }}, {Name: "worker", New: func() any { return new(EmptyInput) }}, {Name: "status", New: func() any { return new(EmptyInput) }}, {Name: "retry", New: func() any { return new(RetryInput) }}, {Name: "captured-session-entries", New: func() any { return new(CapturedInput) }}, {Name: "maintain", New: func() any { return new(EmptyInput) }}, {Name: "rebuild", New: func() any { return new(EmptyInput) }}, {Name: "reindex", New: func() any { return new(EmptyInput) }}, {Name: "doctor", New: func() any { return new(DoctorInput) }}, {Name: "stats", New: func() any { return new(EmptyInput) }},
	}
}
func inputSchema(input any) map[string]any {
	s := model.Schema(input)
	switch input.(type) {
	case *NoteInput:
		s["required"] = []any{"line", "pages", "actor"}
		delete(s["properties"].(map[string]any), "authority")
	case *EditInput:
		s["required"] = []any{"id", "reason", "actor"}
	}
	return s
}
func find(name string) (any, error) {
	for _, d := range definitions() {
		if d.Name == name {
			return d.New(), nil
		}
	}
	return nil, fmt.Errorf("unknown command %s", name)
}

type field struct {
	Name string
	Type reflect.Type
	Pos  bool
}

func fields(t reflect.Type) []field {
	out := []field{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous {
			out = append(out, fields(f.Type)...)
			continue
		}
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		out = append(out, field{name, f.Type, f.Tag.Get("cli") == "pos"})
	}
	return out
}
func Parse(input any, args []string) (map[string]json.RawMessage, error) {
	fs := fields(reflect.TypeOf(input).Elem())
	byFlag := map[string]field{}
	pos := []field{}
	for _, f := range fs {
		flag := "--" + strings.ReplaceAll(f.Name, "_", "-")
		if f.Type.Kind() == reflect.Slice {
			if f.Name == "queries" {
				flag = "--query"
			}
			if f.Name == "pages" {
				flag = "--page"
			}
		}
		byFlag[flag] = f
		if f.Pos {
			pos = append(pos, f)
		}
	}
	values := map[string]any{}
	pidx := 0
	if len(args) == 2 && args[0] == "--input-json" {
		if e := model.ValidateJSON([]byte(args[1]), inputSchema(input)); e != nil {
			return nil, e
		}
		if e := model.Decode([]byte(args[1]), input); e != nil {
			return nil, e
		}
		var raw map[string]json.RawMessage
		e := json.Unmarshal([]byte(args[1]), &raw)
		return raw, e
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		var f field
		var ok bool
		raw := ""
		if !strings.HasPrefix(arg, "--") {
			if pidx >= len(pos) {
				return nil, fmt.Errorf("unexpected argument %s", arg)
			}
			f = pos[pidx]
			pidx++
			raw = arg
		} else {
			if strings.HasPrefix(arg, "--clear-") {
				name := strings.ReplaceAll(strings.TrimPrefix(arg, "--clear-"), "-", "_")
				found := false
				for _, candidate := range fs {
					if candidate.Name == name && candidate.Type.Kind() == reflect.Pointer {
						found = true
					}
				}
				if !found {
					return nil, fmt.Errorf("unknown argument %s", arg)
				}
				values[name] = nil
				continue
			}
			f, ok = byFlag[arg]
			if !ok {
				return nil, fmt.Errorf("unknown argument %s", arg)
			}
			t := f.Type
			if t.Kind() == reflect.Pointer {
				t = t.Elem()
			}
			if t.Kind() == reflect.Bool {
				if i+1 < len(args) && (args[i+1] == "true" || args[i+1] == "false") {
					i++
					raw = args[i]
				} else {
					raw = "true"
				}
			} else {
				if i+1 == len(args) || strings.HasPrefix(args[i+1], "--") {
					return nil, fmt.Errorf("%s needs a value", arg)
				}
				i++
				raw = args[i]
			}
		}
		t := f.Type
		if t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		var v any
		var e error
		switch t.Kind() {
		case reflect.String:
			v = raw
		case reflect.Bool:
			v, e = strconv.ParseBool(raw)
		case reflect.Int, reflect.Int64:
			v, e = strconv.ParseInt(raw, 10, 64)
		case reflect.Float64:
			v, e = strconv.ParseFloat(raw, 64)
		case reflect.Slice:
			xs, _ := values[f.Name].([]string)
			values[f.Name] = append(xs, raw)
			continue
		default:
			return nil, fmt.Errorf("unsupported command field %s", f.Name)
		}
		if e != nil {
			return nil, e
		}
		if _, exists := values[f.Name]; exists {
			return nil, fmt.Errorf("repeated argument %s", f.Name)
		}
		values[f.Name] = v
	}
	b, e := json.Marshal(values)
	if e != nil {
		return nil, e
	}
	if e = model.ValidateJSON(b, inputSchema(input)); e != nil {
		return nil, e
	}
	if e = model.Decode(b, input); e != nil {
		return nil, e
	}
	var raw map[string]json.RawMessage
	e = json.Unmarshal(b, &raw)
	return raw, e
}
