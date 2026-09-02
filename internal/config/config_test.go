package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	cfg, err := load("", "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Depth != 3 || cfg.CloneProtocol != "https" || cfg.DefaultHost != "github.com" {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
	if len(cfg.SearchPaths) != 0 {
		t.Errorf("expected no search paths, got %v", cfg.SearchPaths)
	}
	if _, err := cfg.RequireRoot(); err != ErrNoRoot {
		t.Errorf("expected ErrNoRoot, got %v", err)
	}
}

func TestLoadFileAndRoot(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	extra := filepath.Join(dir, "extra")
	content := "search_paths = [\"" + extra + "\", \"" + root + "\"]\ndepth = 2\nclone_protocol = \"ssh\"\n"
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := load(dir, root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Root != root {
		t.Errorf("root = %q", cfg.Root)
	}
	if cfg.Depth != 2 || cfg.CloneProtocol != "ssh" {
		t.Errorf("unexpected: %+v", cfg)
	}
	// Root first, duplicates removed.
	if len(cfg.SearchPaths) != 2 || cfg.SearchPaths[0] != root || cfg.SearchPaths[1] != extra {
		t.Errorf("search paths = %v", cfg.SearchPaths)
	}
}

func TestRootFallbackFromFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "config.toml"), []byte("root = \""+dir+"/r\"\n"), 0o644)
	cfg, err := load(dir, "")
	if err != nil || cfg.Root != filepath.Join(dir, "r") {
		t.Errorf("root=%q err=%v", cfg.Root, err)
	}
	cfg, _ = load(dir, dir+"/env")
	if cfg.Root != filepath.Join(dir, "env") {
		t.Errorf("env should win: %q", cfg.Root)
	}
}

func TestScanTargetsRootAtLeastCloneDepth(t *testing.T) {
	cfg := Config{Root: "/r", SearchPaths: []string{"/r", "/other"}, Depth: 1}
	got := cfg.ScanTargets()
	if len(got) != 2 || got[0].Depth != CloneDepth || got[1].Depth != 1 {
		t.Errorf("got %+v", got)
	}
	cfg.Depth = 5
	if got := cfg.ScanTargets(); got[0].Depth != 5 {
		t.Errorf("configured deeper depth must be kept: %+v", got)
	}
}

func TestLoadMissingFileIsFine(t *testing.T) {
	if _, err := load(t.TempDir(), ""); err != nil {
		t.Fatal(err)
	}
}
