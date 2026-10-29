package steamworks

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain doubles as a fake helper: Start re-execs the test binary with the
// helper command line, and this prints canned events instead of touching Steam.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == HelperCommand && os.Getenv("DZMAN_FAKE_HELPER") != "" {
		os.Exit(fakeHelper(os.Getenv("DZMAN_FAKE_HELPER")))
	}
	os.Exit(m.Run())
}

func fakeHelper(mode string) int {
	enc := NewEncoder(os.Stdout)
	if os.Getenv("SteamAppId") != AppID || os.Getenv("SteamGameId") != AppID {
		_ = enc.Encode(Event{Type: EventFatal, Error: "app id env not set"})
		return 2
	}
	switch mode {
	case "ok":
		_, _ = os.Stdout.WriteString("Setting breakpad minidump AppID = 221100\n")
		_ = enc.Encode(Event{Type: EventStart, ID: "1"})
		_ = enc.Encode(Event{Type: EventProgress, ID: "1", Downloaded: 5, Total: 10})
		_ = enc.Encode(Event{Type: EventDone, ID: "1", Folder: "/w/1"})
		_ = enc.Encode(Event{Type: EventSummary, OK: 1})
		return 0
	case "partial":
		_ = enc.Encode(Event{Type: EventError, ID: "1", Error: "stalled"})
		_ = enc.Encode(Event{Type: EventSummary, Failed: 1})
		return 1
	case "fatal":
		_ = enc.Encode(Event{Type: EventFatal, Error: ErrNoSteamClient.Error()})
		return 2
	case "hang":
		time.Sleep(time.Minute)
	case "flood":
		// Like the real helper with many items: per-item events, then on
		// SIGTERM a "cancelled" error per item. Far more than Events buffers.
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGTERM)
		for i := range floodItems {
			_ = enc.Encode(Event{Type: EventStart, ID: fmt.Sprint(i + 1)})
		}
		_ = os.WriteFile(os.Getenv("DZMAN_FAKE_READY"), nil, 0o644)
		select {
		case <-sigs:
		case <-time.After(time.Minute):
			return 3
		}
		for i := range floodItems {
			_ = enc.Encode(Event{Type: EventError, ID: fmt.Sprint(i + 1), Error: "cancelled"})
		}
		_ = enc.Encode(Event{Type: EventSummary, Failed: floodItems})
		return 1
	case "linger-stderr", "linger-events", "linger-events-summary", "linger-both":
		// A child that is not exec'd away keeps a pipe open after the
		// helper exits, like a forked steamclient process would.
		child := exec.Command("sleep", "60")
		switch mode {
		case "linger-stderr":
			child.Stderr = os.Stderr
		case "linger-both":
			child.Stdout, child.Stderr = os.Stdout, os.Stderr
		default:
			child.Stdout = os.Stdout
		}
		if err := child.Start(); err != nil {
			_ = enc.Encode(Event{Type: EventFatal, Error: err.Error()})
			return 2
		}
		_ = os.WriteFile(os.Getenv("DZMAN_FAKE_READY"), []byte(strconv.Itoa(child.Process.Pid)), 0o644)
		_ = enc.Encode(Event{Type: EventDone, ID: "1"})
		if mode != "linger-events" && mode != "linger-both" {
			_ = enc.Encode(Event{Type: EventSummary, OK: 1})
		}
		return 0
	case "panic":
		_, _ = os.Stderr.WriteString("library chatter\n")
		panic("boom")
	case "killed":
		_, _ = os.Stderr.WriteString("last words\n")
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		time.Sleep(time.Minute)
	case "redirect":
		// What RunHelper does before loading the library, then library-style
		// chatter on fd 1 that must not reach the events stream.
		events := redirectStdout()
		_, _ = syscall.Write(1, []byte(`{"type":"fatal","error":"chatter via fd 1"}`+"\n"))
		_, _ = fmt.Fprintln(os.Stdout, `{"type":"fatal","error":"chatter via os.Stdout"}`)
		cloexec := "no-cloexec"
		if f, ok := events.(*os.File); ok {
			flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), syscall.F_GETFD, 0)
			if errno == 0 && flags&syscall.FD_CLOEXEC != 0 {
				cloexec = "cloexec"
			}
		}
		enc := NewEncoder(events)
		_ = enc.Encode(Event{Type: EventDone, ID: cloexec})
		_ = enc.Encode(Event{Type: EventSummary, OK: 1})
		return 0
	}
	return 3
}

const floodItems = 200

func TestProtocolRoundTrip(t *testing.T) {
	in := []Event{
		{Type: EventStart, ID: "123", State: ItemSubscribed},
		{Type: EventProgress, ID: "123", State: ItemSubscribed | ItemDownloading, Downloaded: 1 << 40, Total: 1<<40 + 7},
		{Type: EventDone, ID: "123", State: ItemSubscribed | ItemInstalled, Folder: "/x/221100/123"},
		{Type: EventError, ID: "456", Error: "stalled: \"quoted\"\nnewline"},
		{Type: EventFatal, Error: "boom"},
		{Type: EventSummary, OK: 1, Failed: 1},
	}
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	for _, ev := range in {
		if err := enc.Encode(ev); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(buf.String(), "\n"); n != len(in) {
		t.Fatalf("%d lines for %d events", n, len(in))
	}
	// Interleave the kind of noise libsteam_api.so prints.
	noisy := "Setting breakpad minidump AppID = 221100\n" +
		"[S_API] SteamAPI_Init(): Loaded '/home/x/.steam/sdk64/steamclient.so' OK.\n" +
		"{not json\n" + `{"id":"no type"}` + "\n\n" + buf.String()
	var out []Event
	if err := Decode(strings.NewReader(noisy), func(ev Event) { out = append(out, ev) }); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Errorf("round trip:\n got %+v\nwant %+v", out, in)
	}
}

func TestDecodeSkipsOverlongLine(t *testing.T) {
	in := `{"type":"start","id":"1"}` + "\n" + strings.Repeat("x", maxLine+10) + "\n" +
		`{"type":"done","id":"1"}` + "\n" + `{"type":"summary","ok":1}`
	var got []EventType
	if err := Decode(strings.NewReader(in), func(ev Event) { got = append(got, ev.Type) }); err != nil {
		t.Fatal(err)
	}
	if want := []EventType{EventStart, EventDone, EventSummary}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestEventOmitsEmpty(t *testing.T) {
	var buf bytes.Buffer
	_ = NewEncoder(&buf).Encode(Event{Type: EventDone, ID: "9"})
	if got := strings.TrimSpace(buf.String()); got != `{"type":"done","id":"9"}` {
		t.Errorf("got %s", got)
	}
}

func TestParseIDs(t *testing.T) {
	ids, err := parseIDs([]string{"1559212036", "18446744073709551615"})
	if err != nil || !reflect.DeepEqual(ids, []uint64{1559212036, 18446744073709551615}) {
		t.Errorf("got %v %v", ids, err)
	}
	for _, bad := range [][]string{nil, {"abc"}, {"0"}, {"-1"}, {"1", "x"}} {
		if _, err := parseIDs(bad); err == nil {
			t.Errorf("parseIDs(%q) should fail", bad)
		}
	}
}

func TestHelperArgsAndEnviron(t *testing.T) {
	got := helperArgs(JobOptions{Lib: "/l.so", IDs: []string{"1", "2"}, Subscribe: true, Timeout: time.Minute, Stall: time.Second})
	want := []string{HelperCommand, "--lib", "/l.so", "--subscribe", "--timeout", "1m0s", "--stall", "1s", "--", "1", "2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("args = %q", got)
	}
	vars := helperEnviron([]string{"A=1", "SteamAppId=440", "SteamGameId=440"})
	if want := []string{"A=1", "SteamAppId=221100", "SteamGameId=221100"}; !reflect.DeepEqual(vars, want) {
		t.Errorf("env = %q", vars)
	}
}

func TestInitError(t *testing.T) {
	tests := []struct {
		res  int32
		msg  string
		want error
	}{
		{2, "", ErrNoSteamClient},
		{3, "steamclient too old", ErrVersionMismatch},
		{1, "User not logged on", ErrNotLoggedIn},
		{1, "No license for app", ErrNotOwned},
		{1, "something odd", nil},
	}
	for _, tt := range tests {
		err := initError(tt.res, tt.msg)
		if err == nil {
			t.Fatalf("initError(%d) = nil", tt.res)
		}
		if tt.want != nil && !errors.Is(err, tt.want) {
			t.Errorf("initError(%d, %q) = %v, want %v", tt.res, tt.msg, err, tt.want)
		}
		if tt.msg != "" && !strings.Contains(err.Error(), tt.msg) {
			t.Errorf("message lost: %v", err)
		}
	}
}

// ---- library discovery ----

// writeELF writes a minimal ELF header (no sections) for class/machine.
func writeELF(t *testing.T, path string, class elf.Class, machine elf.Machine) {
	t.Helper()
	var b bytes.Buffer
	ident := [16]byte{0x7f, 'E', 'L', 'F', byte(class), byte(elf.ELFDATA2LSB), byte(elf.EV_CURRENT)}
	b.Write(ident[:])
	le := binary.LittleEndian
	if class == elf.ELFCLASS64 {
		h := elf.Header64{Type: uint16(elf.ET_DYN), Machine: uint16(machine), Version: uint32(elf.EV_CURRENT), Ehsize: 64}
		copy(h.Ident[:], ident[:])
		b.Reset()
		_ = binary.Write(&b, le, h)
	} else {
		h := elf.Header32{Type: uint16(elf.ET_DYN), Machine: uint16(machine), Version: uint32(elf.EV_CURRENT), Ehsize: 52}
		copy(h.Ident[:], ident[:])
		b.Reset()
		_ = binary.Write(&b, le, h)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCheckLibraryHeader(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name    string
		class   elf.Class
		machine elf.Machine
		wantSub string
	}{
		{"32-bit", elf.ELFCLASS32, elf.EM_386, "not 64-bit"},
		{"wrong arch", elf.ELFCLASS64, elf.EM_PPC64, "built for"},
		// A right-arch header passes the header check and fails on symbols.
		{"no symbols", elf.ELFCLASS64, goarchMachine["amd64"], "symbols"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.name != "32-bit" && c.name != "wrong arch" {
				if _, ok := goarchMachine[runtime.GOARCH]; !ok {
					t.Skip("unsupported GOARCH")
				}
				c.machine = goarchMachine[runtime.GOARCH]
			}
			p := filepath.Join(dir, c.name+".so")
			writeELF(t, p, c.class, c.machine)
			_, err := checkLibrary(p)
			if err == nil || !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("err = %v, want containing %q", err, c.wantSub)
			}
		})
	}
	p := filepath.Join(dir, "text.so")
	_ = os.WriteFile(p, []byte("not elf"), 0o644)
	if _, err := checkLibrary(p); err == nil {
		t.Error("non-ELF accepted")
	}
}

func TestCheckSymbols(t *testing.T) {
	full := append([]string{symInitFlat, "SteamAPI_SteamUGC_v018", "SteamAPI_SteamUGC_v020", "SteamAPI_SteamUGC_vX"}, requiredSymbols...)
	if info, err := checkSymbols(full); err != nil || info != (libInfo{20, "SteamAPI_SteamUGC_v020", 2}) {
		t.Errorf("full = %+v, %v; want v020", info, err)
	}
	old := append([]string{symInit, "SteamAPI_SteamUGC_v014"}, requiredSymbols...)
	if info, err := checkSymbols(old); err != nil || info.ugcVersion != 14 {
		t.Errorf("old = %+v, %v; want 14", info, err)
	}
	if _, err := checkSymbols(append([]string{symInitFlat}, requiredSymbols...)); err == nil {
		t.Error("no UGC accessor accepted")
	}
	if _, err := checkSymbols(append([]string{"SteamAPI_SteamUGC_v018"}, requiredSymbols...)); err == nil {
		t.Error("no init accepted")
	}
	if _, err := checkSymbols([]string{symInitFlat, "SteamAPI_SteamUGC_v018", symShutdown}); err == nil {
		t.Error("missing required accepted")
	}
}

// TestBindUsesCheckedAccessor ties checkSymbols to bindAPI: whatever UGC
// accessor the check accepts (here one outside any probe range, unpadded)
// is the one bound, and nothing else is looked up for it.
func TestBindUsesCheckedAccessor(t *testing.T) {
	for _, accessor := range []string{"SteamAPI_SteamUGC_v099", "SteamAPI_SteamUGC_v7", "SteamAPI_SteamUGC_v021"} {
		names := append([]string{symInitFlat, accessor}, requiredSymbols...)
		info, err := checkSymbols(names)
		if err != nil {
			t.Fatalf("%s: %v", accessor, err)
		}
		exported := map[string]bool{}
		for _, n := range names {
			exported[n] = true
		}
		var ugcLookups []string
		lookup := func(name string) uintptr {
			if strings.HasPrefix(name, symUGCPrefix) {
				ugcLookups = append(ugcLookups, name)
			}
			if exported[name] {
				return 0x1000 // never called, only registered
			}
			return 0
		}
		a, err := bindAPI("/fake.so", info, lookup)
		if err != nil {
			t.Fatalf("%s: bind: %v", accessor, err)
		}
		if a.ugcAccessorFn == nil || a.ugcSymbol != accessor || !reflect.DeepEqual(ugcLookups, []string{accessor}) {
			t.Errorf("%s: bound %q, looked up %v", accessor, a.ugcSymbol, ugcLookups)
		}
	}
	// A library the check would reject for a missing accessor fails to bind too.
	if _, err := bindAPI("/fake.so", libInfo{ugcVersion: 20, ugcSymbol: "SteamAPI_SteamUGC_v020"},
		func(name string) uintptr {
			if name == "SteamAPI_SteamUGC_v020" {
				return 0
			}
			return 0x1000
		}); err == nil {
		t.Error("bind without the checked accessor succeeded")
	}
}

func TestFindLibrary(t *testing.T) {
	touch := func(t *testing.T, p string) string {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// versions holds each usable fake library's accessor count; ugcVersions
	// its newest ISteamUGC version (0 when unset). Anything else is unusable.
	versions := map[string]int{}
	ugcVersions := map[string]int{}
	check := func(p string) (libInfo, error) {
		if v, ok := versions[p]; ok {
			return libInfo{ugcCount: v, ugcVersion: ugcVersions[p]}, nil
		}
		return libInfo{}, errors.New("bad lib")
	}

	t.Run("configured wins", func(t *testing.T) {
		base := t.TempDir()
		cfgLib := touch(t, filepath.Join(base, "mine.so"))
		versions[cfgLib] = 1
		got, err := findLibrary(FindOptions{Configured: cfgLib, ConfigDir: base}, check)
		if err != nil || got != cfgLib {
			t.Errorf("got %q %v", got, err)
		}
	})
	t.Run("configured missing is an error", func(t *testing.T) {
		base := t.TempDir()
		touch(t, filepath.Join(base, LibName))
		versions[filepath.Join(base, LibName)] = 1
		if _, err := findLibrary(FindOptions{Configured: filepath.Join(base, "nope.so"), ConfigDir: base}, check); err == nil {
			t.Error("want error")
		}
	})
	t.Run("configured unusable is an error", func(t *testing.T) {
		base := t.TempDir()
		bad := touch(t, filepath.Join(base, "bad.so"))
		if _, err := findLibrary(FindOptions{Configured: bad}, check); err == nil {
			t.Error("want error")
		}
	})
	t.Run("config dir next", func(t *testing.T) {
		base := t.TempDir()
		p := touch(t, filepath.Join(base, LibName))
		versions[p] = 1
		sa := filepath.Join(base, "steamapps")
		g := touch(t, filepath.Join(sa, "common", "Game", LibName))
		versions[g] = 1
		got, err := findLibrary(FindOptions{ConfigDir: base, SteamApps: []string{sa}}, check)
		if err != nil || got != p {
			t.Errorf("got %q %v", got, err)
		}
	})
	t.Run("scan takes the first valid in sorted order", func(t *testing.T) {
		base := t.TempDir()
		sa1 := filepath.Join(base, "lib1", "steamapps")
		sa2 := filepath.Join(base, "lib2", "steamapps")
		a := touch(t, filepath.Join(sa1, "common", "A", "bin", "linux64", LibName))
		b := touch(t, filepath.Join(sa2, "common", "B", "B_Data", "Plugins", "x86_64", LibName))
		touch(t, filepath.Join(sa1, "common", "0bad", LibName)) // invalid (e.g. 32-bit), sorts first
		touch(t, filepath.Join(sa1, "common", "D", "deep", "deeper", LibName))
		versions[a], versions[b] = 1, 1
		versions[filepath.Join(sa1, "common", "D", "deep", "deeper", LibName)] = 1 // too deep, never seen
		got, err := findLibrary(FindOptions{ConfigDir: filepath.Join(base, "cfg"), SteamApps: []string{sa2, sa1}}, check)
		if err != nil || got != a {
			t.Errorf("got %q %v, want %q", got, err, a)
		}
	})
	t.Run("scan skips emulators", func(t *testing.T) {
		base := t.TempDir()
		sa := filepath.Join(base, "steamapps")
		emu := touch(t, filepath.Join(sa, "common", "A", LibName))
		real := touch(t, filepath.Join(sa, "common", "B", LibName))
		versions[emu], versions[real] = 12, 2
		got, err := findLibrary(FindOptions{ConfigDir: filepath.Join(base, "cfg"), SteamApps: []string{sa}}, check)
		if err != nil || got != real {
			t.Errorf("got %q %v, want %q", got, err, real)
		}
		// An explicitly configured copy is the user's call.
		got, err = findLibrary(FindOptions{Configured: emu}, check)
		if err != nil || got != emu {
			t.Errorf("configured: got %q %v", got, err)
		}
	})
	t.Run("scan prefers the newest accessor", func(t *testing.T) {
		base := t.TempDir()
		sa := filepath.Join(base, "steamapps")
		old := touch(t, filepath.Join(sa, "common", "A", LibName))
		newer := touch(t, filepath.Join(sa, "common", "C", LibName))
		tie := touch(t, filepath.Join(sa, "common", "D", LibName))
		emu := touch(t, filepath.Join(sa, "common", "E", LibName))
		versions[old], versions[newer], versions[tie], versions[emu] = 1, 2, 1, 12
		ugcVersions[old], ugcVersions[newer], ugcVersions[tie], ugcVersions[emu] = 14, 21, 21, 99
		got, err := findLibrary(FindOptions{ConfigDir: filepath.Join(base, "cfg"), SteamApps: []string{sa}}, check)
		if err != nil || got != newer {
			t.Errorf("got %q %v, want %q (newest non-emulator, first path on a tie)", got, err, newer)
		}
		// The config-dir copy is an explicit choice and still wins.
		cfg := touch(t, filepath.Join(base, "cfg", LibName))
		versions[cfg], ugcVersions[cfg] = 1, 10
		if got, err := findLibrary(FindOptions{ConfigDir: filepath.Join(base, "cfg"), SteamApps: []string{sa}}, check); err != nil || got != cfg {
			t.Errorf("config dir: got %q %v", got, err)
		}
	})
	t.Run("configured relative path is made absolute", func(t *testing.T) {
		base := t.TempDir()
		lib := touch(t, filepath.Join(base, "rel.so"))
		versions[lib] = 1
		t.Chdir(base)
		got, err := findLibrary(FindOptions{Configured: "rel.so"}, check)
		if err != nil || got != lib || !filepath.IsAbs(got) {
			t.Errorf("got %q %v, want %q", got, err, lib)
		}
	})
	t.Run("configured relative path is relative to the config dir", func(t *testing.T) {
		base := t.TempDir()
		lib := touch(t, filepath.Join(base, "cfg", "rel.so"))
		versions[lib] = 1
		t.Chdir(t.TempDir())
		got, err := findLibrary(FindOptions{Configured: "rel.so", ConfigDir: filepath.Join(base, "cfg")}, check)
		if err != nil || got != lib {
			t.Errorf("got %q %v, want %q", got, err, lib)
		}
	})
	t.Run("bare tilde is the home dir", func(t *testing.T) {
		t.Setenv("HOME", "/h")
		if got := expandHome("~"); got != "/h" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("scan treats game folder names literally", func(t *testing.T) {
		base := t.TempDir()
		sa := filepath.Join(base, "steamapps")
		odd := touch(t, filepath.Join(sa, "common", "Game [x]*?", "bin", LibName))
		versions[odd] = 1
		got, err := findLibrary(FindOptions{ConfigDir: filepath.Join(base, "cfg"), SteamApps: []string{sa}}, check)
		if err != nil || got != odd {
			t.Errorf("got %q %v, want %q", got, err, odd)
		}
	})
	t.Run("scan follows a symlinked game folder", func(t *testing.T) {
		base := t.TempDir()
		sa := filepath.Join(base, "steamapps")
		real := touch(t, filepath.Join(base, "elsewhere", "G", LibName))
		versions[filepath.Join(sa, "common", "G", LibName)] = 1
		if err := os.MkdirAll(filepath.Join(sa, "common"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Dir(real), filepath.Join(sa, "common", "G")); err != nil {
			t.Fatal(err)
		}
		got, err := findLibrary(FindOptions{ConfigDir: filepath.Join(base, "cfg"), SteamApps: []string{sa}}, check)
		if err != nil || got != filepath.Join(sa, "common", "G", LibName) {
			t.Errorf("got %q %v", got, err)
		}
	})
	t.Run("nothing found explains how to get one", func(t *testing.T) {
		base := t.TempDir()
		_, err := findLibrary(FindOptions{ConfigDir: base, SteamApps: []string{filepath.Join(base, "steamapps")}}, check)
		if !errors.Is(err, ErrLibraryNotFound) || !strings.Contains(err.Error(), "redistributable_bin/linux64") {
			t.Errorf("err = %v", err)
		}
	})
}

func TestLibraryFolders(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	vdf := `"libraryfolders"
{
	"0"
	{
		"path"		"` + root + `"
		"label"		""
		"apps" { "221100" "123" }
	}
	"1"
	{
		"path"		"` + other + `"
	}
	"2"
	{
		"path"		"relative/ignored"
	}
}`
	if err := os.MkdirAll(filepath.Join(root, "steamapps"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "steamapps", "libraryfolders.vdf"), []byte(vdf), 0o644); err != nil {
		t.Fatal(err)
	}
	got := LibraryFolders(root)
	want := []string{filepath.Join(root, "steamapps"), filepath.Join(other, "steamapps")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := parseLibraryFolders(`"path" "C:\\Games\\Steam"`); len(got) != 0 {
		t.Errorf("windows path kept: %q", got)
	}
}

// ---- download loop ----

// fakeUGC scripts item states over polls.
type fakeUGC struct {
	states     map[uint64][]uint32 // per poll; the last value repeats
	polls      map[uint64]int
	dl         map[uint64]uint64
	subscribed []uint64
	downloaded []uint64
	refuse     map[uint64]bool
	noSub      bool
}

func (f *fakeUGC) subscribe(id uint64) bool {
	f.subscribed = append(f.subscribed, id)
	return !f.noSub
}
func (f *fakeUGC) download(id uint64) bool {
	f.downloaded = append(f.downloaded, id)
	return !f.refuse[id]
}
func (f *fakeUGC) state(id uint64) uint32 {
	s := f.states[id]
	i := f.polls[id]
	if i >= len(s) {
		i = len(s) - 1
	}
	return s[i]
}
func (f *fakeUGC) progress(id uint64) (uint64, uint64) { return f.dl[id], 100 }
func (f *fakeUGC) installFolder(id uint64) string      { return "/ws/" + string(rune('0'+id)) }
func (f *fakeUGC) runCallbacks() {
	for id := range f.states {
		f.polls[id]++
		f.dl[id] += 10
	}
}

// fakeClock advances by each sleep.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time        { return c.t }
func (c *fakeClock) sleep(d time.Duration) { c.t = c.t.Add(d) }
func newClock() *fakeClock                 { return &fakeClock{t: time.Unix(1_000_000, 0)} }
func eventsOf(buf *bytes.Buffer) (out []Event) {
	_ = Decode(buf, func(ev Event) { out = append(out, ev) })
	return out
}

const (
	sub  = ItemSubscribed
	inst = ItemSubscribed | ItemInstalled
	dlng = ItemSubscribed | ItemDownloading
)

func TestRunInstallsAndReportsProgress(t *testing.T) {
	f := &fakeUGC{
		states: map[uint64][]uint32{1: {0, sub, dlng, dlng, inst}},
		polls:  map[uint64]int{}, dl: map[uint64]uint64{},
	}
	clk := newClock()
	var buf bytes.Buffer
	res := run(f, runConfig{ids: []uint64{1}, subscribe: true, now: clk.now, sleep: clk.sleep}, NewEncoder(&buf))
	if res != (runResult{ok: 1}) {
		t.Fatalf("res=%+v", res)
	}
	if !reflect.DeepEqual(f.subscribed, []uint64{1}) {
		t.Errorf("subscribed %v", f.subscribed)
	}
	evs := eventsOf(&buf)
	if evs[0].Type != EventStart || evs[len(evs)-1].Type != EventDone || evs[len(evs)-1].Folder != "/ws/1" {
		t.Errorf("events %+v", evs)
	}
	var sawProgress bool
	for _, ev := range evs {
		sawProgress = sawProgress || (ev.Type == EventProgress && ev.State&ItemDownloading != 0)
	}
	if !sawProgress {
		t.Errorf("no progress event: %+v", evs)
	}
}

func TestRunAlreadyInstalledWaitsToSettle(t *testing.T) {
	f := &fakeUGC{states: map[uint64][]uint32{1: {inst}}, polls: map[uint64]int{}, dl: map[uint64]uint64{}}
	clk := newClock()
	var buf bytes.Buffer
	start := clk.t
	res := run(f, runConfig{ids: []uint64{1}, subscribe: true, settle: 3 * time.Second, now: clk.now, sleep: clk.sleep}, NewEncoder(&buf))
	if res.ok != 1 {
		t.Fatalf("res=%+v", res)
	}
	if clk.t.Sub(start) < 3*time.Second {
		t.Errorf("finished after %s, before settle", clk.t.Sub(start))
	}
	if len(f.subscribed) != 0 {
		t.Error("already-subscribed item was subscribed again")
	}
}

func TestRunUpdateFinishesWhenBusyClears(t *testing.T) {
	f := &fakeUGC{
		states: map[uint64][]uint32{1: {inst, inst | ItemNeedsUpdate, inst | ItemDownloading, inst}},
		polls:  map[uint64]int{}, dl: map[uint64]uint64{},
	}
	clk := newClock()
	var buf bytes.Buffer
	start := clk.t
	res := run(f, runConfig{ids: []uint64{1}, settle: time.Hour, now: clk.now, sleep: clk.sleep}, NewEncoder(&buf))
	if res.ok != 1 || clk.t.Sub(start) > time.Minute {
		t.Errorf("res=%+v after %s", res, clk.t.Sub(start))
	}
}

// Without subscribe (Update Mods), unsubscribed items are reported skipped
// and never handed to DownloadItem; subscribed ones still update.
func TestRunSkipsUnsubscribedWithoutSubscribe(t *testing.T) {
	f := &fakeUGC{
		states: map[uint64][]uint32{1: {ItemInstalled}, 2: {inst, inst | ItemDownloading, inst}},
		polls:  map[uint64]int{}, dl: map[uint64]uint64{},
	}
	clk := newClock()
	var buf bytes.Buffer
	res := run(f, runConfig{ids: []uint64{1, 2}, settle: time.Hour, now: clk.now, sleep: clk.sleep}, NewEncoder(&buf))
	if res != (runResult{ok: 1, skipped: 1}) {
		t.Errorf("res=%+v", res)
	}
	if len(f.subscribed) != 0 || !reflect.DeepEqual(f.downloaded, []uint64{2}) {
		t.Errorf("subscribed %v downloaded %v", f.subscribed, f.downloaded)
	}
	var skipped []Event
	for _, ev := range eventsOf(&buf) {
		if ev.Type == EventSkipped {
			skipped = append(skipped, ev)
		}
	}
	if len(skipped) != 1 || skipped[0].ID != "1" || skipped[0].Error != NotSubscribedMsg {
		t.Errorf("skipped events %+v", skipped)
	}
}

func TestRunFailures(t *testing.T) {
	t.Run("refused and subscribe failure", func(t *testing.T) {
		f := &fakeUGC{
			states: map[uint64][]uint32{1: {0}, 2: {inst}},
			polls:  map[uint64]int{}, dl: map[uint64]uint64{},
			refuse: map[uint64]bool{2: true}, noSub: true,
		}
		clk := newClock()
		var buf bytes.Buffer
		res := run(f, runConfig{ids: []uint64{1, 2}, subscribe: true, now: clk.now, sleep: clk.sleep}, NewEncoder(&buf))
		if res != (runResult{failed: 2}) {
			t.Errorf("res=%+v", res)
		}
	})
	t.Run("stall", func(t *testing.T) {
		f := &stuckUGC{}
		clk := newClock()
		var buf bytes.Buffer
		res := run(f, runConfig{ids: []uint64{1}, stall: time.Minute, now: clk.now, sleep: clk.sleep}, NewEncoder(&buf))
		evs := eventsOf(&buf)
		if res != (runResult{failed: 1}) || !strings.Contains(evs[len(evs)-1].Error, "stalled") {
			t.Errorf("res=%+v evs=%+v", res, evs)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		f := &fakeUGC{states: map[uint64][]uint32{1: {dlng}}, polls: map[uint64]int{}, dl: map[uint64]uint64{}}
		clk := newClock()
		var buf bytes.Buffer
		res := run(f, runConfig{ids: []uint64{1}, timeout: time.Minute, now: clk.now, sleep: clk.sleep}, NewEncoder(&buf))
		evs := eventsOf(&buf)
		if res.failed != 1 || !strings.Contains(evs[len(evs)-1].Error, "timed out") {
			t.Errorf("res=%+v evs=%+v", res, evs)
		}
	})
	t.Run("cancel", func(t *testing.T) {
		f := &fakeUGC{states: map[uint64][]uint32{1: {dlng}}, polls: map[uint64]int{}, dl: map[uint64]uint64{}}
		clk := newClock()
		stop := make(chan struct{})
		close(stop)
		var buf bytes.Buffer
		res := run(f, runConfig{ids: []uint64{1}, stop: stop, now: clk.now, sleep: clk.sleep}, NewEncoder(&buf))
		if res.failed != 1 || len(f.downloaded) != 0 {
			t.Errorf("res=%+v downloaded=%v (cancelled before start must not download)", res, f.downloaded)
		}
	})
}

// stuckUGC accepts the download but nothing ever changes.
type stuckUGC struct{}

func (stuckUGC) subscribe(uint64) bool            { return true }
func (stuckUGC) download(uint64) bool             { return true }
func (stuckUGC) state(uint64) uint32              { return ItemSubscribed }
func (stuckUGC) progress(uint64) (uint64, uint64) { return 0, 0 }
func (stuckUGC) installFolder(uint64) string      { return "" }
func (stuckUGC) runCallbacks()                    {}

// ---- parent side ----

func startFake(t *testing.T, mode string, opts JobOptions) ([]Event, error) {
	t.Helper()
	exe, err := HelperExe()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DZMAN_FAKE_HELPER", mode)
	opts.Exe, opts.Lib = exe, "/fake.so"
	if opts.IDs == nil {
		opts.IDs = []string{"1"}
	}
	j, err := Start(opts)
	if err != nil {
		t.Fatal(err)
	}
	var evs []Event
	for ev := range j.Events {
		evs = append(evs, ev)
	}
	return evs, j.Wait()
}

func TestJobOK(t *testing.T) {
	evs, err := startFake(t, "ok", JobOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 4 || evs[2].Type != EventDone || evs[3].Type != EventSummary || evs[3].OK != 1 {
		t.Errorf("events %+v", evs)
	}
	if Active() {
		t.Error("job still active after exit")
	}
}

func TestJobPartialIsNotAnError(t *testing.T) {
	evs, err := startFake(t, "partial", JobOptions{})
	if err != nil || len(evs) != 2 || evs[0].Type != EventError {
		t.Errorf("err=%v evs=%+v", err, evs)
	}
}

func TestJobFatal(t *testing.T) {
	_, err := startFake(t, "fatal", JobOptions{})
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Errorf("err = %v", err)
	}
}

// TestJobCancelAll quits with nobody reading Events, like the TUI does: the
// helper is killed by SIGTERM's default action.
func TestJobCancelAll(t *testing.T) {
	exe, _ := os.Executable()
	t.Setenv("DZMAN_FAKE_HELPER", "hang")
	j, err := Start(JobOptions{Exe: exe, Lib: "/fake.so", IDs: []string{"1"}})
	if err != nil {
		t.Fatal(err)
	}
	if !Active() {
		t.Fatal("not active")
	}
	cancelAllWithin(t, 10*time.Second)
	if Active() {
		t.Error("still active")
	}
	if err := j.Wait(); !errors.Is(err, ErrCancelled) {
		t.Errorf("killed job: err = %v, want ErrCancelled", err)
	}
}

// TestJobCancelAllFlood is the graceful path with no consumer: the helper
// handles SIGTERM and writes far more events than Events buffers. CancelAll
// must still return.
func TestJobCancelAllFlood(t *testing.T) {
	exe, _ := os.Executable()
	ready := filepath.Join(t.TempDir(), "ready")
	t.Setenv("DZMAN_FAKE_HELPER", "flood")
	t.Setenv("DZMAN_FAKE_READY", ready)
	j, err := Start(JobOptions{Exe: exe, Lib: "/fake.so", IDs: []string{"1"}})
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake helper never got ready")
		}
	}
	cancelAllWithin(t, 10*time.Second)
	if err := j.Wait(); !errors.Is(err, ErrCancelled) {
		t.Errorf("graceful cancel (exit 1): err = %v, want ErrCancelled", err)
	}
	n := 0
	for range j.Events {
		n++
	}
	if n == 0 {
		t.Error("no events buffered")
	}
}

// A helper that handles the cancel and exits 1 still reports ErrCancelled.
func TestJobGracefulCancelIsErrCancelled(t *testing.T) {
	exe, _ := os.Executable()
	ready := filepath.Join(t.TempDir(), "ready")
	t.Setenv("DZMAN_FAKE_HELPER", "flood")
	t.Setenv("DZMAN_FAKE_READY", ready)
	j, err := Start(JobOptions{Exe: exe, Lib: "/fake.so", IDs: []string{"1"}})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for range j.Events {
		}
	}()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake helper never got ready")
		}
	}
	j.Cancel()
	if err := j.Wait(); !errors.Is(err, ErrCancelled) {
		t.Errorf("err = %v, want ErrCancelled", err)
	}
}

func cancelAllWithin(t *testing.T, limit time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		CancelAll()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(limit):
		t.Fatalf("CancelAll still blocked after %s", limit)
	}
}

// TestHelperRedirectsStdout checks RunHelper's fd 1 redirect: chatter the
// library writes to fd 1 goes to stderr, and the events fd is close-on-exec.
func TestHelperRedirectsStdout(t *testing.T) {
	evs, err := startFake(t, "redirect", JobOptions{})
	if err != nil {
		t.Fatalf("chatter reached the events stream: %v", err)
	}
	if len(evs) != 2 || evs[0].Type != EventDone || evs[0].ID != "cloexec" {
		t.Errorf("events %+v", evs)
	}
}

func TestStartNoIDs(t *testing.T) {
	if _, err := Start(JobOptions{Exe: "/bin/true", Lib: "x"}); err == nil {
		t.Error("want error")
	}
}

// startLinger runs a fake helper that leaves a sleep child holding one of
// its pipes, and kills that child when the test ends.
func startLinger(t *testing.T, mode string) ([]Event, time.Duration, error) {
	t.Helper()
	ready := filepath.Join(t.TempDir(), "child")
	t.Setenv("DZMAN_FAKE_READY", ready)
	t.Cleanup(func() {
		if data, err := os.ReadFile(ready); err == nil {
			if pid, err := strconv.Atoi(string(data)); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	start := time.Now()
	evs, err := startFake(t, mode, JobOptions{})
	took := time.Since(start)
	if _, statErr := os.Stat(ready); statErr != nil {
		t.Fatalf("fake helper never started its child (err=%v)", err)
	}
	return evs, took, err
}

// A child holding stderr must not delay the job or turn success into an
// error (exec's WaitDelay would otherwise wait 15s and return ErrWaitDelay).
func TestJobLingeringStderr(t *testing.T) {
	evs, took, err := startLinger(t, "linger-stderr")
	if err != nil {
		t.Errorf("err = %v, want success", err)
	}
	if took > exitGrace+3*time.Second {
		t.Errorf("job took %s", took)
	}
	if len(evs) != 2 || evs[1].Type != EventSummary {
		t.Errorf("events %+v", evs)
	}
	if Active() {
		t.Error("job still active")
	}
}

// A child holding the events pipe: the summary ends the job at once.
func TestJobLingeringEventsAfterSummary(t *testing.T) {
	evs, took, err := startLinger(t, "linger-events-summary")
	if err != nil || len(evs) != 2 {
		t.Errorf("err=%v evs=%+v", err, evs)
	}
	if took > exitGrace+time.Second {
		t.Errorf("job took %s", took)
	}
}

// Without a summary the job ends exitGrace after the helper exits.
func TestJobLingeringEventsNoSummary(t *testing.T) {
	evs, took, err := startLinger(t, "linger-events")
	if err != nil || len(evs) != 1 || evs[0].Type != EventDone {
		t.Errorf("err=%v evs=%+v", err, evs)
	}
	if took > exitGrace+3*time.Second {
		t.Errorf("job took %s", took)
	}
	if Active() {
		t.Error("job still active")
	}
}

// Both pipes held and no summary: still one grace period, not a hang.
func TestJobLingeringBothPipes(t *testing.T) {
	evs, took, err := startLinger(t, "linger-both")
	if err != nil || len(evs) != 1 {
		t.Errorf("err=%v evs=%+v", err, evs)
	}
	if took > exitGrace+3*time.Second {
		t.Errorf("job took %s", took)
	}
}

func TestJobCrash(t *testing.T) {
	t.Run("panic", func(t *testing.T) {
		_, err := startFake(t, "panic", JobOptions{})
		if err == nil || !strings.Contains(err.Error(), "crashed (panic: boom)") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("signal", func(t *testing.T) {
		_, err := startFake(t, "killed", JobOptions{})
		if err == nil || err.Error() != "steamworks helper crashed (SIGKILL); its last output: last words" {
			t.Errorf("err = %v", err)
		}
	})
}

func TestStderrLogCrashLine(t *testing.T) {
	l := &stderrLog{max: 64}
	for _, chunk := range []string{"chatter\npan", "ic: boo", "m\n\ngoroutine 1 [running]:\n", "panic: second\n"} {
		_, _ = l.Write([]byte(chunk))
	}
	if got := l.crashLine(); got != "panic: boom" {
		t.Errorf("crashLine = %q", got)
	}
}

func TestProcOwnedBy(t *testing.T) {
	pid := os.Getpid()
	if !procOwnedBy(pid, uint32(os.Getuid())) {
		t.Error("own process not owned by own uid")
	}
	if procOwnedBy(pid, uint32(os.Getuid())+1) {
		t.Error("own process owned by another uid")
	}
	if procOwnedBy(1<<30, uint32(os.Getuid())) {
		t.Error("missing process owned")
	}
}

func TestIsHelperArgv(t *testing.T) {
	self, _ := os.Executable()
	for _, tc := range []struct {
		argv []string
		want bool
	}{
		{[]string{"/usr/bin/dzman", HelperCommand, "--lib", "x"}, true},
		{[]string{self, HelperCommand}, true},
		{[]string{"dzman"}, false},
		{[]string{"/usr/bin/dzman", "--lib", HelperCommand}, false},
		{[]string{"vim", HelperCommand, "--lib"}, false},
		{[]string{"bash", "-c", "dzman " + HelperCommand + " --lib x"}, false},
	} {
		if got := isHelperArgv(tc.argv); got != tc.want {
			t.Errorf("isHelperArgv(%q) = %v, want %v", tc.argv, got, tc.want)
		}
	}
}

// Steam reports the item installed but never subscribed: not done, and the
// stall says the subscription did not take.
func TestRunSubscriptionDidNotTake(t *testing.T) {
	f := &fakeUGC{states: map[uint64][]uint32{1: {0, ItemInstalled}}, polls: map[uint64]int{}, dl: map[uint64]uint64{}}
	f.dl = nil // progress stays constant so the stall fires
	clk := newClock()
	var buf bytes.Buffer
	res := run(&constProgress{f}, runConfig{ids: []uint64{1}, subscribe: true, stall: time.Minute, now: clk.now, sleep: clk.sleep}, NewEncoder(&buf))
	evs := eventsOf(&buf)
	if res != (runResult{failed: 1}) || !strings.HasPrefix(evs[len(evs)-1].Error, SubscriptionFailedMsg) {
		t.Errorf("res=%+v evs=%+v", res, evs)
	}

	// Once the subscription shows up, the item is done.
	f = &fakeUGC{states: map[uint64][]uint32{1: {0, ItemInstalled, inst}}, polls: map[uint64]int{}, dl: map[uint64]uint64{}}
	buf.Reset()
	res = run(f, runConfig{ids: []uint64{1}, subscribe: true, stall: time.Minute, now: clk.now, sleep: clk.sleep}, NewEncoder(&buf))
	if res != (runResult{ok: 1}) {
		t.Errorf("res=%+v", res)
	}
}

// An item we subscribed to that never shows the subscribed flag or any
// download activity (deleted, private or hidden) fails at the subscribe
// wait, long before the stall. Activity keeps it going.
func TestRunSubscribeWait(t *testing.T) {
	f := &fakeUGC{states: map[uint64][]uint32{1: {0}}, polls: map[uint64]int{}, dl: map[uint64]uint64{}}
	clk := newClock()
	start := clk.t
	var buf bytes.Buffer
	res := run(&constProgress{f}, runConfig{ids: []uint64{1}, subscribe: true, subscribeWait: 10 * time.Second,
		stall: time.Hour, now: clk.now, sleep: clk.sleep}, NewEncoder(&buf))
	evs := eventsOf(&buf)
	last := evs[len(evs)-1]
	if res != (runResult{failed: 1}) || !strings.HasPrefix(last.Error, SubscriptionFailedMsg) ||
		!strings.Contains(last.Error, "removed, private or hidden") {
		t.Errorf("res=%+v evs=%+v", res, evs)
	}
	if took := clk.t.Sub(start); took < 10*time.Second || took > 11*time.Second {
		t.Errorf("failed after %s, want the 10s subscribe wait", took)
	}

	// Download activity without the flag yet: not failed by the wait.
	f = &fakeUGC{states: map[uint64][]uint32{1: {0, ItemDownloadPending}}, polls: map[uint64]int{}, dl: map[uint64]uint64{}}
	clk = newClock()
	buf.Reset()
	res = run(&constProgress{f}, runConfig{ids: []uint64{1}, subscribe: true, subscribeWait: 10 * time.Second,
		stall: time.Minute, now: clk.now, sleep: clk.sleep}, NewEncoder(&buf))
	evs = eventsOf(&buf)
	if res.failed != 1 || !strings.Contains(evs[len(evs)-1].Error, "stalled") {
		t.Errorf("busy item: res=%+v evs=%+v", res, evs)
	}

	// An item that was already subscribed is never subject to it.
	clk = newClock()
	buf.Reset()
	res = run(stuckUGC{}, runConfig{ids: []uint64{1}, subscribe: true, subscribeWait: time.Second,
		stall: time.Minute, now: clk.now, sleep: clk.sleep}, NewEncoder(&buf))
	evs = eventsOf(&buf)
	if res.failed != 1 || !strings.Contains(evs[len(evs)-1].Error, "stalled") {
		t.Errorf("subscribed item: res=%+v evs=%+v", res, evs)
	}
}

func TestParseHelperArgs(t *testing.T) {
	cfg, lib, err := parseHelperArgs([]string{"--lib", "/x/libsteam_api.so", "--subscribe", "--subscribe-wait", "3s", "--", "7"})
	if err != nil || lib != "/x/libsteam_api.so" || !cfg.subscribe || cfg.subscribeWait != 3*time.Second ||
		!reflect.DeepEqual(cfg.ids, []uint64{7}) {
		t.Errorf("cfg=%+v lib=%q err=%v", cfg, lib, err)
	}
	for _, rel := range []string{"libsteam_api.so", "./libsteam_api.so", "lib/libsteam_api.so"} {
		if _, _, err := parseHelperArgs([]string{"--lib", rel, "7"}); err == nil || !strings.Contains(err.Error(), "absolute") {
			t.Errorf("--lib %s: err = %v", rel, err)
		}
	}
}

func TestTimeoutCapped(t *testing.T) {
	args := helperArgs(JobOptions{Lib: "/l.so", IDs: []string{"1"}, Timeout: time.Duration(1<<63 - 1)})
	if !slices.Contains(args, MaxTimeout.String()) {
		t.Errorf("args = %q, want the timeout capped at %s", args, MaxTimeout)
	}
	c := runConfig{timeout: time.Duration(1<<63 - 1)}
	c.fill()
	if c.timeout != MaxTimeout {
		t.Errorf("run timeout = %s", c.timeout)
	}
}

func TestExitErrorTimedOut(t *testing.T) {
	j := &Job{stderr: &stderrLog{max: 64}}
	if err := j.exitError(true, nil, nil); err == nil || err.Error() != "steamworks helper timed out" {
		t.Errorf("exited on its own: %v", err)
	}
	killed := exec.Command("sh", "-c", "kill -9 $$")
	waitErr := killed.Run()
	if err := j.exitError(true, waitErr, nil); err == nil || !strings.Contains(err.Error(), "was killed") {
		t.Errorf("killed: %v (wait %v)", err, waitErr)
	}
	exited := exec.Command("sh", "-c", "exit 1")
	waitErr = exited.Run()
	if err := j.exitError(true, waitErr, nil); err == nil || strings.Contains(err.Error(), "killed") {
		t.Errorf("exit 1 after SIGTERM: %v", err)
	}
}

// constProgress is a fakeUGC whose download counter never moves.
type constProgress struct{ *fakeUGC }

func (constProgress) progress(uint64) (uint64, uint64) { return 0, 0 }
func (c constProgress) runCallbacks() {
	for id := range c.states {
		c.polls[id]++
	}
}
