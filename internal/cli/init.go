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
	keyPrompt := "OpenRouter API key (stored locally in a private credentials file): "
	var key string
	if term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprint(os.Stderr, keyPrompt)
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return 0, err
		}
		key = string(b)
	} else {
		key, e = ask(keyPrompt)
		if e != nil {
			return 0, e
		}
	}
	if key == "" || strings.ContainsAny(key, "\r\n\"' ") {
		return 0, fmt.Errorf("key required, without whitespace or quotes")
	}
	piSetup, e := ask("Add Pi session import? [y/N]: ")
	if e != nil {
		return 0, e
	}
	raw := map[string]any{"settle_after": "3h", "auto_sync": true, "openai": map[string]any{"retention_verified": true}}
	home, e := os.UserHomeDir()
	if e != nil {
		return 0, e
	}
	raw["workspace"] = map[string]any{"dir": filepath.Join(home, ".local", "share", "pd-memory")}
	credentials := filepath.Join(filepath.Dir(path), "credentials.env")
	raw["credentials_file"] = credentials
	raw["identity"] = map[string]any{"user_names": []string{name}, "third_party_names": []string{}}
	// Requests require OpenRouter's ZDR-only routing; the live check verifies the route.
	if strings.EqualFold(piSetup, "y") || strings.EqualFold(piSetup, "yes") {
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
	if e = writeNew(credentials, []byte("OPENROUTER_API_KEY="+key+"\n")); e != nil {
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
	fmt.Fprintln(os.Stderr, "Config:", path, "\nWorkspace:", c.Workspace.Dir, "\nChecking generation and embeddings with ZDR-only provider routing...")
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
			if strings.EqualFold(install, "y") || strings.EqualFold(install, "yes") {
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
