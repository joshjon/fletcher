// Package image describes imported base-image rootfs templates: the sidecar
// metadata recorded at import (where the template came from, and the registry
// digest it was built from) and the registry check that tells whether a newer
// version is available. The CLI writes the metadata; the daemon reads it to
// surface an "update available" hint.
package image

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// MetaSuffix is appended to a template name for its sidecar metadata file
// (e.g. fletcher-base.meta.json). It is neither a directory (subvolume) nor a
// ".ext4" file, so `image ls` skips it.
const MetaSuffix = ".meta.json"

// TemplateMeta records where an imported template came from, so it can be
// updated later and checked against the registry for a newer version.
type TemplateMeta struct {
	// Source is the docker reference the template was imported from
	// (e.g. "ghcr.io/joshjon/fletcher-base:debian-13").
	Source string `json:"source"`
	// Digest is the registry image digest the template was built from
	// ("sha256:..."), or empty for a local-only image with no registry digest.
	Digest string `json:"digest"`
	// Format is "ext4" or "subvolume".
	Format string `json:"format"`
	// ImportedAt is when the template was imported (Unix seconds).
	ImportedAt int64 `json:"imported_at"`
}

// MetaPath is the sidecar metadata path for a template.
func MetaPath(imagesDir, name string) string {
	return filepath.Join(imagesDir, name+MetaSuffix)
}

// WriteMeta writes a template's metadata at mode 0644 so the daemon (a
// different user) can read it.
func WriteMeta(imagesDir, name string, m TemplateMeta) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal image metadata: %w", err)
	}
	if err := os.WriteFile(MetaPath(imagesDir, name), data, 0o644); err != nil { //nolint:gosec // metadata is non-secret and read by the daemon user
		return fmt.Errorf("write image metadata: %w", err)
	}
	return nil
}

// ReadMeta reads a template's metadata. found is false (with no error) when the
// template has no sidecar - e.g. imported before metadata was recorded.
func ReadMeta(imagesDir, name string) (meta TemplateMeta, found bool, err error) {
	data, rerr := os.ReadFile(MetaPath(imagesDir, name))
	if os.IsNotExist(rerr) {
		return TemplateMeta{}, false, nil
	}
	if rerr != nil {
		return TemplateMeta{}, false, fmt.Errorf("read image metadata: %w", rerr)
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return TemplateMeta{}, false, fmt.Errorf("parse image metadata: %w", err)
	}
	return meta, true, nil
}

// RemoveMeta deletes a template's sidecar metadata, ignoring its absence.
func RemoveMeta(imagesDir, name string) error {
	if err := os.Remove(MetaPath(imagesDir, name)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
