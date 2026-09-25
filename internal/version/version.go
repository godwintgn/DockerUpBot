// Package version holds build-time identity for DockerUpBot.
// Values are overridden via -ldflags at image build time.
package version

// Semantic version for releases (keep in sync with the VERSION file / git tags).
var (
	Version   = "0.1.0"
	Commit    = "none"
	BuildDate = "unknown"
)

// String returns a human-readable version line.
func String() string {
	return "dockerupbot " + Version + " (" + Commit + " " + BuildDate + ")"
}

// Short returns version without build metadata.
func Short() string {
	return Version
}
