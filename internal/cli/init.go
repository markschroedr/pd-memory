package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/markschroedr/pd-memory/internal/config"
	inputlog "github.com/markschroedr/pd-memory/internal/log"
	"github.com/markschroedr/pd-memory/internal/memory"
	"github.com/pelletier/go-toml/v2"
	"golang.org/x/term"
)

// Set by release builds. Catalog.Version independently fingerprints the process contract.
var Version = "dev"

func binaryVersion() string {
	if Version != "dev" {
		return Version
	}
	if info, ok := debug.ReadBuildInfo(); ok && strings.HasPrefix(info.Main.Version, "v") {
		return info.Main.Version
	}
	return "dev"
}

func initialize(path string) (int, error) {
	abs, e := filepath.Abs(path)
	if e != nil {
		return 0, e
	}
	path = abs
	if _, e = os.Stat(path); e == nil {
		return 0, fmt.Errorf("configuration already exists: %s", path)
	} else if !os.IsNotExist(e) {
		return 0, e
	}
	reader := bufio.NewReader(os.Stdin)
	ask := func(prompt string) (string, error) {
		fmt.Fprint(os.Stderr, prompt)
		s, e := reader.ReadString('\n')
		if e == io.EOF && s != "" {
			e = nil
		}
		return strings.TrimSpace(s), e
	}
	name, e := ask("Your name: ")
	if e != nil {
		return 0, e
	}
	if name == "" {
		return 0, fmt.Errorf("name required")
	}
	askKey := func(prompt string) (string, error) {
		var key string
		if term.IsTerminal(int(os.Stdin.Fd())) {
			fmt.Fprint(os.Stderr, prompt)
			b, err := term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Fprintln(os.Stderr)
			if err != nil {
				return "", err
			}
			key = string(b)
		} else {
			var err error
			key, err = ask(prompt)
			if err != nil {
				return "", err
			}
		}
		if key == "" || strings.ContainsAny(key, "\r\n\t\"' ") {
			return "", fmt.Errorf("key required, without whitespace or quotes")
		}
		return key, nil
	}
	provider, e := ask("Provider: 1) OpenAI (recommended) 2) OpenRouter [1]: ")
	if e != nil {
		return 0, e
	}
	switch strings.ToLower(provider) {
	case "", "1", "openai":
		provider = "openai"
	case "2", "openrouter":
		provider = "openrouter"
	default:
		return 0, fmt.Errorf("choose OpenAI or OpenRouter")
	}
	keyEnv, providerName := "OPENAI_API_KEY", "OpenAI"
	if provider == "openrouter" {
		keyEnv, providerName = "OPENROUTER_API_KEY", "OpenRouter"
		fmt.Fprintln(os.Stderr, "OpenRouter generation uses only the Azure ZDR route. Perplexity embeddings also require ZDR. Provider fallback is disabled.")
	}
	key, e := askKey(providerName + " API key (" + keyEnv + "): ")
	if e != nil {
		return 0, e
	}
	yes := func(s string) bool { return strings.EqualFold(s, "y") || strings.EqualFold(s, "yes") }
	retention := "zero_data_retention"
	if provider == "openai" {
		zdr, e := ask("Does this OpenAI account have zero data retention? [y/N]: ")
		if e != nil {
			return 0, e
		}
		if !yes(zdr) {
			fmt.Fprintln(os.Stderr, "OpenAI may retain API data for abuse monitoring. store=false still applies.")
			approval, e := ask("Continue with standard retention? [y/N]: ")
			if e != nil {
				return 0, e
			}
			if !yes(approval) {
				fmt.Fprintln(os.Stderr, "Setup canceled; no files written.")
				return 1, nil
			}
			retention = "standard_store_false"
		}
	}
	piSetup, e := ask("Add Pi session import? [y/N]: ")
	if e != nil {
		return 0, e
	}
	raw := map[string]any{"settle_after": "3h", "auto_sync": true,
		"openai":     map[string]any{"provider": provider, "retention_policy": retention, "retention_verified": true},
		"embeddings": map[string]any{"provider": provider}}
	if provider == "openrouter" {
		raw["openai"] = map[string]any{"provider": provider, "base_url": "https://openrouter.ai/api/v1", "api_key_env": keyEnv, "model": "openai/gpt-6-luna", "service_tier": "default", "zdr": true, "allow_fallbacks": false, "only": []string{"Azure"}, "retention_policy": retention, "retention_verified": true}
		raw["embeddings"] = map[string]any{"provider": provider, "base_url": "https://openrouter.ai/api/v1", "api_key_env": keyEnv, "model": "perplexity/pplx-embed-v1-0.6b", "dimension": 1024, "zdr": true, "allow_fallbacks": false, "only": []string{"Perplexity"}}
		raw["pricing"] = map[string]any{"input_per_million": 0.1, "cached_input_per_million": 0.01, "output_per_million": 0.5, "embedding_per_million": 0.004}
	}
	home, e := os.UserHomeDir()
	if e != nil {
		return 0, e
	}
	raw["workspace"] = map[string]any{"dir": filepath.Join(home, ".local", "share", "pd-memory")}
	credentials := filepath.Join(filepath.Dir(path), "credentials.env")
	raw["credentials_file"] = credentials
	raw["identity"] = map[string]any{"user_names": []string{name}, "third_party_names": []string{}}
	if yes(piSetup) {
		sessions := filepath.Join(home, ".pi", "agent", "sessions")
		if root := os.Getenv("PI_CODING_AGENT_DIR"); root != "" {
			sessions = filepath.Join(root, "sessions")
		}
		raw["sources"] = []map[string]any{{"name": "pi", "adapter": "pi", "path": sessions, "kind": "coding_session", "after": time.Now().UTC().Format(time.RFC3339Nano)}}
		if e = os.MkdirAll(sessions, 0700); e != nil {
			return 0, e
		}
	}
	b, e := toml.Marshal(raw)
	if e != nil {
		return 0, e
	}
	if e = os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return 0, e
	}
	writeNew := func(path string, b []byte) error {
		f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return e
		}
		_, e = f.Write(b)
		ce := f.Close()
		if e != nil {
			return e
		}
		return ce
	}
	if e = writeNew(credentials, []byte(keyEnv+"="+key+"\n")); e != nil {
		return 0, e
	}
	if e = writeNew(path, b); e != nil {
		return 0, e
	}
	c, e := config.Load(path)
	if e != nil {
		return 0, e
	}
	l, e := inputlog.Open(c.Workspace.Dir, true)
	if e != nil {
		return 0, e
	}
	l.Close()
	m, e := memory.Open(c.Workspace.Dir, true)
	if e != nil {
		return 0, e
	}
	m.Close()
	fmt.Fprintln(os.Stderr, "Config:", path, "\nWorkspace:", c.Workspace.Dir, "\nChecking generation and embeddings...")
	code, e := run([]string{"doctor", "--live", "--config", path})
	if e != nil || code != 0 {
		return code, e
	}
	if raw["sources"] != nil {
		version := binaryVersion()
		if version == "dev" {
			fmt.Fprintln(os.Stderr, "Development binary: install the Pi extension from your local checkout. Release binaries install a matching version.")
		} else {
			install, e := ask("Install the matching Pi memory extension? [y/N]: ")
			if e != nil {
				return 0, e
			}
			if yes(install) {
				cmd := exec.Command("pi", "install", "git:github.com/markschroedr/pd-memory@"+version)
				cmd.Stdin = os.Stdin
				cmd.Stdout = os.Stdout
				cmd.Stderr = os.Stderr
				if e = cmd.Run(); e != nil {
					return 0, e
				}
			}
		}
	}
	fmt.Fprintln(os.Stderr, "Setup complete. Run pd-memory sync to import settled sources.")
	return 0, nil
}
