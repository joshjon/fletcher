//go:build !linux

// fletcher-guest is the Firecracker microVM init and only builds for Linux.
// This stub lets `go build ./...` succeed on the macOS dev box.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "fletcher-guest runs only inside a Linux microVM")
	os.Exit(1)
}
