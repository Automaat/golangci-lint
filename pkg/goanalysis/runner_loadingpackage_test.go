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

	"github.com/golangci/golangci-lint/v2/internal/cache"
)

type loadingPackageTestFact struct {
	Value string
}

func (*loadingPackageTestFact) AFact() {}

type loadingPackageTestCache struct {
	reads  int
	writes int
	facts  map[string][]Fact
	key    string
}

func (c *loadingPackageTestCache) Get(_ *packages.Package, _ cache.HashMode, key string, data any) error {
	c.reads++
	c.key = key
	dst := data.(*map[string][]Fact)
	*dst = c.facts
	return nil
}

func (c *loadingPackageTestCache) Put(_ *packages.Package, _ cache.HashMode, key string, data any) error {
	c.writes++
	c.key = key
	c.facts = data.(map[string][]Fact)
	return nil
}

func TestLoadingPackageReadsFactBundleOnce(t *testing.T) {
	pkg := &packages.Package{Name: "example", PkgPath: "example.com/example"}
	analyzerA := &analysis.Analyzer{Name: "a", FactTypes: []analysis.Fact{new(loadingPackageTestFact)}}
	analyzerB := &analysis.Analyzer{Name: "b", FactTypes: []analysis.Fact{new(loadingPackageTestFact)}}
	cacheStore := &loadingPackageTestCache{facts: map[string][]Fact{
		"a": {{Fact: &loadingPackageTestFact{Value: "a"}}},
		"b": {},
	}}
	runner := &runner{prefix: "metalinter", pkgCache: cacheStore}
	actionA := &action{Analyzer: analyzerA, Package: pkg, runner: runner}
	actionB := &action{Analyzer: analyzerB, Package: pkg, runner: runner}
	lp := &loadingPackage{pkg: pkg, actions: []*action{actionA, actionB}}

	lp.readCachedFacts()
	lp.readCachedFacts()

	assert.Equal(t, 1, cacheStore.reads)
	assert.Equal(t, factCacheKey("metalinter", lp.actions), cacheStore.key)
	assert.True(t, actionA.loadCachedFactsOk)
	assert.True(t, actionB.loadCachedFactsOk)
	require.Len(t, actionA.cachedFacts, 1)
	assert.Empty(t, actionB.cachedFacts)
}

func TestLoadingPackageWritesMergedFactBundleOnce(t *testing.T) {
	typesPkg := types.NewPackage("example.com/example", "example")
	pkg := &packages.Package{Name: "example", PkgPath: typesPkg.Path(), Types: typesPkg}
	analyzerA := &analysis.Analyzer{Name: "a", FactTypes: []analysis.Fact{new(loadingPackageTestFact)}}
	analyzerB := &analysis.Analyzer{Name: "b", FactTypes: []analysis.Fact{new(loadingPackageTestFact)}}
	cacheStore := &loadingPackageTestCache{}
	runner := &runner{prefix: "metalinter", pkgCache: cacheStore}
	actionA := &action{
		Analyzer:          analyzerA,
		Package:           pkg,
		runner:            runner,
		needAnalyzeSource: true,
		pass:              &analysis.Pass{},
		packageFacts: map[packageFactKey]analysis.Fact{
			{pkg: typesPkg}: &loadingPackageTestFact{Value: "new-a"},
		},
	}
	actionB := &action{Analyzer: analyzerB, Package: pkg, runner: runner}
	lp := &loadingPackage{
		pkg:     pkg,
		actions: []*action{actionA, actionB},
		cachedFacts: map[string][]Fact{
			"a": {{Fact: &loadingPackageTestFact{Value: "old-a"}}},
			"b": {{Fact: &loadingPackageTestFact{Value: "cached-b"}}},
		},
	}

	require.NoError(t, lp.persistFactsToCache())

	assert.Equal(t, 1, cacheStore.writes)
	assert.Equal(t, factCacheKey("metalinter", lp.actions), cacheStore.key)
	require.Len(t, cacheStore.facts["a"], 1)
	assert.Equal(t, "new-a", cacheStore.facts["a"][0].Fact.(*loadingPackageTestFact).Value)
	require.Len(t, cacheStore.facts["b"], 1)
	assert.Equal(t, "cached-b", cacheStore.facts["b"][0].Fact.(*loadingPackageTestFact).Value)
}

func TestFactCacheKeyIncludesAnalyzerSet(t *testing.T) {
	analyzerA := &analysis.Analyzer{Name: "a", FactTypes: []analysis.Fact{new(loadingPackageTestFact)}}
	analyzerB := &analysis.Analyzer{Name: "b", FactTypes: []analysis.Fact{new(loadingPackageTestFact)}}
	actionA := &action{Analyzer: analyzerA}
	actionB := &action{Analyzer: analyzerB}

	keyAB := factCacheKey("metalinter", []*action{actionA, actionB})

	assert.Equal(t, keyAB, factCacheKey("metalinter", []*action{actionB, actionA}))
	assert.NotEqual(t, keyAB, factCacheKey("metalinter", []*action{actionA}))
}

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
