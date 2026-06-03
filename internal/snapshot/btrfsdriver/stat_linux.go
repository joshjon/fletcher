//go:build linux

package btrfsdriver

import "os"

func osStat(p string) (os.FileInfo, error) { return os.Stat(p) }
