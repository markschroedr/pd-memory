package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func DefaultPath() (string, error) {
	home, e := os.UserHomeDir()
	if e != nil {
		return "", e
	}
	return filepath.Join(home, ".config", "pd-memory", "pd-memory.toml"), nil
}
func resolvePath(config, path string) string {
	if strings.HasPrefix(path, "~/") {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, path[2:])
	}
	if !filepath.IsAbs(path) {
		// Relative paths belong to the real config file, so a symlinked default path keeps working.
		if real, e := filepath.EvalSymlinks(config); e == nil {
			config = real
		}
		path = filepath.Join(filepath.Dir(config), path)
	}
	return filepath.Clean(path)
}
func (c *Config) sources() error {
	if d, e := time.ParseDuration(c.SettleAfter); e != nil || d <= 0 {
		return fmt.Errorf("settle_after must be a positive duration")
	}
	names := map[string]bool{}
	for i := range c.Sources {
		s := &c.Sources[i]
		if strings.TrimSpace(s.Name) == "" || names[s.Name] {
			return fmt.Errorf("source names must be nonempty and unique")
		}
		names[s.Name] = true
		if !Contains([]string{"pi", "claude-code", "codex", "files", "sqlite"}, s.Adapter) || !Contains(SourceKinds, s.Kind) || s.Path == "" {
			return fmt.Errorf("source %s requires adapter, path and kind", s.Name)
		}
		s.Path = resolvePath(c.Path, s.Path)
		if s.SettleAfter != "" {
			if d, e := time.ParseDuration(s.SettleAfter); e != nil || d <= 0 {
				return fmt.Errorf("source %s has invalid settle_after", s.Name)
			}
		}
		if s.After != "" {
			if _, e := time.Parse(time.RFC3339Nano, s.After); e != nil {
				return fmt.Errorf("source %s has invalid after", s.Name)
			}
		}
		if (s.Adapter == "sqlite") != (strings.TrimSpace(s.Query) != "") {
			return fmt.Errorf("only sqlite sources require query")
		}
		for _, pattern := range s.Exclude {
			if pattern == "" {
				return fmt.Errorf("empty exclude pattern")
			}
			for _, part := range strings.Split(pattern, "/") {
				if part != "**" {
					if _, e := filepath.Match(part, ""); e != nil {
						return fmt.Errorf("invalid exclude pattern %s", pattern)
					}
				}
			}
		}
	}
	for i := range c.Routes {
		r := &c.Routes[i]
		if r.Root == "" || r.Home == "" {
			return fmt.Errorf("routes require root and home")
		}
		r.Root = resolvePath(c.Path, r.Root)
	}
	return nil
}
func (c *Config) LoadCredentials() error {
	if c.CredentialsFile == "" {
		return nil
	}
	path := c.CredentialsFile
	b, e := os.ReadFile(path)
	if e != nil {
		return e
	}
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" || strings.ContainsAny(key, " \t") {
			return fmt.Errorf("credentials line %d must be NAME=value", i+1)
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
			value = value[1 : len(value)-1]
		}
		if os.Getenv(key) == "" {
			if e = os.Setenv(key, value); e != nil {
				return e
			}
		}
	}
	return nil
}
