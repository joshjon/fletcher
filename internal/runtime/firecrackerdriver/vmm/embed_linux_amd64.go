//go:build linux && amd64

package vmm

import "embed"

// assetsFS carries the amd64 Firecracker binary and guest kernel. The about.txt
// committed alongside guarantees this embed directive compiles even before
// `make fetch-vmm` materialises the (gitignored) binaries.
//
//go:embed assets/amd64
var assetsFS embed.FS

const assetDir = "assets/amd64"
