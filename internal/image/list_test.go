package image

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestListTemplates(t *testing.T) {
	dir := t.TempDir()
	// foo: an ext4 template with sidecar metadata.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "foo.ext4"), []byte("x"), 0o644))
	require.NoError(t, WriteMeta(dir, "foo", TemplateMeta{
		Source:      "ghcr.io/x/foo:v1",
		Digest:      "sha256:abc",
		Format:      "ext4",
		ImportedAt:  100,
		Entrypoint:  []string{"/app", "serve"},
		ExposedPort: 8080,
	}))
	// bar: an ext4 template with no sidecar (imported before metadata).
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bar.ext4"), []byte("x"), 0o644))
	// a stray file that is neither a template nor a sidecar.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o644))

	got, err := ListTemplates(dir)
	require.NoError(t, err)
	require.Len(t, got, 2, "two templates; the .meta.json sidecar and stray file are skipped")

	// Sorted by name: bar, foo.
	require.Equal(t, "bar", got[0].Name)
	require.Equal(t, "ext4", got[0].Format)
	require.Empty(t, got[0].Source, "no sidecar")

	require.Equal(t, "foo", got[1].Name)
	require.Equal(t, "ghcr.io/x/foo:v1", got[1].Source)
	require.Equal(t, []string{"/app", "serve"}, got[1].Entrypoint)
	require.Equal(t, 8080, got[1].ExposedPort)
}

func TestListTemplatesMissingDir(t *testing.T) {
	got, err := ListTemplates(filepath.Join(t.TempDir(), "does-not-exist"))
	require.NoError(t, err)
	require.Empty(t, got)
}
