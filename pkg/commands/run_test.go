package commands

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/golangci/golangci-lint/v2/pkg/config"
)

func TestComputeConfigSaltBuildTags(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{}
	cfg.Run.BuildTags = []string{"integration", "linux"}

	want, err := computeConfigSalt(cfg)
	require.NoError(t, err)

	cfg.Run.BuildTags = []string{"linux", "integration"}
	got, err := computeConfigSalt(cfg)
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.Equal(t, []string{"linux", "integration"}, cfg.Run.BuildTags)

	cfg.Run.BuildTags = []string{"linux"}
	different, err := computeConfigSalt(cfg)
	require.NoError(t, err)
	require.NotEqual(t, want, different)
}
