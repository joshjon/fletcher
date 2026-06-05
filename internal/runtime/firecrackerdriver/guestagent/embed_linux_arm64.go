//go:build linux && arm64

package guestagent

import "embed"

// assetsFS carries the arm64 fletcher-guest binary. See embed_linux_amd64.go.
//
//go:embed assets/arm64
var assetsFS embed.FS

const assetDir = "assets/arm64"
