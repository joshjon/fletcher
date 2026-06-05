//go:build !linux || (!amd64 && !arm64)

package vmm

import "embed"

// assetsFS is intentionally empty: there is no Firecracker build for this
// platform/arch, so Available() reports false and Extract returns ErrNotBundled.
// The daemon falls back to the runc or mock runtime accordingly.
var assetsFS embed.FS

const assetDir = ""
