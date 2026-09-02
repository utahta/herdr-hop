// Package config loads herdr-hop settings from the plugin config dir and environment.
package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/utahta/herdr-hop/internal/scan"
)

// Config is the effective configuration.
type Config struct {
	// Root is the clone destination ($HERDR_HOP_ROOT, or "root" in config.toml). Empty if unset.
	Root string
	// SearchPaths are directories to scan for repositories. Root is always included first.
	SearchPaths []string
	// Depth is the maximum scan depth below each search path.
	Depth int
	// CloneProtocol is "https" or "ssh".
	CloneProtocol string
	// DefaultHost is used to complete "owner/repo" clone inputs.
	DefaultHost string
}

type fileConfig struct {
	Root          string   `toml:"root"`
	SearchPaths   []string `toml:"search_paths"`
	Depth         int      `toml:"depth"`
	CloneProtocol string   `toml:"clone_protocol"`
	DefaultHost   string   `toml:"default_host"`
}

const (
	EnvRoot         = "HERDR_HOP_ROOT"
	envPluginConfig = "HERDR_PLUGIN_CONFIG_DIR"
	defaultDepth    = 3
	defaultProtocol = "https"
	defaultHost     = "github.com"
)

// ErrNoRoot is returned by RequireRoot when HERDR_HOP_ROOT is unset.
var ErrNoRoot = errors.New(EnvRoot + " is not set")

// Load reads config.toml from $HERDR_PLUGIN_CONFIG_DIR (if present) and merges env.
func Load() (Config, error) {
	return load(os.Getenv(envPluginConfig), os.Getenv(EnvRoot))
}

func load(configDir, root string) (Config, error) {
	var fc fileConfig
	if configDir != "" {
		path := filepath.Join(configDir, "config.toml")
		if _, err := toml.DecodeFile(path, &fc); err != nil && !errors.Is(err, os.ErrNotExist) {
			return Config{}, err
		}
	}
	if root == "" {
		root = fc.Root // env wins; config.toml is the fallback
	}
	cfg := Config{
		Root:          expand(root),
		Depth:         fc.Depth,
		CloneProtocol: fc.CloneProtocol,
		DefaultHost:   fc.DefaultHost,
	}
	if cfg.Depth <= 0 {
		cfg.Depth = defaultDepth
	}
	if cfg.CloneProtocol == "" {
		cfg.CloneProtocol = defaultProtocol
	}
	if cfg.DefaultHost == "" {
		cfg.DefaultHost = defaultHost
	}
	seen := map[string]bool{}
	add := func(p string) {
		p = expand(p)
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		cfg.SearchPaths = append(cfg.SearchPaths, p)
	}
	add(cfg.Root)
	for _, p := range fc.SearchPaths {
		add(p)
	}
	return cfg, nil
}

// CloneDepth is the depth of a clone destination below Root
// (Root/host/owner/repo), and therefore the minimum depth Root must be
// scanned at for cloned repositories to be found again.
const CloneDepth = 3

// ScanTargets returns the search paths with their scan depth. Root is always
// scanned at least CloneDepth deep so that a freshly cloned repository shows
// up in the list regardless of the configured depth; other paths use Depth.
func (c Config) ScanTargets() []scan.Target {
	out := make([]scan.Target, 0, len(c.SearchPaths))
	for _, p := range c.SearchPaths {
		d := c.Depth
		if p == c.Root && d < CloneDepth {
			d = CloneDepth
		}
		out = append(out, scan.Target{Path: p, Depth: d})
	}
	return out
}

// RequireRoot returns Root or ErrNoRoot.
func (c Config) RequireRoot() (string, error) {
	if c.Root == "" {
		return "", ErrNoRoot
	}
	return c.Root, nil
}

// expand resolves a leading "~" and cleans the path. Empty stays empty.
func expand(p string) string {
	if p == "" {
		return ""
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	return filepath.Clean(p)
}
