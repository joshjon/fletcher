package image

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
)

// Template is an imported rootfs template the daemon can boot, with whatever
// sidecar metadata was recorded at import.
type Template struct {
	// Name is what jobs/sessions reference via --image.
	Name string
	// Format is "ext4" (a flattened image file) or "subvolume" (a btrfs dir).
	Format string
	// Source is the registry ref it was imported from, empty for older imports.
	Source string
	// Digest is the image digest ("sha256:..."), empty for local-only images.
	Digest string
	// ImportedAt is when it was imported (Unix seconds; 0 if unknown).
	ImportedAt int64
	// ExposedPort is the image's lowest EXPOSE (0 if it declares none).
	ExposedPort int
	// Entrypoint is the image's effective launch command (ENTRYPOINT + CMD).
	Entrypoint []string
}

// ListTemplates lists the imported templates under imagesDir (ext4 files and
// btrfs subvolume dirs), each enriched with its sidecar metadata. A missing
// images dir yields an empty list, not an error. Results are sorted by name.
func ListTemplates(imagesDir string) ([]Template, error) {
	entries, err := os.ReadDir(imagesDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read images dir: %w", err)
	}
	out := make([]Template, 0, len(entries))
	for _, e := range entries {
		var name, format string
		switch {
		case e.IsDir():
			name, format = e.Name(), "subvolume"
		case strings.HasSuffix(e.Name(), ".ext4"):
			name, format = strings.TrimSuffix(e.Name(), ".ext4"), "ext4"
		default:
			continue // sidecar .meta.json and anything else
		}
		t := Template{Name: name, Format: format}
		if meta, found, _ := ReadMeta(imagesDir, name); found {
			t.Source = meta.Source
			t.Digest = meta.Digest
			t.ImportedAt = meta.ImportedAt
			t.ExposedPort = meta.ExposedPort
			t.Entrypoint = meta.Entrypoint
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
