package v1alpha2

import (
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestGPUProfileHashKeepsCPUCompatibility(t *testing.T) {
	profile := NewLegacySandboxResourceProfile(resource.MustParse("1"), resource.MustParse("1Gi"), 128)
	require.Equal(t, "653b07c8ce4443538d905f9a5fe3289ddcf1af81911ac2fd2c98a1d755420ec2", profile.Hash(), "historical CPU-only hash from main@4fe01ba")
	original := profile.Hash()
	profile.GPUs = 1
	require.NotEqual(t, original, profile.Hash())
	profile.GPUs = 0
	require.Equal(t, original, profile.Hash())
	profile.GPUs = -1
	require.Error(t, ValidateSandboxResourceProfile(profile))
	aware := NewResourceAwareSandboxResourceProfile(resource.MustParse("1"), resource.MustParse("1Gi"), resource.MustParse("2"), resource.MustParse("2"), 128)
	require.Equal(t, "54e18a490abe98acede0aa58f66272eaf0f44e9137f84580d0a3cb9669809f44", aware.Hash(), "historical ResourceAware hash from main@4fe01ba")
	original = aware.Hash()
	aware.GPUs = 1
	require.NotEqual(t, original, aware.Hash())
	aware.GPUs = 0
	require.Equal(t, original, aware.Hash())
}
