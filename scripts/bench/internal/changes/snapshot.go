package changes

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
)

// SchemaVersion is the canonical change-set schema.
const SchemaVersion = 1

// State identifies file content and metadata relevant to lint fixes.
type State struct {
	Type   string `json:"type"`
	Mode   uint32 `json:"mode"`
	SHA256 string `json:"sha256"`
}

// File is one path in a filesystem snapshot.
type File struct {
	Path string `json:"path"`
	State
}

// Snapshot is a deterministic view of a worktree excluding Git metadata.
type Snapshot struct {
	Files []File `json:"files"`
}

// Change records one difference between two snapshots.
type Change struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Before *State `json:"before,omitempty"`
	After  *State `json:"after,omitempty"`
}

// Set is the canonical file-change representation.
type Set struct {
	SchemaVersion int      `json:"schema_version"`
	Changes       []Change `json:"changes"`
}

// Capture hashes every regular file and symlink below root except .git.
func Capture(root string) (Snapshot, error) {
	var snapshot Snapshot
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return fmt.Errorf("resolve snapshot path: %w", err)
		}
		if rel == "." {
			return nil
		}
		if rel == ".git" {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}

		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("read snapshot metadata %s: %w", rel, err)
		}
		state := State{Mode: uint32(info.Mode().Perm())}
		switch {
		case info.Mode().IsRegular():
			state.Type = "regular"
			state.SHA256, err = fileSHA256(path)
		case info.Mode()&os.ModeSymlink != 0:
			state.Type = "symlink"
			var target string
			target, err = os.Readlink(path)
			state.SHA256 = sha256Hex([]byte(target))
		default:
			return fmt.Errorf("unsupported file type %s at %s", info.Mode().Type(), rel)
		}
		if err != nil {
			return fmt.Errorf("read snapshot content %s: %w", rel, err)
		}
		snapshot.Files = append(snapshot.Files, File{Path: filepath.ToSlash(rel), State: state})

		return nil
	})
	if err != nil {
		return Snapshot{}, fmt.Errorf("capture %s: %w", root, err)
	}
	slices.SortFunc(snapshot.Files, func(a, b File) int { return comparePath(a.Path, b.Path) })

	return snapshot, nil
}

// Equal reports whether two snapshots contain identical paths and states.
func Equal(a, b Snapshot) bool {
	return slices.Equal(a.Files, b.Files)
}

// Diff returns deterministic add, modify, and delete records.
func Diff(before, after Snapshot) Set {
	beforeByPath := index(before)
	afterByPath := index(after)
	paths := make([]string, 0, len(beforeByPath)+len(afterByPath))
	for path := range beforeByPath {
		paths = append(paths, path)
	}
	for path := range afterByPath {
		if _, exists := beforeByPath[path]; !exists {
			paths = append(paths, path)
		}
	}
	slices.Sort(paths)

	result := Set{SchemaVersion: SchemaVersion}
	for _, path := range paths {
		beforeState, hadBefore := beforeByPath[path]
		afterState, hasAfter := afterByPath[path]
		switch {
		case !hadBefore:
			state := afterState
			result.Changes = append(result.Changes, Change{Path: path, Kind: "add", After: &state})
		case !hasAfter:
			state := beforeState
			result.Changes = append(result.Changes, Change{Path: path, Kind: "delete", Before: &state})
		case beforeState != afterState:
			oldState, newState := beforeState, afterState
			result.Changes = append(result.Changes, Change{
				Path: path, Kind: "modify", Before: &oldState, After: &newState,
			})
		}
	}
	if result.Changes == nil {
		result.Changes = []Change{}
	}

	return result
}

// EqualSets reports whether two canonical change sets are identical.
func EqualSets(a, b Set) bool {
	return a.SchemaVersion == b.SchemaVersion && slices.EqualFunc(a.Changes, b.Changes, func(left, right Change) bool {
		return left.Path == right.Path && left.Kind == right.Kind && equalState(left.Before, right.Before) &&
			equalState(left.After, right.After)
	})
}

// Marshal returns deterministic indented JSON.
func Marshal(set Set) ([]byte, error) {
	data, err := json.MarshalIndent(set, "", "  ")
	if err != nil {
		return nil, err
	}

	return append(data, '\n'), nil
}

// SHA256 returns the digest of the canonical JSON representation.
func SHA256(set Set) (string, error) {
	data, err := Marshal(set)
	if err != nil {
		return "", err
	}

	return sha256Hex(data), nil
}

func index(snapshot Snapshot) map[string]State {
	result := make(map[string]State, len(snapshot.Files))
	for _, file := range snapshot.Files {
		result[file.Path] = file.State
	}

	return result
}

func equalState(a, b *State) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}

	return *a == *b
}

func comparePath(a, b string) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}

	return 0
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:])
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}

	return hex.EncodeToString(hash.Sum(nil)), nil
}
