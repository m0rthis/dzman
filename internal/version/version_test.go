package version

import (
	"runtime"
	"strings"
	"testing"
)

func TestGetReflectsVars(t *testing.T) {
	// Save and restore the package vars around the test.
	origV, origR := Version, Revision
	defer func() { Version, Revision = origV, origR }()

	Version = "v9.9.9"
	Revision = "cafebabe"

	info := Get()
	if info.Version != "v9.9.9" {
		t.Errorf("Version = %q, want v9.9.9", info.Version)
	}
	if info.Revision != "cafebabe" {
		t.Errorf("Revision = %q, want cafebabe", info.Revision)
	}
	if info.GoVersion != runtime.Version() {
		t.Errorf("GoVersion = %q, want %q", info.GoVersion, runtime.Version())
	}
	if info.OS != runtime.GOOS || info.Arch != runtime.GOARCH {
		t.Errorf("platform = %s/%s, want %s/%s", info.OS, info.Arch, runtime.GOOS, runtime.GOARCH)
	}
}

func TestStringContainsFields(t *testing.T) {
	info := Info{
		Name: "dzman", Version: "v1.0.0", Revision: "abc123",
		Reference: "refs/tags/v1.0.0", GoVersion: "go1.26", BuiltAt: "2026-01-01T00:00:00Z",
		OS: "linux", Arch: "amd64",
	}
	s := info.String()
	for _, want := range []string{"dzman v1.0.0", "abc123", "refs/tags/v1.0.0", "linux/amd64"} {
		if !strings.Contains(s, want) {
			t.Errorf("String() missing %q:\n%s", want, s)
		}
	}
}

func TestShortSHA(t *testing.T) {
	if got := shortSHA("0123456789abcdef"); got != "0123456789ab" {
		t.Errorf("shortSHA long = %q, want 0123456789ab", got)
	}
	if got := shortSHA("abc"); got != "abc" {
		t.Errorf("shortSHA short = %q, want abc", got)
	}
}
