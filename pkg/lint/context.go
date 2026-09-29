package lint

import (
	"context"
	"fmt"
	"os"
	"time"

	"golang.org/x/tools/go/packages"

	"github.com/golangci/golangci-lint/v2/internal/cache"
	"github.com/golangci/golangci-lint/v2/pkg/config"
	"github.com/golangci/golangci-lint/v2/pkg/exitcodes"
	"github.com/golangci/golangci-lint/v2/pkg/goanalysis/load"
	"github.com/golangci/golangci-lint/v2/pkg/lint/lifecycle"
	"github.com/golangci/golangci-lint/v2/pkg/lint/linter"
	"github.com/golangci/golangci-lint/v2/pkg/logutils"
	"github.com/golangci/golangci-lint/v2/pkg/result/processors"
)

type ContextBuilder struct {
	cfg *config.Config

	pkgLoader *PackageLoader

	pkgCache *cache.Cache

	loadGuard *load.Guard

	lifecycle *lifecycle.Recorder
}

func NewContextBuilder(cfg *config.Config, pkgLoader *PackageLoader,
	pkgCache *cache.Cache, loadGuard *load.Guard,
) *ContextBuilder {
	return &ContextBuilder{
		cfg:       cfg,
		pkgLoader: pkgLoader,
		pkgCache:  pkgCache,
		loadGuard: loadGuard,
	}
}

// WithLifecycle enables lifecycle recording for contexts built by this builder.
func (cl *ContextBuilder) WithLifecycle(recorder *lifecycle.Recorder) *ContextBuilder {
	cl.lifecycle = recorder

	return cl
}

// Build loads packages and constructs the shared linter context.
func (cl *ContextBuilder) Build(ctx context.Context, log logutils.Log, linters []*linter.Config) (*linter.Context, error) {
	var started time.Time
	if cl.lifecycle != nil {
		started = time.Now()
	}

	pkgs, deduplicatedPkgs, err := cl.pkgLoader.Load(ctx, linters)
	if err != nil {
		loadErr := fmt.Errorf("failed to load packages: %w", err)
		cl.recordPackageLoad(started, len(pkgs), len(deduplicatedPkgs), loadErr)

		return nil, loadErr
	}

	if len(deduplicatedPkgs) == 0 {
		loadErr := fmt.Errorf("%w: running `go mod tidy` may solve the problem", exitcodes.ErrNoGoFiles)
		cl.recordPackageLoad(started, len(pkgs), 0, loadErr)

		return nil, loadErr
	}
	cl.recordPackageLoad(started, len(pkgs), len(deduplicatedPkgs), nil)

	pkgs, deduplicatedPkgs = cl.narrowToChanges(log, pkgs, deduplicatedPkgs)

	ret := &linter.Context{
		Packages: deduplicatedPkgs,

		// At least `unused` linters works properly only on original (not deduplicated) packages,
		// see https://github.com/golangci/golangci-lint/pull/585.
		OriginalPackages: pkgs,

		Cfg:       cl.cfg,
		Log:       log,
		PkgCache:  cl.pkgCache,
		LoadGuard: cl.loadGuard,
		Lifecycle: cl.lifecycle,
	}

	return ret, nil
}

// narrowToChanges drops packages whose issues the diff filter would hide anyway,
// so `new`, `new-from-rev`, `new-from-merge-base` and `new-from-patch` analyze only
// what changed. Dropped packages are still loaded as dependencies when needed.
func (cl *ContextBuilder) narrowToChanges(log logutils.Log, pkgs, deduplicatedPkgs []*packages.Package) (
	narrowedPkgs, narrowedDeduplicatedPkgs []*packages.Package,
) {
	if diffNarrowingDisabled() {
		return pkgs, deduplicatedPkgs
	}

	checker, err := processors.PrepareDiff(&cl.cfg.Issues)
	if err != nil || checker == nil {
		return pkgs, deduplicatedPkgs
	}

	wd, err := os.Getwd()
	if err != nil {
		return pkgs, deduplicatedPkgs
	}

	files := checker.ChangedFiles()

	keptDeduplicated, ok := changedPackages(deduplicatedPkgs, files, wd)
	if !ok {
		log.Infof("Analyzing all packages: module files changed")
		return pkgs, deduplicatedPkgs
	}

	keptOriginal, _ := changedPackages(pkgs, files, wd)

	log.Infof("Analyzing %d of %d packages affected by %d changed files", len(keptDeduplicated), len(deduplicatedPkgs), len(files))

	return keptOriginal, keptDeduplicated
}

func (cl *ContextBuilder) recordPackageLoad(started time.Time, original, deduplicated int, err error) {
	if cl.lifecycle == nil {
		return
	}

	cl.lifecycle.RecordPackageLoad(time.Since(started), original, deduplicated, err)
}
