// Package recent persists hop visits without watching workspace activity.
package recent

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Store struct{ dir string }

// New scopes workspace IDs to the herdr connection. An empty dir disables storage.
func New(dir, connection string) *Store {
	if dir == "" {
		return &Store{}
	}
	return &Store{dir: filepath.Join(dir, "recent", fmt.Sprintf("%x", sha256.Sum256([]byte(connection))))}
}

func (s *Store) path(id string) string {
	return filepath.Join(s.dir, fmt.Sprintf("%x", sha256.Sum256([]byte(id))))
}

// Load reads only live workspace IDs; closed workspaces do not affect startup cost.
func (s *Store) Load(ids []string) (map[string]int64, error) {
	visits := map[string]int64{}
	if s == nil || s.dir == "" {
		return visits, nil
	}
	var errs []error
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		data, err := os.ReadFile(s.path(id))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err == nil {
			var at int64
			at, err = strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
			if err == nil {
				visits[id] = at
			}
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("history for %s: %w", id, err))
		}
	}
	return visits, errors.Join(errs...)
}

// Record replaces one workspace's timestamp atomically so concurrent popups
// visiting different workspaces cannot overwrite each other's history.
func (s *Store) Record(id string, at time.Time) error {
	if s == nil || s.dir == "" || id == "" {
		return nil
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(s.dir, ".visit-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, writeErr := f.WriteString(strconv.FormatInt(at.UnixNano(), 10) + "\n")
	closeErr := f.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return err
	}
	return os.Rename(f.Name(), s.path(id))
}
