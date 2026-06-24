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
