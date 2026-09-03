package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteReplacesExistingFile(t *testing.T) {
	name := filepath.Join(t.TempDir(), "current.json")
	if err := Write(name, []byte("first\n"), 0o600); err != nil {
		t.Fatalf("write first file: %v", err)
	}
	if err := Write(name, []byte("second\n"), 0o600); err != nil {
		t.Fatalf("replace existing file: %v", err)
	}
	contents, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read replaced file: %v", err)
	}
	if got, want := string(contents), "second\n"; got != want {
		t.Fatalf("contents = %q, want %q", got, want)
	}
}
