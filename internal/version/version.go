// Package version holds the build metadata the linker stamps into a binary.
package version

import "fmt"

// Version, Commit and Date are set at link time by `make build`. A binary
// built any other way reports the defaults.
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// String is the one line `lectiod -version` prints.
func String() string {
	return fmt.Sprintf("lectiod %s (%s, %s)", Version, Commit, Date)
}
