package agent

// Published manifest: the content-addressed file
// list of one build, downloaded at index.manifestRef and verified against
// index.artifactDigest before any artifact is pulled.

import (
	"encoding/json"
	"fmt"

	"fast-sandbox/internal/runtime/firecracker/imagecache"
)

// manifestFile describes one published artifact.
type manifestFile = imagecache.File

// manifest is the consumer-side view of manifest.json; only the file list
// is needed by the pull layer.
type manifest struct {
	Files map[string]manifestFile `json:"files"`
}

// parseManifest decodes a downloaded manifest document.
func parseManifest(payload []byte) (manifest, error) {
	var document manifest
	if err := json.Unmarshal(payload, &document); err != nil {
		return manifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	if len(document.Files) == 0 {
		return manifest{}, fmt.Errorf("manifest has no files")
	}
	return document, nil
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

// validSHA256 reports whether sum is a lowercase sha256 hex digest.
func validSHA256(sum string) bool {
	return imagecache.ValidKey(sum)
}
