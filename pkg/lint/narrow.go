package lint

import (
	"os"
	"path/filepath"
	"slices"

	"golang.org/x/tools/go/packages"
)

// envDiffAnalyzeAll disables changed-package narrowing when set to "1".
const envDiffAnalyzeAll = "GOLANGCI_LINT_DIFF_ANALYZE_ALL"

// moduleFiles are files whose changes can produce issues outside any package,
// so narrowing is disabled when they change.
var moduleFiles = []string{"go.mod", "go.work"}

// changedPackages selects the packages whose issues can survive the diff filter:
// the filter only keeps issues located in changed files, and always keeps typecheck issues.
// It keeps packages owning a changed file and packages whose own or dependencies'
// load errors produce typecheck issues.
// It returns false when every package must be analyzed.
func changedPackages(pkgs []*packages.Package, changedFiles []string, wd string) ([]*packages.Package, bool) {
	changed := newPathSet()
	for _, file := range changedFiles {
		if slices.Contains(moduleFiles, filepath.Base(file)) {
			return nil, false
		}
		if !filepath.IsAbs(file) {
			file = filepath.Join(wd, filepath.FromSlash(file))
		}
		changed.add(file)
	}

	hasErrors := errorReachability()

	var kept []*packages.Package
	for _, pkg := range pkgs {
		if hasErrors(pkg) || changed.containsAny(pkg.GoFiles, pkg.CompiledGoFiles, pkg.OtherFiles, pkg.IgnoredFiles, pkg.EmbedFiles) {
			kept = append(kept, pkg)
		}
	}

	return kept, true
}

// errorReachability reports whether a package or any of its transitive imports has load errors.
func errorReachability() func(*packages.Package) bool {
	memo := map[*packages.Package]bool{}

	var visit func(*packages.Package) bool
	visit = func(pkg *packages.Package) bool {
		if result, ok := memo[pkg]; ok {
			return result
		}

		memo[pkg] = false

		result := len(pkg.Errors) > 0
		for _, imp := range pkg.Imports {
			if visit(imp) {
				result = true
			}
		}

		memo[pkg] = result

		return result
	}

	return visit
}

// pathSet matches file paths directly or through their symlink-resolved directory,
// since the working directory and go list paths may differ in symlinks.
type pathSet struct {
	paths    map[string]bool
	resolved map[string]string
}

func newPathSet() *pathSet {
	return &pathSet{paths: map[string]bool{}, resolved: map[string]string{}}
}

func (s *pathSet) add(path string) {
	s.paths[filepath.Clean(path)] = true
	s.paths[s.resolve(path)] = true
}

func (s *pathSet) containsAny(lists ...[]string) bool {
	for _, list := range lists {
		for _, path := range list {
			if s.paths[filepath.Clean(path)] || s.paths[s.resolve(path)] {
				return true
			}
		}
	}

	return false
}

func (s *pathSet) resolve(path string) string {
	dir, base := filepath.Split(filepath.Clean(path))

	resolvedDir, ok := s.resolved[dir]
	if !ok {
		resolvedDir = dir
		if evaluated, err := filepath.EvalSymlinks(dir); err == nil {
			resolvedDir = evaluated
		}
		s.resolved[dir] = resolvedDir
	}

	return filepath.Join(resolvedDir, base)
}

func diffNarrowingDisabled() bool {
	return os.Getenv(envDiffAnalyzeAll) == "1"
}
