// Package buildinfo holds version and build metadata injected at link time.
package buildinfo

// Version, Commit, and Date are set via -ldflags at build time. Defaults
// indicate an unstamped local build.
var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

// Information about the current build.
type Information struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
}

// Info returns the current build information.
func Info() Information {
	return Information{
		Version: Version,
		Commit:  Commit,
		Date:    Date,
	}
}
