package imagecache

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"

	"fast-sandbox/internal/artifacts"
	agentprotocol "fast-sandbox/internal/runtime/firecracker/agent/protocol"
)

// File is the producer's digest and logical size of one artifact.
type File = artifacts.ManifestFile

// NativeFile maps an artifact name to its immutable local cache name.
type NativeFile struct {
	Published string
	Cached    string
	File
}

// NativeFiles is the one native artifact mapping used by pull and prewarm.
// The root drive changes name locally; snapshot names remain unchanged.
func NativeFiles(files map[string]File) ([]NativeFile, error) {
	var result []NativeFile
	for _, name := range [][2]string{
		{"rootfs.ext4", "rootfs.img"},
		{"vmstate.snap", "vmstate.snap"},
		{"memory.snap", "memory.snap"},
	} {
		entry, ok := files[name[0]]
		if !ok {
			return nil, fmt.Errorf("manifest is missing the %s artifact", name[0])
		}
		if !ValidKey(entry.SHA256) {
			return nil, fmt.Errorf("manifest artifact %s has an invalid sha256", name[0])
		}
		if entry.SizeBytes < 0 {
			return nil, fmt.Errorf("manifest artifact %s has a negative size", name[0])
		}
		result = append(result, NativeFile{Published: name[0], Cached: name[1], File: entry})
	}
	return result, nil
}

// RestoreFiles includes platform state listed by the shared manifest. Native
// prewarm must retain the same vault/policy bytes that a later pull restores.
func RestoreFiles(document artifacts.VMManifest) ([]NativeFile, error) {
	files, err := NativeFiles(document.Files)
	if err != nil {
		return nil, err
	}
	if document.Platform == nil {
		return files, nil
	}
	for _, name := range []string{agentprotocol.VaultBundleName, agentprotocol.PolicyBundleName} {
		entry, ok := document.Platform.StateFiles[name]
		if !ok {
			continue
		}
		if !ValidKey(entry.SHA256) || entry.SizeBytes < 1 {
			return nil, fmt.Errorf("invalid platform state file %q", name)
		}
		files = append(files, NativeFile{Published: name, Cached: name, File: entry})
	}
	return files, nil
}

// ReadManifest reads the complete native file list without hashing artifact
// contents again. Producers hash staging, and pull verifies downloaded bytes.
func ReadManifest(dir string) ([]byte, []NativeFile, error) {
	payload, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, nil, err
	}
	document, err := artifacts.DecodeVMManifest(payload)
	if err != nil {
		return nil, nil, fmt.Errorf("decode manifest: %w", err)
	}
	files, err := RestoreFiles(document)
	return payload, files, err
}

// CheckFiles performs the cheap completeness check, using producer names in
// staging or consumer names in the cache. This detects absent/truncated files,
// not same-size bit corruption; full checksum verification remains in pull.
func CheckFiles(dir string, files []NativeFile, published bool) error {
	for _, file := range files {
		name := file.Cached
		if published {
			name = file.Published
		}
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() != file.SizeBytes {
			return fmt.Errorf("invalid cache artifact %s: expected regular file of %d bytes", name, file.SizeBytes)
		}
	}
	return nil
}

// ValidKey reports whether value is a lowercase SHA-256 hex string.
func ValidKey(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, char := range value {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return false
		}
	}
	return true
}
