// Package version exposes build and version metadata. The string variables
// are meant to be overridden at build time with -ldflags, e.g.:
//
//	go build -ldflags "\
//	  -X github.com/m0rthis/dzman/internal/version.Version=v1.2.3 \
//	  -X github.com/m0rthis/dzman/internal/version.Revision=abc1234 \
//	  -X github.com/m0rthis/dzman/internal/version.Reference=refs/tags/v1.2.3 \
//	  -X github.com/m0rthis/dzman/internal/version.BuiltAt=2026-07-09T12:00:00Z" \
//	  ./cmd
//
// When not injected (plain `go build`/`go install`), Get falls back to the
// VCS stamps the Go toolchain embeds in the binary.
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Build-time overridable variables.
var (
	// Name is the application name.
	Name = "dzman"
	// Version is the semantic version or ref name ("dev" when not injected).
	Version = "dev"
	// Revision is the short commit SHA.
	Revision = "unknown"
	// Reference is the full git ref that produced the build.
	Reference = "unknown"
	// BuiltAt is the build timestamp (RFC3339).
	BuiltAt = "unknown"
)

// Info is an immutable snapshot of version and build metadata.
type Info struct {
	Name      string
	Version   string
	Revision  string
	Reference string
	GoVersion string
	BuiltAt   string
	OS        string
	Arch      string
}

// Get returns the current build info, enriching un-injected fields from the
// binary's embedded VCS data when available.
func Get() Info {
	rev, built := Revision, BuiltAt
	if rev == "unknown" || built == "unknown" {
		if bi, ok := debug.ReadBuildInfo(); ok {
			for _, s := range bi.Settings {
				switch s.Key {
				case "vcs.revision":
					if rev == "unknown" && s.Value != "" {
						rev = shortSHA(s.Value)
					}
				case "vcs.time":
					if built == "unknown" && s.Value != "" {
						built = s.Value
					}
				case "vcs.modified":
					if s.Value == "true" {
						rev += "-dirty"
					}
				}
			}
		}
	}
	return Info{
		Name:      Name,
		Version:   Version,
		Revision:  rev,
		Reference: Reference,
		GoVersion: runtime.Version(),
		BuiltAt:   built,
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
	}
}

// String renders the info as an aligned, human-readable block.
func (i Info) String() string {
	return fmt.Sprintf(
		"%s %s\n"+
			"  Revision:   %s\n"+
			"  Reference:  %s\n"+
			"  Go version: %s\n"+
			"  Built at:   %s\n"+
			"  Platform:   %s/%s\n",
		i.Name, i.Version, i.Revision, i.Reference,
		i.GoVersion, i.BuiltAt, i.OS, i.Arch,
	)
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
