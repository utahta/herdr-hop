package tui

import "testing"

func TestFuzzyFilter(t *testing.T) {
	items := []string{"github.com/utahta/herdr-warm", "github.com/other/warm-drive", "gitlab.com/x/hw"}
	got := fuzzyFilter("hw", items)
	if len(got) != 3 {
		t.Fatalf("got %v", got)
	}
	// exact substring "hw" ranks first; boundary matches in herdr-warm beat the scattered warm-drive match.
	if got[0] != 2 || got[1] != 0 {
		t.Errorf("unexpected order %v", got)
	}
	if got := fuzzyFilter("hewa", items); len(got) == 0 || got[0] != 0 {
		t.Errorf("hewa: %v", got)
	}
	if got := fuzzyFilter("zzz", items); len(got) != 0 {
		t.Errorf("got %v", got)
	}
	if got := fuzzyFilter("", items); len(got) != 3 || got[0] != 0 || got[2] != 2 {
		t.Errorf("empty query should keep order: %v", got)
	}
	if got := fuzzyFilter("WARM", items); len(got) != 2 {
		t.Errorf("case-insensitive: %v", got)
	}
}
