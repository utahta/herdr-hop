// Package logging writes a plain log file under $HERDR_PLUGIN_STATE_DIR.
package logging

import (
	"io"
	"log"
	"os"
	"path/filepath"
)

// Setup returns a logger writing to $HERDR_PLUGIN_STATE_DIR/herdr-hop.log,
// or a discarding logger if the dir is unset/unwritable.
func Setup() *log.Logger {
	dir := os.Getenv("HERDR_PLUGIN_STATE_DIR")
	if dir == "" {
		return log.New(io.Discard, "", 0)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return log.New(io.Discard, "", 0)
	}
	f, err := os.OpenFile(filepath.Join(dir, "herdr-hop.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return log.New(io.Discard, "", 0)
	}
	return log.New(f, "", log.LstdFlags)
}
