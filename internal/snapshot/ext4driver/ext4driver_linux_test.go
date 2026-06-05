//go:build linux

package ext4driver_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/joshjon/fletcher/internal/snapshot/ext4driver"
)

func writeTemplate(t *testing.T, root, name string, content []byte) {
	t.Helper()
	imagesDir := filepath.Join(root, "images")
	if err := os.MkdirAll(imagesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(imagesDir, name+".ext4"), content, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCreateClonesTemplate(t *testing.T) {
	root := t.TempDir()
	content := bytes.Repeat([]byte("rootfs-block"), 8192)
	writeTemplate(t, root, "base", content)

	d, err := ext4driver.New(ext4driver.Options{RootDir: root})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := d.Create(context.Background(), "base")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := os.ReadFile(snap.Path)
	if err != nil {
		t.Fatalf("read clone: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("clone content mismatch: got %d bytes, want %d", len(got), len(content))
	}

	// The clone must be independent of the template: writing to it must not
	// change the template (true for both the reflink and the copy path).
	if err := os.WriteFile(snap.Path, []byte("scribble"), 0o640); err != nil {
		t.Fatal(err)
	}
	tmpl, _ := os.ReadFile(filepath.Join(root, "images", "base.ext4"))
	if !bytes.Equal(tmpl, content) {
		t.Fatal("writing to the clone mutated the template")
	}

	if err := d.Delete(context.Background(), snap.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(snap.Path); !os.IsNotExist(err) {
		t.Fatalf("clone still present after Delete: %v", err)
	}
	// Delete of a missing snapshot is a no-op.
	if err := d.Delete(context.Background(), snap.ID); err != nil {
		t.Fatalf("Delete of missing snapshot should be a no-op, got: %v", err)
	}
}

func TestCreateMissingTemplate(t *testing.T) {
	d, err := ext4driver.New(ext4driver.Options{RootDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Create(context.Background(), "nope"); err == nil {
		t.Fatal("expected error for missing template")
	}
}

func TestCreateEmptyImageRejected(t *testing.T) {
	d, err := ext4driver.New(ext4driver.Options{RootDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Create(context.Background(), ""); err == nil {
		t.Fatal("expected error for empty image (Firecracker needs a rootfs)")
	}
}

func TestCreateRejectsPathTraversal(t *testing.T) {
	d, err := ext4driver.New(ext4driver.Options{RootDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../escape", "sub/dir", "..", "."} {
		if _, err := d.Create(context.Background(), name); err == nil {
			t.Errorf("expected rejection of image name %q", name)
		}
	}
}
