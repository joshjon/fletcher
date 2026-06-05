//go:build linux && arm64

package vmm

import "embed"

// assetsFS carries the arm64 Firecracker binary and guest kernel. See
// embed_linux_amd64.go for the about.txt rationale.
//
//go:embed assets/arm64
var assetsFS embed.FS

const assetDir = "assets/arm64"
