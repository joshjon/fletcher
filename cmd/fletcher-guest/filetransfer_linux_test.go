//go:build linux

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/joshjon/fletcher/internal/runtime/firecrackerdriver/guestproto"
)

// TestWriteUploadRoundTrip drives writeUpload over a pipe the way the host
// would: read the readiness ack, stream the bytes, read the final result, then
// check the file landed with the right content and hash.
func TestWriteUploadRoundTrip(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "sub", "uploaded.bin")
	payload := bytes.Repeat([]byte{0x00, 0x7f, 0xff, 'x'}, 5000) // 20 KB, binary

	host, guest := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		writeUpload(guest, guestproto.FileSpec{Path: dest, Mode: 0o640, Size: int64(len(payload))})
		_ = guest.Close()
	}()

	ack, err := guestproto.ReadFileResult(host)
	if err != nil {
		t.Fatalf("read ack: %v", err)
	}
	if ack.Error != "" {
		t.Fatalf("unexpected ack error: %s", ack.Error)
	}
	if _, err := host.Write(payload); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	res, err := guestproto.ReadFileResult(host)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	if res.Error != "" {
		t.Fatalf("write failed: %s", res.Error)
	}
	<-done

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("content mismatch: got %d bytes, want %d", len(got), len(payload))
	}
	sum := sha256.Sum256(payload)
	if res.Sha256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("sha256 mismatch: got %s", res.Sha256)
	}
	if res.BytesWritten != int64(len(payload)) {
		t.Fatalf("bytes_written = %d, want %d", res.BytesWritten, len(payload))
	}
	if fi, _ := os.Stat(dest); fi != nil && fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %o, want 0640", fi.Mode().Perm())
	}
}

// TestWriteUploadNoClobber checks that an upload to an existing path is refused
// at the readiness ack unless overwrite is set, and replaces when it is.
func TestWriteUploadNoClobber(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "exists.txt")
	if err := os.WriteFile(dest, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}

	// overwrite=false: the ack should carry an "already exists" error and the
	// file is untouched.
	host, guest := net.Pipe()
	go func() {
		writeUpload(guest, guestproto.FileSpec{Path: dest, Size: 3})
		_ = guest.Close()
	}()
	ack, err := guestproto.ReadFileResult(host)
	if err != nil {
		t.Fatalf("read ack: %v", err)
	}
	if ack.Error == "" {
		t.Fatalf("expected an 'already exists' error without overwrite")
	}
	_ = host.Close()
	if got, _ := os.ReadFile(dest); string(got) != "original" {
		t.Fatalf("file changed despite refused upload: %q", got)
	}

	// overwrite=true: replaces the file.
	payload := []byte("NEW")
	host2, guest2 := net.Pipe()
	go func() {
		writeUpload(guest2, guestproto.FileSpec{Path: dest, Size: int64(len(payload)), Overwrite: true})
		_ = guest2.Close()
	}()
	if ack2, aerr := guestproto.ReadFileResult(host2); aerr != nil || ack2.Error != "" {
		t.Fatalf("overwrite ack: err=%v ack=%+v", aerr, ack2)
	}
	if _, werr := host2.Write(payload); werr != nil {
		t.Fatalf("write payload: %v", werr)
	}
	if res, rerr := guestproto.ReadFileResult(host2); rerr != nil || res.Error != "" {
		t.Fatalf("overwrite result: err=%v res=%+v", rerr, res)
	}
	if got, _ := os.ReadFile(dest); string(got) != "NEW" {
		t.Fatalf("file not replaced: %q", got)
	}
}

// TestReadDownloadRoundTrip writes a file, then drives readDownload and checks
// the host receives the right header and bytes.
func TestReadDownloadRoundTrip(t *testing.T) {
	src := filepath.Join(t.TempDir(), "data.bin")
	payload := bytes.Repeat([]byte{1, 2, 3, 0, 255}, 3000) // 15 KB
	if err := os.WriteFile(src, payload, 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	host, guest := net.Pipe()
	go func() {
		readDownload(guest, guestproto.FileSpec{Path: src})
		_ = guest.Close()
	}()

	hdr, err := guestproto.ReadFileResult(host)
	if err != nil {
		t.Fatalf("read header: %v", err)
	}
	if hdr.Error != "" {
		t.Fatalf("unexpected header error: %s", hdr.Error)
	}
	if hdr.Size != int64(len(payload)) {
		t.Fatalf("size = %d, want %d", hdr.Size, len(payload))
	}
	got := make([]byte, hdr.Size)
	if _, err := io.ReadFull(host, got); err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("content mismatch")
	}
}

// TestListDir lists a directory and checks ordering (dirs first), entry fields,
// and a symlink's resolution.
func TestListDir(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "bfile.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "adir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("adir", filepath.Join(root, "zlink")); err != nil {
		t.Fatal(err)
	}

	host, guest := net.Pipe()
	go func() {
		listDir(guest, guestproto.FileSpec{Path: root})
		_ = guest.Close()
	}()
	listing, err := guestproto.ReadDirListing(host)
	if err != nil {
		t.Fatalf("read listing: %v", err)
	}
	if listing.Error != "" {
		t.Fatalf("unexpected error: %s", listing.Error)
	}
	if len(listing.Entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(listing.Entries))
	}
	// Directories first: "adir" and the dir-symlink "zlink" precede "bfile.txt".
	if !listing.Entries[0].IsDir || listing.Entries[0].Name != "adir" {
		t.Fatalf("first entry = %+v, want dir adir", listing.Entries[0])
	}
	var bfile, zlink *guestproto.DirEntry
	for i := range listing.Entries {
		switch listing.Entries[i].Name {
		case "bfile.txt":
			bfile = &listing.Entries[i]
		case "zlink":
			zlink = &listing.Entries[i]
		}
	}
	if bfile == nil || bfile.IsDir || bfile.Size != 5 {
		t.Fatalf("bfile entry wrong: %+v", bfile)
	}
	if zlink == nil || !zlink.IsSymlink || !zlink.IsDir || zlink.SymlinkTarget != "adir" {
		t.Fatalf("zlink entry wrong: %+v", zlink)
	}
}

// TestListDirNotADir reports a non-directory path as an error.
func TestListDirNotADir(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	host, guest := net.Pipe()
	go func() {
		listDir(guest, guestproto.FileSpec{Path: f})
		_ = guest.Close()
	}()
	listing, err := guestproto.ReadDirListing(host)
	if err != nil {
		t.Fatalf("read listing: %v", err)
	}
	if listing.Error == "" {
		t.Fatalf("expected an error for a non-directory")
	}
}

// TestFileOpDeleteMoveCopy exercises delete, move, and recursive copy via the
// pipe protocol.
func TestFileOpDeleteMoveCopy(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) string {
		p := filepath.Join(root, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	run := func(spec guestproto.FileOpSpec) guestproto.FileResult {
		host, guest := net.Pipe()
		go func() {
			fileOp(guest, spec)
			_ = guest.Close()
		}()
		res, err := guestproto.ReadFileResult(host)
		if err != nil {
			t.Fatalf("read result: %v", err)
		}
		return res
	}

	// delete a file
	f := write("a.txt", "hi")
	if res := run(guestproto.FileOpSpec{Op: guestproto.FileOpDelete, Path: f}); res.Error != "" {
		t.Fatalf("delete: %s", res.Error)
	}
	if _, err := os.Stat(f); !os.IsNotExist(err) {
		t.Fatalf("file still present after delete")
	}

	// delete a non-empty dir without recursive should fail; with recursive succeed
	write("d/inner.txt", "x")
	if res := run(guestproto.FileOpSpec{Op: guestproto.FileOpDelete, Path: filepath.Join(root, "d")}); res.Error == "" {
		t.Fatalf("non-recursive delete of a non-empty dir should fail")
	}
	if res := run(guestproto.FileOpSpec{Op: guestproto.FileOpDelete, Path: filepath.Join(root, "d"), Recursive: true}); res.Error != "" {
		t.Fatalf("recursive delete: %s", res.Error)
	}

	// move (rename)
	src := write("m.txt", "move me")
	dst := filepath.Join(root, "moved.txt")
	if res := run(guestproto.FileOpSpec{Op: guestproto.FileOpMove, Path: src, Dest: dst}); res.Error != "" {
		t.Fatalf("move: %s", res.Error)
	}
	if got, _ := os.ReadFile(dst); string(got) != "move me" {
		t.Fatalf("moved content wrong: %q", got)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("source still present after move")
	}

	// recursive copy of a directory tree
	write("tree/sub/leaf.txt", "leaf")
	copyDst := filepath.Join(root, "tree-copy")
	if res := run(guestproto.FileOpSpec{Op: guestproto.FileOpCopy, Path: filepath.Join(root, "tree"), Dest: copyDst, Recursive: true}); res.Error != "" {
		t.Fatalf("copy: %s", res.Error)
	}
	if got, _ := os.ReadFile(filepath.Join(copyDst, "sub", "leaf.txt")); string(got) != "leaf" {
		t.Fatalf("copied tree content wrong: %q", got)
	}

	// guard: refuse to delete "/"
	if res := run(guestproto.FileOpSpec{Op: guestproto.FileOpDelete, Path: "/", Recursive: true}); res.Error == "" {
		t.Fatalf("deleting / should be refused")
	}
}

// TestReadDownloadMissing reports a missing file in the header error rather than
// streaming.
func TestReadDownloadMissing(t *testing.T) {
	host, guest := net.Pipe()
	go func() {
		readDownload(guest, guestproto.FileSpec{Path: filepath.Join(t.TempDir(), "nope")})
		_ = guest.Close()
	}()
	hdr, err := guestproto.ReadFileResult(host)
	if err != nil {
		t.Fatalf("read header: %v", err)
	}
	if hdr.Error == "" {
		t.Fatalf("expected an error for a missing file")
	}
}
