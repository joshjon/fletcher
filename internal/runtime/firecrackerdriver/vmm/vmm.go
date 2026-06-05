// Package vmm bundles the Firecracker VMM binary and a guest kernel via
// embed.FS and extracts them to a cache directory on first run, so the single
// fletcher binary is self-sufficient on a fresh KVM host with no separate VMM
// install step. Per the thesis: "VMM bundled via embed.FS, extracted on first
// run."
//
// The embedded assets are architecture-specific, selected by build constraints
// in embed_linux_amd64.go / embed_linux_arm64.go. On platforms without a
// Firecracker build (macOS dev, unsupported arch) embed_other.go provides an
// empty set and Extract returns ErrNotBundled.
//
// The actual binaries are gitignored and fetched by `make fetch-vmm`; a fresh
// checkout still compiles (the asset directory carries a committed about.txt),
// but Extract reports ErrNotBundled until the assets are fetched and the binary
// rebuilt. This keeps multi-megabyte blobs out of git while preserving the
// single-binary shipping model for releases.
package vmm

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
)

// ErrNotBundled is returned by Extract when this build does not carry the VMM
// assets: a fresh checkout built without `make fetch-vmm`, or a non-Linux /
// unsupported-arch build.
var ErrNotBundled = errors.New(
	"firecracker VMM assets are not bundled in this build " +
		"(run `make fetch-vmm` and rebuild, or use a release binary)",
)

// Bundle is the set of extracted, ready-to-use VMM file paths.
type Bundle struct {
	// FirecrackerPath is the extracted firecracker binary (executable).
	FirecrackerPath string
	// KernelPath is the extracted guest kernel (vmlinux).
	KernelPath string
}

// Asset names inside the embedded FS and on disk after extraction.
const (
	firecrackerName = "firecracker"
	kernelName      = "vmlinux"
)

// Available reports whether this build carries the VMM assets. Used by the
// daemon and doctor to tell "Firecracker unsupported in this build" apart from
// "Firecracker failed to start".
func Available() bool {
	return assetPresent(firecrackerName) && assetPresent(kernelName)
}

func assetPresent(name string) bool {
	if assetDir == "" {
		return false
	}
	f, err := assetsFS.Open(path.Join(assetDir, name))
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// Extract writes the bundled VMM files into dir (created if needed) and returns
// their paths. It is idempotent: a file already present with the expected size
// and mode is left untouched, so repeated daemon starts do not rewrite tens of
// megabytes each boot. Returns ErrNotBundled when this build has no assets.
func Extract(dir string) (Bundle, error) {
	if !Available() {
		return Bundle{}, ErrNotBundled
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return Bundle{}, fmt.Errorf("vmm: create cache dir: %w", err)
	}
	fcPath := filepath.Join(dir, firecrackerName)
	if err := extractFile(firecrackerName, fcPath, 0o755); err != nil {
		return Bundle{}, err
	}
	kernelPath := filepath.Join(dir, kernelName)
	if err := extractFile(kernelName, kernelPath, 0o644); err != nil {
		return Bundle{}, err
	}
	return Bundle{FirecrackerPath: fcPath, KernelPath: kernelPath}, nil
}

// extractFile materialises one embedded asset at dest with the given mode,
// skipping the write when dest already matches (size + perms). The write is
// atomic via a temp file + rename so a crash mid-extract can't leave a
// truncated binary that later boots fail on opaquely.
func extractFile(name, dest string, mode os.FileMode) error {
	data, err := assetsFS.ReadFile(path.Join(assetDir, name))
	if err != nil {
		return fmt.Errorf("vmm: read embedded %s: %w", name, err)
	}
	if fi, statErr := os.Stat(dest); statErr == nil &&
		fi.Size() == int64(len(data)) && fi.Mode().Perm() == mode {
		return nil
	}
	tmp := dest + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return fmt.Errorf("vmm: write %s: %w", name, err)
	}
	// WriteFile honours the umask, so the exec bit may have been masked off;
	// force the intended mode before publishing.
	if err := os.Chmod(tmp, mode); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("vmm: chmod %s: %w", name, err)
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("vmm: install %s: %w", name, err)
	}
	return nil
}
