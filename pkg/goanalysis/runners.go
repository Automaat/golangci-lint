package goanalysis

import (
	"fmt"
	"go/token"
	"slices"
	"strings"
	"time"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/packages"

	"github.com/golangci/golangci-lint/v2/pkg/goanalysis/pkgerrors"
	"github.com/golangci/golangci-lint/v2/pkg/lint/lifecycle"
	"github.com/golangci/golangci-lint/v2/pkg/lint/linter"
	"github.com/golangci/golangci-lint/v2/pkg/logutils"
	"github.com/golangci/golangci-lint/v2/pkg/result"
	"github.com/golangci/golangci-lint/v2/pkg/timeutils"
)

type runAnalyzersConfig interface {
	getName() string
	getLinterNames() []string
	getLinterNameForAnalyzer(*analysis.Analyzer) string
	getLinterNameForDiagnostic(*Diagnostic) string
	getAnalyzers() []*analysis.Analyzer
	useOriginalPackages() bool
	reportIssues(*linter.Context) []*Issue
	getLoadMode() LoadMode
}

type analysisLifecycle struct {
	recorder      *lifecycle.Recorder
	cfg           runAnalyzersConfig
	started       time.Time
	report        *lifecycle.AnalysisRun
	linterIndexes map[string]int
}

func runAnalyzers(cfg runAnalyzersConfig, lintCtx *linter.Context) (retIssues []*result.Issue, retErr error) {
	log := lintCtx.Log.Child(logutils.DebugKeyGoAnalysis)
	sw := timeutils.NewStopwatch("analyzers", log)

	const stagesToPrint = 10
	defer sw.PrintTopStages(stagesToPrint)

	runner := newRunner(cfg.getName(), log, lintCtx.PkgCache, lintCtx.LoadGuard,
		cfg.getLoadMode(), sw, lintCtx.Lifecycle != nil)

	pkgs := lintCtx.Packages
	if cfg.useOriginalPackages() {
		pkgs = lintCtx.OriginalPackages
	}

	issues, pkgsFromCache := loadIssuesFromCache(pkgs, lintCtx, cfg.getAnalyzers())
	var pkgsToAnalyze []*packages.Package
	for _, pkg := range pkgs {
		if !pkgsFromCache[pkg] {
			pkgsToAnalyze = append(pkgsToAnalyze, pkg)
		}
	}

	metrics := newAnalysisLifecycle(cfg, lintCtx.Lifecycle, len(pkgs), len(pkgsToAnalyze))
	var statsReady func(analysisStats)
	if metrics != nil {
		statsReady = metrics.recordStats
		defer func() { metrics.finishRecovered(retIssues, retErr, recover()) }()
	}

	diags, errs, passToPkg := runner.run(cfg.getAnalyzers(), pkgsToAnalyze, statsReady)

	defer func() {
		if len(errs) == 0 {
			// If we try to save to cache even if we have compilation errors
			// we won't see them on repeated runs.
			saveIssuesToCache(pkgs, pkgsFromCache, issues, lintCtx, cfg.getAnalyzers())
		}
	}()

	buildAllIssues := func() []*result.Issue {
		var retIssues []*result.Issue

		reportedIssues := cfg.reportIssues(lintCtx)
		for _, reportedIssue := range reportedIssues {
			if reportedIssue.Pkg == nil {
				reportedIssue.Pkg = passToPkg[reportedIssue.Pass]
			}

			retIssues = append(retIssues, reportedIssue.Issue)
		}

		return slices.Concat(retIssues, buildIssues(diags, cfg.getLinterNameForDiagnostic))
	}

	errIssues, err := pkgerrors.BuildIssuesFromIllTypedError(errs, lintCtx)
	if err != nil {
		return nil, err
	}

	issues = append(issues, errIssues...)
	issues = append(issues, buildAllIssues()...)

	return issues, nil
}

func newAnalysisLifecycle(cfg runAnalyzersConfig, recorder *lifecycle.Recorder,
	requestedPackages, analyzedPackages int,
) *analysisLifecycle {
	if recorder == nil {
		return nil
	}

	metrics := &analysisLifecycle{
		recorder: recorder,
		cfg:      cfg,
		started:  time.Now(),
		report: &lifecycle.AnalysisRun{
			Name:              cfg.getName(),
			RequestedPkgs:     requestedPackages,
			CachedPkgs:        requestedPackages - analyzedPackages,
			AnalyzedPkgs:      analyzedPackages,
			ConfiguredLinters: []lifecycle.ConfiguredLinterRun{},
			Analyzers:         []lifecycle.AnalyzerRun{},
		},
		linterIndexes: map[string]int{},
	}

	linterNames := slices.Clone(cfg.getLinterNames())
	slices.Sort(linterNames)
	for _, name := range linterNames {
		metrics.linterIndexes[name] = len(metrics.report.ConfiguredLinters)
		metrics.report.ConfiguredLinters = append(metrics.report.ConfiguredLinters,
			lifecycle.ConfiguredLinterRun{Name: name})
	}

	return metrics
}

func (m *analysisLifecycle) recordStats(stats analysisStats) {
	if m == nil {
		return
	}

	m.report.InitialPkgs = stats.initialPackages
	m.report.TotalPkgs = stats.totalPackages
	m.report.Actions = stats.actions
	m.report.Parallelism = stats.parallelism
	for _, analyzer := range stats.analyzers {
		linterName := m.cfg.getLinterNameForAnalyzer(analyzer.analyzer)
		m.report.Analyzers = append(m.report.Analyzers, lifecycle.AnalyzerRun{
			Name:            analyzer.analyzer.Name,
			Linter:          linterName,
			ElapsedNS:       analyzer.elapsed.Nanoseconds(),
			Actions:         analyzer.actions,
			ExecutedActions: analyzer.executedActions,
			Diagnostics:     analyzer.diagnostics,
			Errors:          analyzer.errors,
		})
		if index, ok := m.linterIndexes[linterName]; ok {
			m.report.ConfiguredLinters[index].ElapsedNS += analyzer.elapsed.Nanoseconds()
			m.report.ConfiguredLinters[index].Actions += analyzer.actions
			m.report.ConfiguredLinters[index].Errors += analyzer.errors
		}
	}
}

func (m *analysisLifecycle) finish(issues []*result.Issue, err error) {
	m.report.ElapsedNS = time.Since(m.started).Nanoseconds()
	if err != nil {
		m.report.Error = err.Error()
	}
	for _, issue := range issues {
		if index, ok := m.linterIndexes[issue.FromLinter]; ok {
			m.report.ConfiguredLinters[index].Issues++
		}
	}
	m.recorder.RecordAnalysis(m.report)
}

func (m *analysisLifecycle) finishRecovered(issues []*result.Issue, runErr error, panicValue any) {
	if panicValue == nil {
		m.finish(issues, runErr)
		return
	}

	panicErr, ok := panicValue.(error)
	if !ok {
		panicErr = fmt.Errorf("panic: %v", panicValue)
	}
	m.finish(issues, panicErr)
	panic(panicValue)
}

func buildIssues(diags []*Diagnostic, linterNameBuilder func(diag *Diagnostic) string) []*result.Issue {
	var issues []*result.Issue

	for _, diag := range diags {
		linterName := linterNameBuilder(diag)

		var text string
		if diag.Analyzer.Name == linterName {
			text = diag.Message
		} else {
			text = fmt.Sprintf("%s: %s", diag.Analyzer.Name, diag.Message)
		}

		var suggestedFixes []analysis.SuggestedFix

		for _, sf := range diag.SuggestedFixes {
			// Skip suggested fixes on cgo files.
			// The related error is: "diff has out-of-bounds edits"
			// This is a temporary workaround.
			if !strings.HasSuffix(diag.File.Name(), ".go") {
				continue
			}

			nsf := analysis.SuggestedFix{Message: sf.Message}

			for _, edit := range sf.TextEdits {
				end := edit.End

				if !end.IsValid() {
					end = edit.Pos
				}

				// To be applied the positions need to be "adjusted" based on the file.
				// This is the difference between the "displayed" positions and "effective" positions.
				nsf.TextEdits = append(nsf.TextEdits, analysis.TextEdit{
					Pos:     token.Pos(diag.File.Offset(edit.Pos)),
					End:     token.Pos(diag.File.Offset(end)),
					NewText: edit.NewText,
				})
			}

			suggestedFixes = append(suggestedFixes, nsf)
		}

		issues = append(issues, &result.Issue{
			FromLinter:     linterName,
			Text:           text,
			Pos:            diag.Position,
			Pkg:            diag.Pkg,
			SuggestedFixes: suggestedFixes,
		})

		if len(diag.Related) > 0 {
			for _, info := range diag.Related {
				relatedPos := diag.Pkg.Fset.Position(info.Pos)

				if relatedPos.Filename != diag.Position.Filename {
					relatedPos = diag.Position
				}

				issues = append(issues, &result.Issue{
					FromLinter: linterName,
					Text:       fmt.Sprintf("%s(related information): %s", diag.Analyzer.Name, info.Message),
					Pos:        relatedPos,
					Pkg:        diag.Pkg,
				})
			}
		}
	}
	return issues
}
