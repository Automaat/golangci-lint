package commands

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/golangci/golangci-lint/v2/pkg/exitcodes"
	"github.com/golangci/golangci-lint/v2/pkg/lint/lifecycle"
	"github.com/golangci/golangci-lint/v2/pkg/logutils"
)

func TestRunLifecycleReportSetupFailures(t *testing.T) {
	testCases := []struct {
		name string
		args []string
	}{
		{name: "flags", args: []string{"--does-not-exist"}},
		{name: "config", args: []string{"--config", filepath.Join(t.TempDir(), "missing.yml")}},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			reportPath := filepath.Join(t.TempDir(), "lifecycle.json")
			t.Setenv(lifecycle.EnvReportPath, reportPath)

			command := newRunCommand(logutils.NewStderrLog(logutils.DebugKeyEmpty), BuildInfo{})
			command.cmd.SetArgs(testCase.args)
			require.Error(t, command.cmd.Execute())

			data, err := os.ReadFile(reportPath)
			require.NoError(t, err)

			var report lifecycle.Report
			require.NoError(t, json.Unmarshal(data, &report))
			require.NotNil(t, report.Outcome)
			assert.Equal(t, exitcodes.Failure, report.Outcome.ExitCode)
			assert.NotEmpty(t, report.Outcome.Error)
		})
	}
}
