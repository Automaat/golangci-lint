package goanalysis

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/types/objectpath"
)

type Fact struct {
	Path string // non-empty only for object facts
	Fact analysis.Fact
}

func (act *action) loadCachedFacts() bool {
	if !act.readCachedFacts() {
		return false
	}

	act.applyCachedFacts()

	return true
}

func (act *action) readCachedFacts() bool {
	if act.loadCachedFactsDone { // can't be set in parallel
		return act.loadCachedFactsOk
	}

	res := func() bool {
		if act.isInitialPkg {
			return true // load cached facts only for non-initial packages
		}

		if len(act.Analyzer.FactTypes) == 0 {
			return true // no need to load facts
		}

		if act.cachedFacts == nil {
			return false
		}

		return true
	}()

	act.loadCachedFactsDone = true
	act.loadCachedFactsOk = res

	return res
}

func (act *action) applyCachedFacts() {
	if act.cachedFactsApplied || !act.loadCachedFactsOk || act.isInitialPkg || len(act.Analyzer.FactTypes) == 0 {
		return
	}

	act.applyPersistedFacts(act.cachedFacts)
	act.cachedFacts = nil
	act.cachedFactsApplied = true

	// The cache only stores facts a package produces about its own objects.
	// Rebuild re-exported facts from dependencies after package types are loaded.
	act.inheritFactsFromDeps()
}

// inheritFactsFromDeps rebuilds, in memory,
// the facts re-exported from this action's dependencies (vertical edges: same analyzer, different package),
// mirroring the inheritance performed by analyze() for packages analyzed from source.
func (act *action) inheritFactsFromDeps() {
	for _, dep := range act.Deps {
		if dep.Package == act.Package || dep.Analyzer != act.Analyzer {
			continue
		}

		inheritFacts(act, dep)
	}
}

func (act *action) persistedFacts() []Fact {
	analyzer := act.Analyzer

	if len(analyzer.FactTypes) == 0 {
		return nil
	}

	// Persist only the facts this package produces about its own objects.
	//
	// Facts about objects from other packages (inherited through this package's export data) are intentionally NOT persisted:
	// doing so duplicates them in every cache entry along the import graph and makes the cache grow quadratically.
	// When a package is restored from the cache,
	// those re-exported facts are rebuilt in memory by inheriting from its dependencies (see inheritFactsFromDeps).

	var facts []Fact

	for key, fact := range act.packageFacts {
		if key.pkg != act.Package.Types {
			// The fact is inherited from another package.
			continue
		}

		facts = append(facts, Fact{
			Path: "",
			Fact: fact,
		})
	}

	for key, fact := range act.objectFacts {
		obj := key.obj

		if obj.Pkg() != act.Package.Types {
			// The fact is inherited from another package.
			continue
		}

		path, err := objectpath.For(obj)
		if err != nil {
			// The object is not globally addressable
			continue
		}

		facts = append(facts, Fact{
			Path: string(path),
			Fact: fact,
		})
	}

	factsCacheDebugf("Caching %d facts for package %q and analyzer %s", len(facts), act.Package.Name, act.Analyzer.Name)

	return facts
}

func (act *action) applyPersistedFacts(facts []Fact) {
	for _, f := range facts {
		if f.Path == "" { // this is a package fact
			key := packageFactKey{pkg: act.Package.Types, typ: act.factType(f.Fact)}
			act.packageFacts[key] = f.Fact
			continue
		}

		obj, err := objectpath.Object(act.Package.Types, objectpath.Path(f.Path))
		if err != nil {
			// Be lenient about these errors.
			// For example, when analyzing io/ioutil from source,
			// we may get a fact for methods on the devNull type,
			// and objectpath will happily create a path for them.
			// However,
			// when we later load io/ioutil from export data,
			// the path no longer resolves.
			//
			// If an exported type embeds the unexported type,
			// then (part of) the unexported type will become part of the type information and our path will resolve again.
			continue
		}
		factKey := objectFactKey{obj, act.factType(f.Fact)}
		act.objectFacts[factKey] = f.Fact
	}
}

func factCacheKey(prefix string, actions []*action) string {
	var analyzerNames []string
	for _, act := range actions {
		if len(act.Analyzer.FactTypes) != 0 {
			analyzerNames = append(analyzerNames, act.Analyzer.Name)
		}
	}
	slices.Sort(analyzerNames)

	hash := sha256.New()
	for _, name := range analyzerNames {
		fmt.Fprintf(hash, "%d:%s\n", len(name), name)
	}

	return prefix + "/facts-v2/" + hex.EncodeToString(hash.Sum(nil))
}
