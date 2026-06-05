//go:build !linux || (!amd64 && !arm64)

package guestagent

import "embed"

// assetsFS is empty where there is no Firecracker build; Available() is false.
var assetsFS embed.FS

const assetDir = ""
