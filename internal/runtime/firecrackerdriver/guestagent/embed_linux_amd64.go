//go:build linux && amd64

package guestagent

import "embed"

// assetsFS carries the amd64 fletcher-guest binary. The committed about.txt
// keeps this directive compiling before `make build` produces the binary.
//
//go:embed assets/amd64
var assetsFS embed.FS

const assetDir = "assets/amd64"
