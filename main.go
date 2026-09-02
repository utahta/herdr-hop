// herdr-hop: herdr plugin for clone / hop / worktree.
package main

import (
	"flag"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/utahta/herdr-hop/internal/config"
	"github.com/utahta/herdr-hop/internal/forge"
	"github.com/utahta/herdr-hop/internal/gitx"
	"github.com/utahta/herdr-hop/internal/herdr"
	"github.com/utahta/herdr-hop/internal/logging"
	"github.com/utahta/herdr-hop/internal/tui"
)

const pluginID = "utahta.hop"

// version is stamped at release time via -ldflags "-X main.version=…".
var version = "dev"

func usage() {
	fmt.Fprintln(os.Stderr, "usage: herdr-hop <tui|open> --mode <hop|worktree>")
	fmt.Fprintln(os.Stderr, "       herdr-hop --version")
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	if os.Args[1] == "--version" || os.Args[1] == "version" {
		fmt.Println("herdr-hop " + version)
		return
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	mode := fs.String("mode", "hop", "hop|worktree")
	_ = fs.Parse(os.Args[2:])
	if *mode != "hop" && *mode != "worktree" {
		usage()
	}
	lg := logging.Setup()
	h := herdr.NewCLI()

	switch os.Args[1] {
	case "open":
		id := os.Getenv("HERDR_PLUGIN_ID")
		if id == "" {
			id = pluginID
		}
		if err := h.PluginPaneOpen(id, *mode); err != nil {
			lg.Printf("open %s: %v", *mode, err)
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "tui":
		cfg, err := config.Load()
		if err != nil {
			lg.Printf("config: %v", err)
			fmt.Fprintln(os.Stderr, "config:", err)
			os.Exit(1)
		}
		model := tui.NewHop(cfg, h, gitx.New(), lg, *mode == "worktree")
		if gh := forge.NewGitHub(); gh != nil {
			gh.Log = lg
			model = model.WithForge(gh)
		} else {
			lg.Printf("tui: gh not found; pull request titles are not shown")
		}
		if _, err := tea.NewProgram(model, tea.WithAltScreen()).Run(); err != nil {
			lg.Printf("tui: %v", err)
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	default:
		usage()
	}
}
