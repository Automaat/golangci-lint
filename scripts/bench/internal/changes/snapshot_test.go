package changes

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCaptureAndDiff(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "modified.txt"), []byte("before"), 0o600)
	writeTestFile(t, filepath.Join(root, "deleted.bin"), []byte{0, 1, 2}, 0o600)
	writeTestFile(t, filepath.Join(root, ".git", "ignored"), []byte("metadata"), 0o600)
	if symlinkErr := os.Symlink("modified.txt", filepath.Join(root, "link")); symlinkErr != nil {
		t.Fatal(symlinkErr)
	}

	before, err := Capture(root)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(root, "modified.txt"), []byte("after"), 0o700)
	if removeErr := os.Remove(filepath.Join(root, "deleted.bin")); removeErr != nil {
		t.Fatal(removeErr)
	}
	writeTestFile(t, filepath.Join(root, "added.bin"), []byte{3, 0, 4}, 0o600)
	if removeErr := os.Remove(filepath.Join(root, "link")); removeErr != nil {
		t.Fatal(removeErr)
	}
	if symlinkErr := os.Symlink("added.bin", filepath.Join(root, "link")); symlinkErr != nil {
		t.Fatal(symlinkErr)
	}

	after, err := Capture(root)
	if err != nil {
		t.Fatal(err)
	}
	set := Diff(before, after)
	if len(set.Changes) != 4 {
		t.Fatalf("expected 4 changes, got %+v", set.Changes)
	}
	want := map[string]string{
		"added.bin": "add", "deleted.bin": "delete", "link": "modify", "modified.txt": "modify",
	}
	for _, change := range set.Changes {
		if want[change.Path] != change.Kind {
			t.Fatalf("unexpected change: %+v", change)
		}
	}
	if set.Changes[0].Path != "added.bin" || set.Changes[len(set.Changes)-1].Path != "modified.txt" {
		t.Fatalf("changes are not sorted: %+v", set.Changes)
	}
}

func TestCaptureExcludesGitFile(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, ".git"), []byte("gitdir: elsewhere"), 0o600)
	writeTestFile(t, filepath.Join(root, "source.go"), []byte("package source"), 0o600)

	snapshot, err := Capture(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Files) != 1 || snapshot.Files[0].Path != "source.go" {
		t.Fatalf("unexpected snapshot: %+v", snapshot.Files)
	}
}

func TestEqualSetsIncludesModeAndType(t *testing.T) {
	regular := State{Type: "regular", Mode: 0o600, SHA256: "same"}
	executable := State{Type: "regular", Mode: 0o700, SHA256: "same"}
	left := Set{SchemaVersion: SchemaVersion, Changes: []Change{{Path: "file", Kind: "modify", After: &regular}}}
	right := Set{SchemaVersion: SchemaVersion, Changes: []Change{{Path: "file", Kind: "modify", After: &executable}}}
	if EqualSets(left, right) {
		t.Fatal("expected modes to differ")
	}
}

func writeTestFile(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}
