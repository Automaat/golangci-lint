package processors

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/golangci/revgrep"

	"github.com/golangci/golangci-lint/v2/pkg/config"
	"github.com/golangci/golangci-lint/v2/pkg/result"
)

const envGolangciDiffProcessorPatch = "GOLANGCI_DIFF_PROCESSOR_PATCH"

var _ Processor = (*Diff)(nil)

// Diff filters issues based on options `new`, `new-from-rev`, etc.
//
// Uses `git`.
// The paths inside the patch are relative to the path where git is run (the same location where golangci-lint is run).
//
// Warning: it doesn't use `path-prefix` option.
type Diff struct {
	cfg *config.Issues
}

func NewDiff(cfg *config.Issues) *Diff {
	return &Diff{cfg: cfg}
}

func (*Diff) Name() string {
	return "diff"
}

func (p *Diff) Process(issues []*result.Issue) ([]*result.Issue, error) {
	checker, err := PrepareDiff(p.cfg)
	if err != nil {
		return nil, err
	}
	if checker == nil {
		return issues, nil
	}

	return transformIssues(issues, func(issue *result.Issue) *result.Issue {
		if issue.FromLinter == typeCheckName {
			return issue
		}

		hunkPos, isNew := checker.IsNew(issue.WorkingDirectoryRelativePath, issue.Line())
		if !isNew {
			return nil
		}

		newIssue := *issue
		newIssue.HunkPos = hunkPos

		return &newIssue
	}), nil
}

func (*Diff) Finish() {}

type preparedDiff struct {
	once    sync.Once
	checker *revgrep.Checker
	err     error
}

// preparedDiffs maps each *config.Issues to its *preparedDiff.
var preparedDiffs sync.Map

// PrepareDiff returns the changes selected by the `new`, `new-from-rev`, `new-from-merge-base`
// and `new-from-patch` options, or nil when none is set.
// The result is computed once per configuration, so package selection and issue filtering
// see the same changes.
func PrepareDiff(cfg *config.Issues) (*revgrep.Checker, error) {
	value, _ := preparedDiffs.LoadOrStore(cfg, &preparedDiff{})
	prepared := value.(*preparedDiff)

	prepared.once.Do(func() {
		prepared.checker, prepared.err = prepareDiff(cfg)
	})

	return prepared.checker, prepared.err
}

func prepareDiff(cfg *config.Issues) (*revgrep.Checker, error) {
	patch := os.Getenv(envGolangciDiffProcessorPatch)
	if !cfg.Diff && cfg.DiffFromRevision == "" && cfg.DiffFromMergeBase == "" && cfg.DiffPatchFilePath == "" && patch == "" {
		return nil, nil
	}

	var patchReader io.Reader
	switch {
	case cfg.DiffPatchFilePath != "":
		content, err := os.ReadFile(cfg.DiffPatchFilePath)
		if err != nil {
			return nil, fmt.Errorf("can't read from patch file %s: %w", cfg.DiffPatchFilePath, err)
		}

		patchReader = bytes.NewReader(content)

	case patch != "":
		patchReader = strings.NewReader(patch)
	}

	checker := &revgrep.Checker{
		Patch:        patchReader,
		RevisionFrom: cfg.DiffFromRevision,
		MergeBase:    cfg.DiffFromMergeBase,
		WholeFiles:   cfg.WholeFiles,
	}

	if err := checker.Prepare(context.Background()); err != nil {
		return nil, fmt.Errorf("can't prepare diff by revgrep: %w", err)
	}

	return checker, nil
}
