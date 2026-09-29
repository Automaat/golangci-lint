package lint

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

func pkgPaths(pkgs []*packages.Package) []string {
	var paths []string
	for _, pkg := range pkgs {
		paths = append(paths, pkg.PkgPath)
	}
	return paths
}

func TestChangedPackages(t *testing.T) {
	wd := t.TempDir()
	file := func(name string) string { return filepath.Join(wd, name) }

	broken := &packages.Package{PkgPath: "broken", GoFiles: []string{file("broken/x.go")}, Errors: []packages.Error{{Msg: "compile error"}}}
	a := &packages.Package{PkgPath: "a", GoFiles: []string{file("a/a.go")}, CompiledGoFiles: []string{file("a/a.go")}}
	b := &packages.Package{PkgPath: "b", GoFiles: []string{file("b/b.go")}, Imports: map[string]*packages.Package{"a": a}}
	c := &packages.Package{PkgPath: "c", GoFiles: []string{file("c/c.go")}, Imports: map[string]*packages.Package{"broken": broken}}
	d := &packages.Package{PkgPath: "d", GoFiles: []string{file("d/d.go")}, IgnoredFiles: []string{file("d/d_windows.go")}}
	e := &packages.Package{PkgPath: "e", GoFiles: []string{file("e/e.go")}, EmbedFiles: []string{file("e/data.txt")}, OtherFiles: []string{file("e/e.s")}}
	all := []*packages.Package{a, b, c, d, e}

	tests := []struct {
		desc    string
		changed []string
		want    []string
		wantAll bool
	}{
		{desc: "dependents of a changed package are not needed", changed: []string{"a/a.go"}, want: []string{"a", "c"}},
		{desc: "absolute new file", changed: []string{file("b/b.go")}, want: []string{"b", "c"}},
		{desc: "ignored file", changed: []string{"d/d_windows.go"}, want: []string{"c", "d"}},
		{desc: "embed and assembly files", changed: []string{"e/data.txt", "e/e.s"}, want: []string{"c", "e"}},
		{desc: "unrelated files", changed: []string{"README.md", "testdata/x.go"}, want: []string{"c"}},
		{desc: "go.mod", changed: []string{"a/a.go", "go.mod"}, wantAll: true},
		{desc: "go.work", changed: []string{"go.work"}, wantAll: true},
	}

	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			kept, ok := changedPackages(all, test.changed, wd)
			if test.wantAll {
				assert.False(t, ok)
				return
			}

			require.True(t, ok)
			assert.ElementsMatch(t, test.want, pkgPaths(kept))
		})
	}
}

func TestChangedPackagesResolvesSymlinks(t *testing.T) {
	realDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(realDir, "a"), 0o750))

	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	pkg := &packages.Package{PkgPath: "a", GoFiles: []string{filepath.Join(realDir, "a", "a.go")}}

	kept, ok := changedPackages([]*packages.Package{pkg}, []string{"a/a.go"}, link)
	require.True(t, ok)
	assert.Equal(t, []string{"a"}, pkgPaths(kept))
}
