package cli

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/markschroedr/pd-memory/internal/brief"
	"github.com/markschroedr/pd-memory/internal/fold"
	"github.com/markschroedr/pd-memory/internal/importer"
	inputlog "github.com/markschroedr/pd-memory/internal/log"
	"github.com/markschroedr/pd-memory/internal/memory"
	"github.com/markschroedr/pd-memory/internal/model"
	"github.com/markschroedr/pd-memory/internal/view"
)

// Agent commands stay separate from operators so hosts do not expose operator tools to a model.
type EngineCatalog struct {
	Version   string       `json:"version"`
	Commands  []Definition `json:"commands"`
	Operators []Definition `json:"operators"`
}
type MutationResult struct {
	Submitted inputlog.Submitted `json:"submitted"`
	Status    string             `json:"status"`
	Fold      *fold.Outcome      `json:"fold,omitempty"`
}
type IngestResult struct {
	Submitted []inputlog.Submitted `json:"submitted"`
	Status    string               `json:"status"`
	Fold      *fold.Outcome        `json:"fold,omitempty"`
}
type ImportResult struct {
	Import importer.Result `json:"import"`
	Fold   *fold.Outcome   `json:"fold,omitempty"`
}
type CapturedResult struct {
	Session  string   `json:"session"`
	EntryIDs []string `json:"entry_ids"`
}
type NavigationItem struct {
	ID       string  `json:"id"`
	Kind     string  `json:"kind" jsonschema:"enum=page,enum=observation,enum=timeline"`
	Line     *string `json:"line" jsonschema:"nullable"`
	Happened *string `json:"happened,omitempty" jsonschema:"nullable"`
	Parent   *string `json:"parent,omitempty" jsonschema:"nullable"`
	Category string  `json:"category,omitempty"`
}
type BrowseResult struct {
	Items    []NavigationItem `json:"items"`
	More     bool             `json:"more"`
	Page     *NavigationItem  `json:"page,omitempty"`
	Children []NavigationItem `json:"children,omitempty"`
}
type OpenSourceResult struct {
	ID           string          `json:"id"`
	Kind         string          `json:"kind" jsonschema:"enum=source"`
	EntryKind    string          `json:"entry_kind"`
	Actor        string          `json:"actor"`
	Created      string          `json:"created"`
	Source       map[string]any  `json:"source"`
	Digest       string          `json:"digest,omitempty"`
	Participants []string        `json:"participants,omitempty"`
	Chunks       []SourcePreview `json:"chunks"`
	Previous     string          `json:"previous,omitempty"`
	Next         string          `json:"next,omitempty"`
}
type SourcePreview struct {
	ID   string `json:"id"`
	Line string `json:"line"`
}
type OpenChunkResult struct {
	ID       string `json:"id"`
	Kind     string `json:"kind" jsonschema:"enum=chunk"`
	Context  string `json:"context"`
	Text     string `json:"text"`
	Source   string `json:"source"`
	Previous string `json:"previous,omitempty"`
	Next     string `json:"next,omitempty"`
}
type OpenPageResult struct {
	memory.Page
	ID           string               `json:"id"`
	Kind         string               `json:"kind" jsonschema:"enum=page"`
	Subpages     []PagePreview        `json:"subpages"`
	Observations []ObservationPreview `json:"observations"`
}
type PagePreview struct {
	Slug string  `json:"slug"`
	Line *string `json:"line" jsonschema:"nullable"`
}
type ObservationPreview struct {
	ID       string  `json:"id"`
	Line     string  `json:"line"`
	HasBody  bool    `json:"has_body"`
	Happened *string `json:"happened" jsonschema:"nullable"`
	Seq      int64   `json:"seq"`
}
type OpenObservationResult struct {
	memory.Observation
	ObservationKind  string              `json:"observation_kind"`
	ForgottenReason  string              `json:"forgotten_reason,omitempty"`
	History          []HistoryEntry      `json:"history,omitempty"`
	ReplacementChain []string            `json:"replacement_chain,omitempty"`
	Predecessors     []map[string]string `json:"predecessors,omitempty"`
}
type HistoryEntry struct {
	Seq     int64          `json:"seq"`
	Kind    string         `json:"kind"`
	Actor   string         `json:"actor"`
	Created string         `json:"created"`
	Payload map[string]any `json:"payload"`
}
type OpenTimelineResult struct {
	ID              string           `json:"id"`
	Kind            string           `json:"kind" jsonschema:"enum=timeline"`
	NodeKind        string           `json:"node_kind"`
	Text            string           `json:"text"`
	Headline        string           `json:"headline"`
	Seq             int64            `json:"seq"`
	Created         string           `json:"created"`
	Citations       []string         `json:"citations"`
	Parent          string           `json:"parent"`
	ParentAvailable bool             `json:"parent_available"`
	Children        []string         `json:"children"`
	Cited           []map[string]any `json:"cited,omitempty"`
}
type StatusEntry struct {
	Seq      int64   `json:"seq"`
	Kind     string  `json:"kind"`
	Status   string  `json:"status"`
	Attempts int     `json:"attempts"`
	Error    *string `json:"error" jsonschema:"nullable"`
}
type MaintenanceError struct {
	ID      string `json:"id"`
	Error   string `json:"error"`
	Created string `json:"created"`
}
type StatusResult struct {
	Entries           []StatusEntry      `json:"entries"`
	MaintenanceErrors []MaintenanceError `json:"maintenance_errors"`
	Cost              float64            `json:"cost_usd"`
}
type CostSummary struct {
	Phase string  `json:"phase"`
	Model string  `json:"model"`
	Tier  string  `json:"tier"`
	Calls int     `json:"calls"`
	Cost  float64 `json:"cost_usd"`
}
type StatsResult struct {
	Pages         int           `json:"pages"`
	Observations  int           `json:"observations"`
	Chunks        int           `json:"chunks"`
	Contributions int           `json:"contributions"`
	Evidence      int           `json:"evidence"`
	Views         int           `json:"views"`
	Calls         []CostSummary `json:"model_calls"`
	Cost          float64       `json:"cost_usd"`
}
type DoctorResult struct {
	Workspace              string         `json:"workspace"`
	Provider               string         `json:"provider"`
	BaseURL                string         `json:"base_url"`
	Model                  string         `json:"model"`
	ServiceTier            string         `json:"service_tier"`
	Store                  bool           `json:"store"`
	RetentionPolicy        string         `json:"retention_policy"`
	RetentionVerified      bool           `json:"retention_verified"`
	CredentialEnv          string         `json:"credential_env"`
	CredentialSet          bool           `json:"credential_set"`
	EmbeddingProvider      string         `json:"embedding_provider"`
	EmbeddingModel         string         `json:"embedding_model"`
	EmbeddingCredentialSet bool           `json:"embedding_credential_set"`
	Live                   map[string]any `json:"live,omitempty"`
	Cost                   float64        `json:"cost_usd,omitempty"`
}

func resultSchema(name string) map[string]any {
	var output any
	switch name {
	case "recall", "brief":
		output = new(brief.Result)
	case "browse":
		output = new(BrowseResult)
	case "note", "edit", "forget", "focus":
		output = new(MutationResult)
	case "ingest":
		output = new(IngestResult)
	case "import":
		output = new(ImportResult)
	case "captured-session-entries":
		output = new(CapturedResult)
	case "worker", "retry", "rebuild", "reindex":
		output = new(fold.Outcome)
	case "maintain":
		output = new(view.Result)
	case "status":
		output = new(StatusResult)
	case "stats":
		output = new(StatsResult)
	case "doctor":
		output = new(DoctorResult)
	case "catalog":
		output = new(EngineCatalog)
	case "open":
		observation := model.Schema(new(OpenObservationResult))
		observation["properties"].(map[string]any)["kind"] = map[string]any{"type": "string", "enum": []any{"observation"}}
		return map[string]any{"anyOf": []any{observation, model.Schema(new(OpenPageResult)), model.Schema(new(OpenSourceResult)), model.Schema(new(OpenChunkResult)), model.Schema(new(OpenTimelineResult))}}
	default:
		panic("missing result contract: " + name)
	}
	return model.Schema(output)
}
func FullCatalog() EngineCatalog {
	catalog := EngineCatalog{Commands: []Definition{}, Operators: []Definition{}}
	for _, d := range definitions() {
		d.InputSchema = inputSchema(d.New())
		d.ResultSchema = resultSchema(d.Name)
		if d.Description != "" {
			catalog.Commands = append(catalog.Commands, d)
		} else {
			if d.Name == "import" {
				d.Name = "import sessions"
			}
			catalog.Operators = append(catalog.Operators, d)
		}
	}
	data, _ := json.Marshal(catalog)
	fingerprint := sha256.Sum256(data)
	catalog.Version = fmt.Sprintf("0.1.0+%x", fingerprint[:8])
	return catalog
}

// This renders only the JSON Schema vocabulary emitted by our command structs. It is not a schema runtime.
func typeScriptType(s map[string]any) string {
	if variants, ok := s["anyOf"].([]any); ok {
		types := []string{}
		for _, v := range variants {
			types = append(types, typeScriptType(v.(map[string]any)))
		}
		return "(" + strings.Join(types, " | ") + ")"
	}
	if values, ok := s["enum"].([]any); ok {
		literals := []string{}
		for _, v := range values {
			b, _ := json.Marshal(v)
			literals = append(literals, string(b))
		}
		return strings.Join(literals, " | ")
	}
	switch s["type"] {
	case "null":
		return "null"
	case "string":
		return "string"
	case "boolean":
		return "boolean"
	case "number", "integer":
		return "number"
	case "array":
		if item, ok := s["items"].(map[string]any); ok {
			return "Array<" + typeScriptType(item) + ">"
		}
		return "Array<unknown>"
	case "object":
		required := map[string]bool{}
		requiredKeys, _ := s["required"].([]any)
		for _, key := range requiredKeys {
			required[key.(string)] = true
		}
		keys := []string{}
		properties, _ := s["properties"].(map[string]any)
		for key := range properties {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		lines := []string{}
		for _, key := range keys {
			optional := "?"
			if required[key] {
				optional = ""
			}
			encoded, _ := json.Marshal(key)
			lines = append(lines, string(encoded)+optional+": "+typeScriptType(properties[key].(map[string]any))+";")
		}
		if additional, ok := s["additionalProperties"].(map[string]any); ok {
			lines = append(lines, "[key: string]: "+typeScriptType(additional)+";")
		} else if s["additionalProperties"] != false {
			lines = append(lines, "[key: string]: unknown;")
		}
		return "{\n" + strings.Join(lines, "\n") + "\n}"
	}
	return "unknown"
}
func TypeScript(c EngineCatalog) string {
	var out strings.Builder
	fmt.Fprintln(&out, "// Generated by pd-memory catalog --typescript. Do not edit.")
	fmt.Fprintf(&out, "export const ENGINE_VERSION = %q as const;\n", c.Version)
	all := append(append([]Definition{}, c.Commands...), c.Operators...)
	names := map[string]string{}
	for _, d := range all {
		parts := strings.FieldsFunc(d.Name, func(r rune) bool { return r == '-' || r == ' ' })
		for i, p := range parts {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
		name := strings.Join(parts, "")
		names[d.Name] = name
		fmt.Fprintf(&out, "\nexport type %sInput = %s;\nexport type %sResult = %s;\n", name, typeScriptType(d.InputSchema), name, typeScriptType(d.ResultSchema))
	}
	fmt.Fprintf(&out, "\nexport type CatalogResult = %s;\n", typeScriptType(model.Schema(new(EngineCatalog))))
	fmt.Fprintln(&out, "\nexport interface Commands {")
	for _, d := range all {
		fmt.Fprintf(&out, "%q: { input: %sInput; result: %sResult };\n", d.Name, names[d.Name], names[d.Name])
	}
	fmt.Fprintln(&out, "\"catalog\": { input: Record<string, never>; result: CatalogResult };\n}\nexport type Command = keyof Commands;")
	return out.String()
}
