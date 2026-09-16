package goanalysis

import (
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/packages"
)

type loadingPackageTestFact struct {
	Value string
}

func (*loadingPackageTestFact) AFact() {}

func TestLoadingPackageFactHitUsesExportData(t *testing.T) {
	loaded, err := packages.Load(&packages.Config{
		Mode: packages.NeedName | packages.NeedImports | packages.NeedDeps | packages.NeedExportFile,
	}, "errors")
	require.NoError(t, err)
	require.Len(t, loaded, 1)

	pkg := loaded[0]
	pkg.Fset = token.NewFileSet()
	seen := map[*packages.Package]bool{}
	var prepareImports func(*packages.Package)
	prepareImports = func(current *packages.Package) {
		if seen[current] {
			return
		}
		seen[current] = true
		for _, imported := range current.Imports {
			imported.Types = types.NewPackage(imported.PkgPath, imported.Name)
			prepareImports(imported)
		}
	}
	prepareImports(pkg)
	pkg.Types = nil
	analyzer := &analysis.Analyzer{
		Name:      "facts",
		FactTypes: []analysis.Fact{new(loadingPackageTestFact)},
	}
	act := &action{
		Analyzer:            analyzer,
		Package:             pkg,
		loadCachedFactsDone: true,
		loadCachedFactsOk:   true,
		cachedFacts: []Fact{{
			Fact: &loadingPackageTestFact{Value: "cached"},
		}},
		packageFacts: map[packageFactKey]analysis.Fact{},
		objectFacts:  map[objectFactKey]analysis.Fact{},
	}
	metrics := &schedulerMetrics{}
	lp := &loadingPackage{
		pkg:       pkg,
		actions:   []*action{act},
		scheduler: metrics,
	}

	require.NoError(t, lp.loadImportedPackageWithFacts(LoadModeTypesInfo))

	assert.EqualValues(t, 0, metrics.sourceLoads.Load())
	assert.EqualValues(t, 1, metrics.exportLoads.Load())
	assert.True(t, act.cachedFactsApplied)
	require.Len(t, act.packageFacts, 1)
}

func TestLoadingPackageFactMissSkipsExportData(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "package.go")
	require.NoError(t, os.WriteFile(sourcePath, []byte("package example\n"), 0o600))

	pkg := &packages.Package{
		Name:            "example",
		PkgPath:         "example.com/example",
		Fset:            token.NewFileSet(),
		CompiledGoFiles: []string{sourcePath},
		Imports:         map[string]*packages.Package{},
	}
	analyzer := &analysis.Analyzer{
		Name:      "facts",
		FactTypes: []analysis.Fact{new(loadingPackageTestFact)},
	}
	hit := &action{
		Analyzer:            analyzer,
		Package:             pkg,
		loadCachedFactsDone: true,
		loadCachedFactsOk:   true,
		cachedFacts: []Fact{{
			Fact: &loadingPackageTestFact{Value: "cached"},
		}},
		packageFacts: map[packageFactKey]analysis.Fact{},
		objectFacts:  map[objectFactKey]analysis.Fact{},
	}
	miss := &action{
		Analyzer:            analyzer,
		Package:             pkg,
		loadCachedFactsDone: true,
		packageFacts:        map[packageFactKey]analysis.Fact{},
		objectFacts:         map[objectFactKey]analysis.Fact{},
	}
	metrics := &schedulerMetrics{}
	lp := &loadingPackage{
		pkg:       pkg,
		actions:   []*action{hit, miss},
		scheduler: metrics,
	}

	require.NoError(t, lp.loadImportedPackageWithFacts(LoadModeTypesInfo))

	assert.EqualValues(t, 1, metrics.sourceLoads.Load())
	assert.EqualValues(t, 0, metrics.exportLoads.Load())
	assert.True(t, miss.needAnalyzeSource)
	assert.True(t, hit.cachedFactsApplied)
	require.Len(t, hit.packageFacts, 1)
	for key, fact := range hit.packageFacts {
		assert.Same(t, pkg.Types, key.pkg)
		assert.Equal(t, "cached", fact.(*loadingPackageTestFact).Value)
	}
}
