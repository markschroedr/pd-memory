package config

import (
	"bytes"
	_ "embed"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/expr-lang/expr"
	"github.com/pelletier/go-toml/v2"
)

//go:embed defaults.toml
var Defaults string

type Route struct {
	Root string `toml:"root"`
	Home string `toml:"home"`
}
type Source struct {
	Name        string   `toml:"name"`
	Adapter     string   `toml:"adapter"`
	Path        string   `toml:"path"`
	Kind        string   `toml:"kind"`
	SettleAfter string   `toml:"settle_after"`
	After       string   `toml:"after"`
	Exclude     []string `toml:"exclude"`
	Query       string   `toml:"query"`
}
type Config struct {
	CredentialsFile string   `toml:"credentials_file"`
	SettleAfter     string   `toml:"settle_after"`
	AutoSync        bool     `toml:"auto_sync"`
	Routes          []Route  `toml:"routes"`
	Sources         []Source `toml:"sources"`
	Workspace       struct {
		Dir string `toml:"dir"`
	} `toml:"workspace"`
	OpenAI struct {
		Provider          string   `toml:"provider"`
		BaseURL           string   `toml:"base_url"`
		APIKeyEnv         string   `toml:"api_key_env"`
		Model             string   `toml:"model"`
		ServiceTier       string   `toml:"service_tier"`
		Store             bool     `toml:"store"`
		RetentionPolicy   string   `toml:"retention_policy"`
		RetentionVerified bool     `toml:"retention_verified"`
		TimeoutMS         int      `toml:"timeout_ms"`
		ZDR               bool     `toml:"zdr"`
		AllowFallbacks    bool     `toml:"allow_fallbacks"`
		Only              []string `toml:"only"`
	} `toml:"openai"`
	Embeddings struct {
		Provider       string   `toml:"provider"`
		BaseURL        string   `toml:"base_url"`
		APIKeyEnv      string   `toml:"api_key_env"`
		Model          string   `toml:"model"`
		Dimension      int      `toml:"dimension"`
		TimeoutMS      int      `toml:"timeout_ms"`
		ZDR            bool     `toml:"zdr"`
		AllowFallbacks bool     `toml:"allow_fallbacks"`
		Only           []string `toml:"only"`
	} `toml:"embeddings"`
	Identity struct {
		UserNames       []string `toml:"user_names"`
		ThirdPartyNames []string `toml:"third_party_names"`
	} `toml:"identity"`
	Ingest struct {
		MatchesPerCandidate int `toml:"matches_per_candidate"`
		ExtractConcurrency  int `toml:"extract_concurrency"`
	} `toml:"ingest"`
	Chunk struct {
		TargetTokens        int `toml:"target_tokens"`
		SingleChunkTokens   int `toml:"single_chunk_tokens"`
		ExtractWindowTokens int `toml:"extract_window_tokens"`
	} `toml:"chunk"`
	Search struct {
		MaxLimit int `toml:"max_limit"`
		RRFK     int `toml:"rrf_k"`
	} `toml:"search"`
	Brief struct {
		GlobalBudget             int     `toml:"global_budget"`
		ProjectBudget            int     `toml:"project_budget"`
		RecallBudget             int     `toml:"recall_budget"`
		SpineShare               float64 `toml:"spine_share"`
		PageDecay                float64 `toml:"page_decay"`
		NextCap                  int     `toml:"next_cap"`
		ProjectAffinityExponent  int     `toml:"project_affinity_exponent"`
		DirectoryShare           float64 `toml:"directory_share"`
		DirectoryMinObservations int     `toml:"directory_min_observations"`
		ComposeInputFactor       int     `toml:"compose_input_factor"`
	} `toml:"brief"`
	Timeline struct {
		Timezone      string `toml:"timezone"`
		HistoryBudget int    `toml:"history_budget"`
		RecentBudget  int    `toml:"recent_budget"`
		OpenBudget    int    `toml:"open_budget"`
	} `toml:"timeline"`
	Compose struct {
		Mode       string   `toml:"mode"`
		MinChanges int      `toml:"min_changes"`
		Folders    []string `toml:"folders"`
	} `toml:"compose"`
	Buckets struct {
		Three float64 `toml:"bucket_3_min"`
		Two   float64 `toml:"bucket_2_min"`
	} `toml:"buckets"`
	Formulas  map[string]string `toml:"formulas"`
	CallSites struct {
		Search           string `toml:"search"`
		SearchChunk      string `toml:"search_chunk"`
		BriefStanding    string `toml:"brief_standing"`
		BriefSituational string `toml:"brief_situational"`
		PageRank         string `toml:"page_rank"`
	} `toml:"call_sites"`
	SourcePriors map[string]float64 `toml:"source_priors"`
	Pricing      struct {
		Input     float64 `toml:"input_per_million"`
		Cached    float64 `toml:"cached_input_per_million"`
		Output    float64 `toml:"output_per_million"`
		Embedding float64 `toml:"embedding_per_million"`
	} `toml:"pricing"`
	Path string `toml:"-"`
}

var SourceKinds = []string{"note_user", "note_agent", "conversation", "meeting", "chat_log", "document", "coding_session"}

func Contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
func Load(path string) (*Config, error) {
	data, e := os.ReadFile(path)
	if e != nil {
		return nil, e
	}
	c := new(Config)
	c.Path, e = filepath.Abs(path)
	if e != nil {
		return nil, e
	}
	if e = toml.Unmarshal([]byte(Defaults), c); e != nil {
		return nil, e
	}
	if e = toml.NewDecoder(bytes.NewReader(data)).DisallowUnknownFields().Decode(c); e != nil {
		return nil, e
	}
	if c.OpenAI.Store || c.OpenAI.AllowFallbacks || c.Embeddings.AllowFallbacks {
		return nil, fmt.Errorf("store and allow_fallbacks must be false")
	}
	if e = c.sources(); e != nil {
		return nil, e
	}
	if c.CredentialsFile != "" {
		c.CredentialsFile = resolvePath(c.Path, c.CredentialsFile)
	}
	for _, names := range [][]string{c.Identity.UserNames, c.Identity.ThirdPartyNames, c.OpenAI.Only, c.Embeddings.Only} {
		for _, name := range names {
			if strings.TrimSpace(name) == "" {
				return nil, fmt.Errorf("empty configured identity or provider")
			}
		}
	}
	for _, endpoint := range []string{c.OpenAI.BaseURL, c.Embeddings.BaseURL} {
		u, err := url.Parse(endpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" {
			return nil, fmt.Errorf("invalid provider base_url")
		}
	}
	for name := range c.Formulas {
		if _, e = c.Evaluate(name, Variables()); e != nil {
			return nil, e
		}
	}
	if c.Workspace.Dir == "" {
		return nil, fmt.Errorf("workspace.dir is required")
	}
	c.Workspace.Dir = resolvePath(c.Path, c.Workspace.Dir)
	c.OpenAI.BaseURL = strings.TrimRight(c.OpenAI.BaseURL, "/")
	c.Embeddings.BaseURL = strings.TrimRight(c.Embeddings.BaseURL, "/")
	if !Contains([]string{"openai", "openrouter"}, c.OpenAI.Provider) || !Contains([]string{"local_pplx", "openrouter"}, c.Embeddings.Provider) {
		return nil, fmt.Errorf("invalid provider")
	}
	if !Contains([]string{"flex", "default"}, c.OpenAI.ServiceTier) || c.OpenAI.Store || !Contains([]string{"zero_data_retention", "modified_abuse_monitoring", "standard_store_false"}, c.OpenAI.RetentionPolicy) {
		return nil, fmt.Errorf("invalid generation tier or retention configuration")
	}
	if c.OpenAI.Provider == "openrouter" && (c.OpenAI.ServiceTier != "default" || !c.OpenAI.ZDR || c.OpenAI.AllowFallbacks || len(c.OpenAI.Only) == 0) {
		return nil, fmt.Errorf("OpenRouter generation requires default tier, zdr=true, allow_fallbacks=false, only providers")
	}
	if c.Embeddings.Provider == "openrouter" && (!c.Embeddings.ZDR || c.Embeddings.AllowFallbacks || len(c.Embeddings.Only) == 0 || c.Embeddings.APIKeyEnv == "") {
		return nil, fmt.Errorf("OpenRouter embeddings require zdr=true, allow_fallbacks=false, only providers and api_key_env")
	}
	for name, value := range map[string]string{"openai.base_url": c.OpenAI.BaseURL, "openai.api_key_env": c.OpenAI.APIKeyEnv, "openai.model": c.OpenAI.Model, "embeddings.base_url": c.Embeddings.BaseURL, "embeddings.model": c.Embeddings.Model} {
		if value == "" {
			return nil, fmt.Errorf("missing %s", name)
		}
	}
	for name, value := range map[string]int{"openai.timeout_ms": c.OpenAI.TimeoutMS, "embeddings.timeout_ms": c.Embeddings.TimeoutMS, "embeddings.dimension": c.Embeddings.Dimension, "ingest.matches_per_candidate": c.Ingest.MatchesPerCandidate, "ingest.extract_concurrency": c.Ingest.ExtractConcurrency, "chunk.target_tokens": c.Chunk.TargetTokens, "chunk.single_chunk_tokens": c.Chunk.SingleChunkTokens, "chunk.extract_window_tokens": c.Chunk.ExtractWindowTokens, "search.max_limit": c.Search.MaxLimit, "search.rrf_k": c.Search.RRFK, "brief.next_cap": c.Brief.NextCap, "brief.project_affinity_exponent": c.Brief.ProjectAffinityExponent, "brief.directory_min_observations": c.Brief.DirectoryMinObservations, "brief.compose_input_factor": c.Brief.ComposeInputFactor, "timeline.history_budget": c.Timeline.HistoryBudget, "timeline.recent_budget": c.Timeline.RecentBudget, "timeline.open_budget": c.Timeline.OpenBudget, "compose.min_changes": c.Compose.MinChanges} {
		if value < 1 {
			return nil, fmt.Errorf("invalid %s", name)
		}
	}
	for name, value := range map[string]int{"global_budget": c.Brief.GlobalBudget, "project_budget": c.Brief.ProjectBudget, "recall_budget": c.Brief.RecallBudget} {
		if value < 200 {
			return nil, fmt.Errorf("brief %s must be at least 200", name)
		}
	}
	for _, v := range []float64{c.Brief.SpineShare, c.Brief.DirectoryShare, c.Buckets.Three, c.Buckets.Two} {
		if v < 0 || v > 1 || math.IsNaN(v) {
			return nil, fmt.Errorf("invalid share or bucket")
		}
	}
	if !(c.Brief.PageDecay > 0 && c.Brief.PageDecay <= 1) {
		return nil, fmt.Errorf("brief page_decay must be in (0, 1]")
	}
	if c.Buckets.Two > c.Buckets.Three {
		return nil, fmt.Errorf("rank buckets out of order")
	}
	for _, v := range []float64{c.Pricing.Input, c.Pricing.Cached, c.Pricing.Output, c.Pricing.Embedding} {
		if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, fmt.Errorf("invalid pricing")
		}
	}
	for _, k := range SourceKinds {
		if v, ok := c.SourcePriors[k]; !ok || v < 0 || v > 1 {
			return nil, fmt.Errorf("invalid source prior %s", k)
		}
	}
	for k := range c.SourcePriors {
		if !Contains(SourceKinds, k) {
			return nil, fmt.Errorf("unknown source prior %s", k)
		}
	}
	if _, e = time.LoadLocation(c.Timeline.Timezone); e != nil {
		return nil, e
	}
	if !Contains([]string{"off", "global", "projects"}, c.Compose.Mode) {
		return nil, fmt.Errorf("invalid compose mode")
	}
	for i, f := range c.Compose.Folders {
		c.Compose.Folders[i], e = filepath.Abs(f)
		if e != nil {
			return nil, e
		}
	}
	for _, name := range []string{c.CallSites.Search, c.CallSites.SearchChunk, c.CallSites.BriefStanding, c.CallSites.BriefSituational, c.CallSites.PageRank} {
		if _, e = c.Evaluate(name, Variables()); e != nil {
			return nil, e
		}
	}
	return c, nil
}
func Variables() map[string]float64 {
	return map[string]float64{"reach": .5, "surprise": .5, "directive": 0, "sensitivity": 0, "best": .5, "confidence": .9, "freshness": 1, "source_prior": 1, "sources": 2, "relevance": .8, "observations": 5, "subpages": 0, "inbound": 2}
}
func (c *Config) Evaluate(name string, values map[string]float64) (float64, error) {
	formula, ok := c.Formulas[name]
	if !ok {
		return 0, fmt.Errorf("unknown formula %s", name)
	}
	env := map[string]any{"log": func(x float64) float64 { return math.Log(x) }}
	for k, v := range values {
		env[k] = v
	}
	p, e := expr.Compile(formula, expr.Env(env), expr.AsFloat64())
	if e != nil {
		return 0, e
	}
	v, e := expr.Run(p, env)
	if e != nil {
		return 0, e
	}
	n := v.(float64)
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, fmt.Errorf("formula %s not finite", name)
	}
	return math.Max(0, n), nil
}
func (c *Config) Bucket(n float64) int {
	if n >= c.Buckets.Three {
		return 3
	}
	if n >= c.Buckets.Two {
		return 2
	}
	return 1
}
