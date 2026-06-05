//go:build !linux

package doctor

import "os"

// deviceGID cannot inspect device ownership off Linux; the KVM check skips
// before reaching it on those platforms.
func deviceGID(os.FileInfo) (uint32, bool) { return 0, false }
