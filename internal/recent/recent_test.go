package recent

import (
	"os"
	"sync"
	"testing"
	"time"
)

func TestVisitsPersistAndAreScoped(t *testing.T) {
	dir := t.TempDir()
	store := New(dir, "session-a")
	first, second := time.Unix(100, 0), time.Unix(200, 0)
	for _, visit := range []struct {
		id string
		at time.Time
	}{{"w1", first}, {"w2", first}, {"w1", second}} {
		if err := store.Record(visit.id, visit.at); err != nil {
			t.Fatal(err)
		}
	}
	visits, err := New(dir, "session-a").Load([]string{"w1", "w2", "missing", "w1"})
	if err != nil || len(visits) != 2 || visits["w1"] != second.UnixNano() || visits["w2"] != first.UnixNano() {
		t.Fatalf("persisted visits: %v, %v", visits, err)
	}
	other, err := New(dir, "session-b").Load([]string{"w1"})
	if err != nil || len(other) != 0 {
		t.Fatalf("session histories overlap: %v, %v", other, err)
	}
}

func TestConcurrentVisitsDoNotLoseOtherWorkspaces(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	for _, id := range []string{"w1", "w2", "w3"} {
		wg.Go(func() {
			if err := New(dir, "session").Record(id, time.Unix(100, 0)); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	visits, err := New(dir, "session").Load([]string{"w1", "w2", "w3"})
	if err != nil || len(visits) != 3 {
		t.Fatalf("concurrent visits: %v, %v", visits, err)
	}
}

func TestMissingAndCorruptHistory(t *testing.T) {
	store := New(t.TempDir(), "session")
	if err := store.Record("valid", time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.path("corrupt"), []byte("not a timestamp"), 0o600); err != nil {
		t.Fatal(err)
	}
	visits, err := store.Load([]string{"valid", "missing", "corrupt"})
	if err == nil || visits["valid"] != time.Unix(100, 0).UnixNano() || visits["corrupt"] != 0 {
		t.Fatalf("partial history: %v, %v", visits, err)
	}
	for _, disabled := range []*Store{nil, New("", "session")} {
		if err := disabled.Record("w1", time.Now()); err != nil {
			t.Fatal(err)
		}
		if visits, err := disabled.Load([]string{"w1"}); err != nil || len(visits) != 0 {
			t.Fatalf("disabled history: %v, %v", visits, err)
		}
	}
}
