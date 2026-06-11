//go:build linux

package ext4driver_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
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

func TestNewCreatesMissingRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "snapshots") // does not exist yet
	if _, err := ext4driver.New(ext4driver.Options{RootDir: root}); err != nil {
		t.Fatalf("New: %v", err)
	}
	fi, err := os.Stat(root)
	if err != nil || !fi.IsDir() {
		t.Fatalf("New did not create the root dir: %v", err)
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

func TestCommitTemplateRoundTrip(t *testing.T) {
	root := t.TempDir()
	writeTemplate(t, root, "base", bytes.Repeat([]byte("base-block"), 4096))

	d, err := ext4driver.New(ext4driver.Options{RootDir: root})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	snap, err := d.Create(ctx, "base")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Mutate the fork (a session doing work), then commit it as a template.
	mutated := bytes.Repeat([]byte("work-block"), 4096)
	if err := os.WriteFile(snap.Path, mutated, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := d.CommitTemplate(ctx, snap.ID, "webapp", false, nil); err != nil {
		t.Fatalf("CommitTemplate: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(root, "images", "webapp.ext4"))
	if err != nil {
		t.Fatalf("read committed template: %v", err)
	}
	if !bytes.Equal(got, mutated) {
		t.Fatalf("committed template mismatch: got %d bytes, want %d", len(got), len(mutated))
	}

	// A clone of the committed template boots the mutated content.
	clone, err := d.Create(ctx, "webapp")
	if err != nil {
		t.Fatalf("Create from committed template: %v", err)
	}
	got, err = os.ReadFile(clone.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, mutated) {
		t.Fatal("clone of committed template content mismatch")
	}

	// Without force, committing over an existing template is refused; with
	// force it is replaced.
	if err := d.CommitTemplate(ctx, snap.ID, "webapp", false, nil); err == nil {
		t.Fatal("expected conflict committing over an existing template")
	}
	if err := d.CommitTemplate(ctx, snap.ID, "webapp", true, nil); err != nil {
		t.Fatalf("force commit: %v", err)
	}
}

func TestCommitTemplateValidatesNames(t *testing.T) {
	root := t.TempDir()
	writeTemplate(t, root, "base", []byte("x"))
	d, err := ext4driver.New(ext4driver.Options{RootDir: root})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	snap, err := d.Create(ctx, "base")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", ".", "..", "a/b", "../escape"} {
		if err := d.CommitTemplate(ctx, snap.ID, bad, false, nil); err == nil {
			t.Fatalf("expected invalid name error for %q", bad)
		}
	}
	if err := d.CommitTemplate(ctx, "../sneaky", "ok", false, nil); err == nil {
		t.Fatal("expected invalid snapshot id error")
	}
	if err := d.CommitTemplate(ctx, "missing-snap", "ok", false, nil); err == nil {
		t.Fatal("expected missing snapshot error")
	}
}

// TestCommitTemplateInjectsFiles builds a real (tiny) ext4 image and verifies a
// commit can inject a file into it offline via e2fsck + debugfs. Skipped when
// the e2fsprogs tools are not installed.
func TestCommitTemplateInjectsFiles(t *testing.T) {
	for _, tool := range []string{"mkfs.ext4", "debugfs", "e2fsck"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}

	root := t.TempDir()
	imagesDir := filepath.Join(root, "images")
	if err := os.MkdirAll(imagesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	staging := t.TempDir()
	if err := os.MkdirAll(filepath.Join(staging, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	img := filepath.Join(imagesDir, "base.ext4")
	if err := os.Truncate(img, 0); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if out, err := exec.Command("truncate", "-s", "8M", img).CombinedOutput(); err != nil {
		t.Fatalf("truncate: %v: %s", err, out)
	}
	if out, err := exec.Command("mkfs.ext4", "-F", "-q", "-d", staging, img).CombinedOutput(); err != nil {
		t.Fatalf("mkfs.ext4: %v: %s", err, out)
	}

	d, err := ext4driver.New(ext4driver.Options{RootDir: root})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	snap, err := d.Create(ctx, "base")
	if err != nil {
		t.Fatal(err)
	}

	want := []byte(`{"entrypoint":["node","server.js"]}`)
	err = d.CommitTemplate(ctx, snap.ID, "withspec", false, map[string][]byte{
		"/etc/fletcher/app.json": want,
	})
	if err != nil {
		t.Fatalf("CommitTemplate with files: %v", err)
	}

	out, err := exec.Command("debugfs", "-R", "cat /etc/fletcher/app.json",
		filepath.Join(imagesDir, "withspec.ext4")).Output()
	if err != nil {
		t.Fatalf("read back injected file: %v", err)
	}
	if !bytes.Equal(out, want) {
		t.Fatalf("injected content mismatch: got %q", out)
	}
}
