package cache

import (
	"bytes"
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestHashFileLargerThanCopyBuffer(t *testing.T) {
	content := bytes.Repeat([]byte("golangci-lint"), 10_000)
	name := filepath.Join(t.TempDir(), "large.go")
	if err := os.WriteFile(name, content, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := FileHash(name)
	if err != nil {
		t.Fatal(err)
	}
	if want := sha256.Sum256(content); got != want {
		t.Errorf("FileHash = %x, want %x", got, want)
	}
}

func TestCopyFileReusesBuffer(t *testing.T) {
	name := filepath.Join(t.TempDir(), "small.go")
	if err := os.WriteFile(name, []byte("package p\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	h := sha256.New()
	allocs := testing.AllocsPerRun(100, func() {
		h.Reset()
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		if _, err := copyFile(h, f); err != nil {
			t.Fatal(err)
		}
	})
	if allocs != 0 {
		t.Errorf("copyFile allocates %.0f times per call, want 0", allocs)
	}
}
