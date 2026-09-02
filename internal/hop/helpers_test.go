package hop

import (
	"os"
	"testing"
)

func osMkdirAll(p string) error { return os.MkdirAll(p, 0o755) }

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
