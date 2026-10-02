package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/invopop/jsonschema"
	"github.com/markschroedr/pd-memory/internal/config"
	validation "github.com/santhosh-tekuri/jsonschema/v6"
)

type Usage struct {
	Input   int64 `json:"input_tokens"`
	Output  int64 `json:"output_tokens"`
	Prompt  int64 `json:"prompt_tokens,omitempty"`
	Details struct {
		Cached int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}
type Call struct {
	Phase     string
	RequestID string
	Model     string
	Tier      string
	Usage     Usage
	Cost      float64
	Repaired  bool
	Error     string
}
type ProbeOutput struct {
	OK bool `json:"ok"`
}
type Client struct {
	Config  *config.Config
	Record  func(Call) error
	Invalid func(error) error
	Cost    float64
	Last    Call
}

func (c *Client) record(call Call) error {
	p := c.Config.Pricing
	factor := 1.
	if c.Config.OpenAI.ServiceTier == "flex" && call.Tier == "default" {
		factor = 2
	}
	if c.Config.OpenAI.ServiceTier == "default" && call.Tier == "flex" {
		factor = .5
	}
	if strings.HasPrefix(call.Phase, "embed") {
		if c.Config.Embeddings.Provider != "local_pplx" {
			call.Cost = float64(call.Usage.Input) * p.Embedding / 1e6
		}
	} else {
		call.Cost = factor * (float64(call.Usage.Input-call.Usage.Details.Cached)*p.Input + float64(call.Usage.Details.Cached)*p.Cached + float64(call.Usage.Output)*p.Output) / 1e6
	}
	c.Cost += call.Cost
	c.Last = call
	if c.Record != nil {
		return c.Record(call)
	}
	return nil
}
func Schema(v any) map[string]any {
	r := jsonschema.Reflector{DoNotReference: true, ExpandedStruct: true}
	b, _ := json.Marshal(r.Reflect(v))
	var s map[string]any
	json.Unmarshal(b, &s)
	var normalize func(any)
	normalize = func(value any) {
		switch node := value.(type) {
		case map[string]any:
			delete(node, "$schema")
			delete(node, "$id")
			if variants, ok := node["oneOf"]; ok {
				node["anyOf"] = variants
				delete(node, "oneOf")
			}
			for _, child := range node {
				normalize(child)
			}
		case []any:
			for _, child := range node {
				normalize(child)
			}
		}
	}
	normalize(s)
	return s
}

func ValidateJSON(data []byte, schema map[string]any) error {
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	compiler := validation.NewCompiler()
	const location = "https://pd-memory.invalid/input"
	if err := compiler.AddResource(location, schema); err != nil {
		return err
	}
	compiled, err := compiler.Compile(location)
	if err != nil {
		return err
	}
	return compiled.Validate(value)
}
func Decode(data []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		return e
	}
	var extra any
	if e := d.Decode(&extra); e != io.EOF {
		return fmt.Errorf("unexpected trailing JSON")
	}
	return nil
}
func (c *Client) Generate(phase, system, input string, schema map[string]any, repaired bool) (string, error) {
	cfg := c.Config.OpenAI
	if !cfg.RetentionVerified {
		return "", fmt.Errorf("generation retention is not operator-verified")
	}
	key := os.Getenv(cfg.APIKeyEnv)
	if key == "" {
		return "", fmt.Errorf("credential environment variable %s is not set", cfg.APIKeyEnv)
	}
	body := map[string]any{"model": cfg.Model, "store": false, "reasoning": map[string]any{"effort": "medium"}, "input": []any{map[string]any{"role": "system", "content": []any{map[string]any{"type": "input_text", "text": system}}}, map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": input}}}}}
	if cfg.Provider == "openai" {
		body["service_tier"] = cfg.ServiceTier
	} else {
		body["provider"] = map[string]any{"zdr": true, "allow_fallbacks": false, "require_parameters": true, "only": cfg.Only}
	}
	if schema != nil {
		body["text"] = map[string]any{"format": map[string]any{"type": "json_schema", "name": "pd_memory_" + phase, "strict": true, "schema": schema}}
	}
	call := Call{Phase: phase, Model: cfg.Model, Repaired: repaired}
	var raw string
	var err error
	client := &http.Client{Timeout: time.Duration(cfg.TimeoutMS) * time.Millisecond}
	for attempt := 0; attempt < 2; attempt++ {
		if attempt == 1 {
			body["service_tier"] = "auto"
		}
		response, status, requestID, e := request(client, cfg.BaseURL+"/responses", key, body)
		call.RequestID = requestID
		if e != nil {
			err = e
			break
		}
		var res struct {
			ID         string `json:"id"`
			Model      string `json:"model"`
			Tier       string `json:"service_tier"`
			Status     string `json:"status"`
			Incomplete any    `json:"incomplete_details"`
			Usage      Usage  `json:"usage"`
			Error      struct {
				Message string `json:"message"`
				Code    any    `json:"code"`
			} `json:"error"`
			Output []struct {
				Type    string `json:"type"`
				Content []struct {
					Type    string `json:"type"`
					Text    string `json:"text"`
					Refusal string `json:"refusal"`
				} `json:"content"`
			} `json:"output"`
		}
		if e = json.Unmarshal(response, &res); e != nil {
			err = e
			break
		}
		if status < 200 || status >= 300 {
			if attempt == 0 && status == 429 && cfg.Provider == "openai" && cfg.ServiceTier == "flex" && !strings.Contains(strings.ToLower(fmt.Sprint(res.Error.Code)+" "+res.Error.Message), "quota") {
				continue
			}
			err = fmt.Errorf("%s responses failed (%d): %s", cfg.Provider, status, res.Error.Message)
			break
		}
		if res.ID != "" {
			call.RequestID = res.ID
		}
		if res.Model != "" {
			call.Model = res.Model
		}
		call.Tier = res.Tier
		if call.Tier == "" {
			if attempt == 1 {
				call.Tier = "default"
			} else if cfg.Provider == "openai" {
				call.Tier = cfg.ServiceTier
			}
		}
		call.Usage = res.Usage
		if res.Status == "incomplete" {
			err = fmt.Errorf("incomplete response: %v", res.Incomplete)
			break
		}
		for _, item := range res.Output {
			if item.Type != "message" {
				continue
			}
			for _, content := range item.Content {
				if content.Type == "refusal" {
					err = fmt.Errorf("model refused: %s", content.Refusal)
				}
				if content.Type == "output_text" {
					raw += content.Text
				}
			}
		}
		if raw == "" && err == nil {
			err = fmt.Errorf("response has no output_text")
		}
		break
	}
	if err != nil {
		call.Error = err.Error()
	}
	if e := c.record(call); e != nil {
		return "", e
	}
	return raw, err
}
func Structured[T any](c *Client, phase, system, input string, validate func(T) error) (T, error) {
	return StructuredSchema[T](c, phase, system, input, Schema(new(T)), validate)
}
func StructuredSchema[T any](c *Client, phase, system, input string, schema map[string]any, validate func(T) error) (T, error) {
	var zero T
	var last error
	var prior string
	for i := 0; i < 2; i++ {
		in := input
		if i > 0 {
			in += "\n\nThe prior response failed local validation. Return a corrected complete object.\nValidation error: " + last.Error() + "\nPrior response: " + prior
		}
		raw, e := c.Generate(phase, system, in, schema, i == 1)
		if e != nil {
			return zero, e
		}
		prior = raw
		var out T
		e = ValidateJSON([]byte(raw), schema)
		if e == nil {
			e = Decode([]byte(raw), &out)
		}
		if e == nil {
			e = validate(out)
		}
		if e == nil {
			return out, nil
		}
		if c.Invalid != nil {
			if err := c.Invalid(e); err != nil {
				return zero, err
			}
		}
		last = e
	}
	return zero, fmt.Errorf("structured output failed after one repair: %w", last)
}
func (c *Client) Embed(inputs []string) ([][]float64, error) {
	if len(inputs) == 0 {
		return [][]float64{}, nil
	}
	cfg := c.Config.Embeddings
	local := cfg.Provider == "local_pplx"
	endpoint := cfg.BaseURL
	key := ""
	body := map[string]any{}
	if local {
		body = map[string]any{"texts": inputs, "max_length": 1024, "batch_size": 8, "quantization": "none", "normalize_embeddings": false}
	} else {
		key = os.Getenv(cfg.APIKeyEnv)
		if key == "" {
			return nil, fmt.Errorf("credential environment variable %s is not set", cfg.APIKeyEnv)
		}
		endpoint += "/embeddings"
		body = map[string]any{"model": cfg.Model, "input": inputs, "encoding_format": "float", "provider": map[string]any{"zdr": true, "allow_fallbacks": false, "only": cfg.Only}}
	}
	call := Call{Phase: "embed", Model: cfg.Model}
	data, status, id, e := request(&http.Client{Timeout: time.Duration(cfg.TimeoutMS) * time.Millisecond}, endpoint, key, body)
	call.RequestID = id
	var vectors [][]float64
	if e == nil {
		var r struct {
			ID          string      `json:"id"`
			Model       string      `json:"model"`
			Embeddings  [][]float64 `json:"embeddings"`
			TokenCounts []int64     `json:"token_counts"`
			Usage       Usage       `json:"usage"`
			Data        []struct {
				Index     int       `json:"index"`
				Embedding []float64 `json:"embedding"`
			} `json:"data"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		e = json.Unmarshal(data, &r)
		if e == nil && (status < 200 || status >= 300) {
			e = fmt.Errorf("embedding request failed (%d): %s", status, r.Error.Message)
		}
		if e == nil {
			if r.Model != "" {
				call.Model = r.Model
			}
			if r.ID != "" {
				call.RequestID = r.ID
			}
			if local {
				vectors = r.Embeddings
				for _, n := range r.TokenCounts {
					call.Usage.Input += n
				}
			} else {
				call.Usage = r.Usage
				if call.Usage.Prompt != 0 {
					call.Usage.Input = call.Usage.Prompt
				}
				vectors = make([][]float64, len(inputs))
				for _, v := range r.Data {
					if v.Index < 0 || v.Index >= len(inputs) || vectors[v.Index] != nil {
						e = fmt.Errorf("missing or duplicate embedding indexes")
						break
					}
					vectors[v.Index] = v.Embedding
				}
			}
			if len(vectors) != len(inputs) {
				e = fmt.Errorf("embedding count mismatch")
			}
			for _, v := range vectors {
				norm := 0.
				for _, x := range v {
					norm += x * x
				}
				if len(v) != cfg.Dimension || norm == 0 || math.IsNaN(norm) || math.IsInf(norm, 0) {
					e = fmt.Errorf("invalid embedding dimension or norm")
					break
				}
			}
		}
	}
	if e != nil {
		call.Error = e.Error()
	}
	if err := c.record(call); err != nil {
		return nil, err
	}
	return vectors, e
}
func request(client *http.Client, endpoint, key string, body any) ([]byte, int, string, error) {
	b, e := json.Marshal(body)
	if e != nil {
		return nil, 0, "", e
	}
	req, e := http.NewRequest("POST", endpoint, bytes.NewReader(b))
	if e != nil {
		return nil, 0, "", e
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	r, e := client.Do(req)
	if e != nil {
		return nil, 0, "", e
	}
	defer r.Body.Close()
	b, e = io.ReadAll(r.Body)
	return b, r.StatusCode, r.Header.Get("x-request-id"), e
}
