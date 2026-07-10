package launcher

import (
	"strings"
	"testing"
)

func TestToSteamArgs(t *testing.T) {
	a := ConnectArgs{
		ServerIP:  "1.2.3.4",
		GamePort:  2302,
		Password:  "secret",
		ModIDs:    []string{"111", "222"},
		ExtraArgs: []string{"-nosplash", "-high"},
	}
	args := a.ToSteamArgs()
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"-mod=@111;@222",
		"-connect=1.2.3.4",
		"-port=2302",
		"-password=secret",
		"-nosplash",
		"-high",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %q", want, joined)
		}
	}

	// -mod must precede -connect (mods load before connection).
	modIdx := indexOf(args, "-mod=@111;@222")
	connIdx := indexOf(args, "-connect=1.2.3.4")
	if modIdx < 0 || connIdx < 0 || modIdx > connIdx {
		t.Errorf("-mod should come before -connect: %v", args)
	}
}

func TestToSteamArgsNoModsNoPassword(t *testing.T) {
	a := ConnectArgs{ServerIP: "1.2.3.4", GamePort: 2302}
	joined := strings.Join(a.ToSteamArgs(), " ")
	if strings.Contains(joined, "-mod=") {
		t.Errorf("unexpected -mod in %q", joined)
	}
	if strings.Contains(joined, "-password=") {
		t.Errorf("unexpected -password in %q", joined)
	}
}

func TestToSteamArgsLaunchOnly(t *testing.T) {
	// No server IP: just extra args, no connect/port.
	a := ConnectArgs{ExtraArgs: []string{"-nosplash"}}
	joined := strings.Join(a.ToSteamArgs(), " ")
	if strings.Contains(joined, "-connect") {
		t.Errorf("unexpected -connect in %q", joined)
	}
	if !strings.Contains(joined, "-nosplash") {
		t.Errorf("missing -nosplash in %q", joined)
	}
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}
