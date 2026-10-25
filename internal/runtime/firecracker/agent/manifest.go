package agent

// Published manifest: the content-addressed file
// list of one build, downloaded at index.manifestRef and verified against
// index.artifactDigest before any artifact is pulled.

import (
	"fmt"

	"fast-sandbox/internal/artifacts"
	"fast-sandbox/internal/runtime/firecracker/imagecache"
)

// manifest adds transport-specific helpers to the shared metadata model.
type manifest artifacts.VMManifest

func parseManifest(payload []byte) (manifest, error) {
	document, err := artifacts.DecodeVMManifest(payload)
	if err != nil {
		return manifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	if len(document.Files) == 0 {
		return manifest{}, fmt.Errorf("manifest has no native files; OCI artifacts must be prepared through fsb-vm")
	}
	return manifest(document), nil
}

// nativeFile pairs a published artifact with its local cache name and the
// expected digest.
type nativeFile struct {
	publish   string
	cache     string
	sha256    string
	sizeBytes int64
}

// manifestVMStateName and manifestMemoryName are the published artifact
// names of the guest memory snapshots; they double as the local cache names
// and are shared with the publish list.
const (
	manifestVMStateName = "vmstate.snap"
	manifestMemoryName  = "memory.snap"
)

// nativeFiles validates that the manifest carries the complete native
// artifact set and returns the mapping with their expected digests.
func (m manifest) nativeFiles() ([]nativeFile, error) {
	native, err := imagecache.NativeFiles(m.Files)
	if err != nil {
		return nil, err
	}
	files := make([]nativeFile, 0, len(native))
	for _, file := range native {
		files = append(files, nativeFile{
			publish: file.Published, cache: file.Cached,
			sha256: file.SHA256, sizeBytes: file.SizeBytes,
		})
	}
	return files, nil
}

// vaultFile returns the optional vault bundle entry of a checkpoint set.
// An absent entry is (file, false, nil) — golden images never carry one; a
// present-but-malformed entry fails the pull, matching nativeFiles(): the
// committed manifest lists the file, so silently skipping it would strand
// the checkpoint's vault state.
func (m manifest) vaultFile() (nativeFile, bool, error) {
	return m.bundleFile(publishVaultName)
}

// policyFile returns the optional recorded-policy bundle entry of a
// snapshot set (checkpoint or template). Same contract as vaultFile().
func (m manifest) policyFile() (nativeFile, bool, error) {
	return m.bundleFile(publishPolicyName)
}

// bundleFile resolves one optional state bundle entry by published name.
func (m manifest) bundleFile(name string) (nativeFile, bool, error) {
	if m.Platform == nil {
		return nativeFile{}, false, nil
	}
	entry, ok := m.Platform.StateFiles[name]
	if !ok {
		return nativeFile{}, false, nil
	}
	if !validSHA256(entry.SHA256) {
		return nativeFile{}, false, fmt.Errorf("manifest artifact %s has an invalid sha256", name)
	}
	return nativeFile{
		publish: name, cache: name,
		sha256: entry.SHA256, sizeBytes: entry.SizeBytes,
	}, true, nil
}

// validSHA256 reports whether sum is a lowercase sha256 hex digest.
func validSHA256(sum string) bool {
	return imagecache.ValidKey(sum)
}
