package steam

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestIsLoginFailure(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want bool
	}{
		{"cached credentials", "Logging in user 'bob' to Steam Public...\nCached credentials not found.\n", true},
		{"invalid password", "FAILED (Invalid Password)", true},
		{"failed login", "FAILED login with result code Rate Limit Exceeded", true},
		{"login failure", "Login Failure: Account Logon Denied", true},
		{"steam guard", "This computer has not been authenticated for your account using Steam Guard.", true},
		{"two factor", "Two-factor code mismatch", true},
		{"password prompt", "password: ", true},
		{"mixed case", "INVALID PASSWORD", true},
		{"success", "Success. Downloaded item 123 to \"/x\" (4 bytes)", false},
		{"other failure", "ERROR! Download item 123 failed (Failure).", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsLoginFailure([]byte(tt.out)); got != tt.want {
				t.Errorf("IsLoginFailure(%q) = %v, want %v", tt.out, got, tt.want)
			}
		})
	}
}

func TestRunError(t *testing.T) {
	base := errors.New("exit status 5")
	// The login marker is early in the output, outside the last 3 lines.
	out := "Cached credentials not found.\na\nb\nc\nd\n"
	err := runError("update game", base, []byte(out))
	if !errors.Is(err, ErrLoginRequired) {
		t.Errorf("want ErrLoginRequired, got %v", err)
	}
	if err := runError("update game", base, []byte("disk full")); errors.Is(err, ErrLoginRequired) || !errors.Is(err, base) {
		t.Errorf("non-login failure misclassified: %v", err)
	}
}

func TestLoginCmd(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	cmd, err := LoginCmd("steamcmd", "bob")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cmd.Args, " "); got != "steamcmd +login bob +quit" {
		t.Errorf("args = %q", got)
	}
	if cmd.Stdin != nil || cmd.Stdout != nil || cmd.Stderr != nil {
		t.Error("stdio must be left for ExecProcess")
	}
	home, _ := SteamCMDHomeDir()
	if got := lastHome(cmd.Env); got != home {
		t.Errorf("HOME = %q, want %q", got, home)
	}
	if fi, err := os.Stat(home); err != nil || !fi.IsDir() {
		t.Errorf("steamcmd home not created: %v", err)
	}
}

// lastHome returns the HOME value exec would use (the last one wins).
func lastHome(vars []string) string {
	home := ""
	for _, kv := range vars {
		if v, ok := strings.CutPrefix(kv, "HOME="); ok {
			home = v
		}
	}
	return home
}

func TestIsolatedEnvReplacesHome(t *testing.T) {
	got := isolatedEnv([]string{"PATH=/bin", "HOME=/home/me", "HOMEX=keep", "XDG_CONFIG_HOME=/home/me/.config", "XDG_RUNTIME_DIR=/run/user/1"}, "/iso")
	want := "PATH=/bin HOMEX=keep XDG_RUNTIME_DIR=/run/user/1 HOME=/iso XDG_DATA_HOME=/iso/.local/share " +
		"XDG_CONFIG_HOME=/iso/.config XDG_CACHE_HOME=/iso/.cache XDG_STATE_HOME=/iso/.local/state"
	if strings.Join(got, " ") != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestDownloadArgsOrder(t *testing.T) {
	args := downloadArgs("/c", "bob", "42")
	idx := func(s string) int {
		for i, a := range args {
			if a == s {
				return i
			}
		}
		return -1
	}
	fi, li, wi := idx("+force_install_dir"), idx("+login"), idx("+workshop_download_item")
	if fi < 0 || li < 0 || wi < 0 || fi >= li || li >= wi {
		t.Fatalf("bad order: %q", args)
	}
	if args[fi+1] != "/c" || args[wi+1] != GameID || args[wi+2] != "42" {
		t.Errorf("bad args: %q", args)
	}
	if args[len(args)-1] != "+quit" {
		t.Errorf("must end with +quit: %q", args)
	}
}

func TestCheckSteamCMDIsolated(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "Steam")
	if err := os.MkdirAll(filepath.Join(root, "steamcmd"), 0o755); err != nil {
		t.Fatal(err)
	}
	// ~/.steam/steam style symlink to the real root.
	link := filepath.Join(base, "steamlink")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, bin, root string
		wantErr         bool
	}{
		{"inside root", filepath.Join(root, "steamcmd", "steamcmd.sh"), root, true},
		{"inside via symlinked root", filepath.Join(root, "steamcmd.sh"), link, true},
		{"sibling with shared prefix", filepath.Join(base, "SteamCMD", "steamcmd.sh"), root, false},
		{"elsewhere", "/usr/games/steamcmd", root, false},
		{"no root known", filepath.Join(root, "steamcmd.sh"), "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckSteamCMDIsolated(tt.bin, tt.root)
			if (err != nil) != tt.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestCheckSteamCMDIsolatedAnyRoot(t *testing.T) {
	home := t.TempDir()
	// ~/.steam/root links to the real root; the detected library is elsewhere.
	real := filepath.Join(home, ".local", "share", "Steam")
	if err := os.MkdirAll(filepath.Join(home, ".steam"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(home, ".steam", "root")); err != nil {
		t.Fatal(err)
	}
	library := filepath.Join(home, "games", "SteamLibrary")
	roots := clientRoots(home, library, "")
	for _, bin := range []string{
		filepath.Join(real, "steamcmd", "steamcmd.sh"),
		filepath.Join(home, ".steam", "root", "steamcmd.sh"),
		filepath.Join(home, FlatpakSteamRoot, "steamcmd", "steamcmd.sh"),
		filepath.Join(library, "steamcmd.sh"),
	} {
		if err := CheckSteamCMDIsolated(bin, roots...); err == nil {
			t.Errorf("%s accepted", bin)
		}
	}
	if err := CheckSteamCMDIsolated(filepath.Join(home, "steamcmd", "steamcmd.sh"), roots...); err != nil {
		t.Errorf("~/steamcmd rejected: %v", err)
	}
	seen := map[string]bool{}
	for _, r := range roots {
		if r == "" || seen[r] {
			t.Errorf("roots %q has empty or duplicate entries", roots)
		}
		seen[r] = true
	}
}

func TestSteamCMDContentDir(t *testing.T) {
	home := t.TempDir()
	data := filepath.Join(home, ".local", "share", "dzman")
	flatpak := filepath.Join(home, FlatpakSteamRoot)
	if err := os.MkdirAll(flatpak, 0o755); err != nil {
		t.Fatal(err)
	}
	// A symlink into the Flatpak tree counts as Flatpak Steam.
	link := filepath.Join(home, "flatpak-steam")
	if err := os.Symlink(flatpak, link); err != nil {
		t.Fatal(err)
	}
	inFlatpak := filepath.Join(home, ".var", "app", "com.valvesoftware.Steam", "data", "dzman", "steamcmd-content")
	tests := []struct {
		name, root, want string
	}{
		{"native", filepath.Join(home, ".local", "share", "Steam"), filepath.Join(data, "steamcmd-content")},
		{"unknown root", "", filepath.Join(data, "steamcmd-content")},
		{"flatpak", flatpak, inFlatpak},
		{"flatpak via symlink", link, inFlatpak},
		{"other flatpak app", filepath.Join(home, ".var", "app", "com.valvesoftware.SteamLink"), filepath.Join(data, "steamcmd-content")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := steamCMDContentDir(home, data, isFlatpakClient(home, tt.root))
			if got != tt.want {
				t.Errorf("got %s, want %s", got, tt.want)
			}
			// Never inside the client's own steamapps.
			if tt.root != "" && isWithin(got, filepath.Join(tt.root, "steamapps")) {
				t.Errorf("%s is inside the client's steamapps", got)
			}
		})
	}
}

func TestValidModID(t *testing.T) {
	for id, want := range map[string]bool{
		"1559212036": true, "1": true, "18446744073709551615": true,
		"": false, "0": false, "-1": false, "1.5": false, "1e5": false, "+5": false,
		" 1": false, "../x": false, "18446744073709551616": false,
		"0123": false, "00": false, "10": true,
	} {
		if got := ValidModID(id); got != want {
			t.Errorf("ValidModID(%q) = %v, want %v", id, got, want)
		}
	}
}

// makeMod creates <dir>/<id>/meta.cpp, optionally with a .dzman marker.
func makeMod(t *testing.T, dir, id, name string, marked bool) {
	t.Helper()
	p := filepath.Join(dir, id)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := "name = \"" + name + "\";\npublishedid = " + id + ";\ntimestamp = 1;\n"
	if err := os.WriteFile(filepath.Join(p, "meta.cpp"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	if marked {
		if err := os.WriteFile(filepath.Join(p, ".dzman"), []byte(id), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func testPaths(t *testing.T) *Paths {
	t.Helper()
	base := t.TempDir()
	return &Paths{
		SteamApps:   filepath.Join(base, "Steam", "steamapps"),
		DayZ:        filepath.Join(base, "Steam", "steamapps", "common", "DayZ"),
		Workshop:    WorkshopDir(filepath.Join(base, "Steam")),
		CmdWorkshop: WorkshopDir(filepath.Join(base, "dzman", "steamcmd-content")),
	}
}

func TestScanAllModsAndFind(t *testing.T) {
	p := testPaths(t)
	makeMod(t, p.Workshop, "1", "Client", true) // stale marker from old dzman
	makeMod(t, p.CmdWorkshop, "1", "Dup", true)
	makeMod(t, p.CmdWorkshop, "2", "Cmd", true)
	makeMod(t, p.CmdWorkshop, "3", "CmdUnmarked", false)

	mods, err := ScanAllMods(p)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]InstalledMod{}
	for _, m := range mods {
		got[m.ID] = m
	}
	if len(mods) != 3 {
		t.Fatalf("got %d mods, want 3: %+v", len(mods), mods)
	}
	if m := got["1"]; m.Name != "Client" || m.Managed {
		t.Errorf("mod 1 = %+v, want client copy, unmanaged", m)
	}
	if !got["2"].Managed || got["3"].Managed {
		t.Errorf("managed flags wrong: %+v", got)
	}

	if d, ok := p.FindMod("1"); !ok || d != filepath.Join(p.Workshop, "1") {
		t.Errorf("FindMod(1) = %q %v", d, ok)
	}
	if d, ok := p.FindMod("2"); !ok || d != filepath.Join(p.CmdWorkshop, "2") {
		t.Errorf("FindMod(2) = %q %v", d, ok)
	}
	if _, ok := p.FindMod("9"); ok {
		t.Error("FindMod(9) should miss")
	}
}

func TestRemoveManagedModsOnlyCmdDir(t *testing.T) {
	p := testPaths(t)
	makeMod(t, p.Workshop, "1", "Client", true)
	makeMod(t, p.CmdWorkshop, "2", "Cmd", true)
	makeMod(t, p.CmdWorkshop, "3", "CmdUnmarked", false)

	n, err := RemoveManagedMods(p)
	if err != nil || n != 1 {
		t.Fatalf("RemoveManagedMods = %d, %v; want 1", n, err)
	}
	for _, keep := range []string{filepath.Join(p.Workshop, "1"), filepath.Join(p.CmdWorkshop, "3")} {
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("%s was removed", keep)
		}
	}
	if _, err := os.Stat(filepath.Join(p.CmdWorkshop, "2")); !os.IsNotExist(err) {
		t.Error("managed steamcmd mod not removed")
	}
}

func TestEnsureSymlinks(t *testing.T) {
	p := testPaths(t)
	if err := os.MkdirAll(p.DayZ, 0o755); err != nil {
		t.Fatal(err)
	}
	makeMod(t, p.Workshop, "1", "A", false)
	makeMod(t, p.CmdWorkshop, "2", "B", false)
	makeMod(t, p.CmdWorkshop, "3", "C", false)
	makeMod(t, p.Workshop, "4", "D", false)

	// @2: stale link to a source that no longer exists.
	if err := os.Symlink(filepath.Join(p.Workshop, "2"), filepath.Join(p.DayZ, "@2")); err != nil {
		t.Fatal(err)
	}
	// @3: link to a different, existing dir.
	other := filepath.Join(t.TempDir(), "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, filepath.Join(p.DayZ, "@3")); err != nil {
		t.Fatal(err)
	}
	// @4: a real directory, which must be left alone.
	if err := os.MkdirAll(filepath.Join(p.DayZ, "@4", "keep"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := EnsureSymlinks(p.DayZ, p.ModDirs(), []string{"1", "2", "3", "4"}); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{
		"1": filepath.Join(p.Workshop, "1"),
		"2": filepath.Join(p.CmdWorkshop, "2"),
		"3": filepath.Join(p.CmdWorkshop, "3"),
	} {
		got, err := os.Readlink(filepath.Join(p.DayZ, "@"+id))
		if err != nil || got != want {
			t.Errorf("@%s -> %q (%v), want %q", id, got, err, want)
		}
	}
	if fi, err := os.Lstat(filepath.Join(p.DayZ, "@4")); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Errorf("@4 real dir was touched: %v", err)
	}
	if _, err := os.Stat(filepath.Join(p.DayZ, "@4", "keep")); err != nil {
		t.Error("@4 contents lost")
	}

	// Idempotent.
	if err := EnsureSymlinks(p.DayZ, p.ModDirs(), []string{"1", "2"}); err != nil {
		t.Fatal(err)
	}
	// Missing everywhere is an error.
	if err := EnsureSymlinks(p.DayZ, p.ModDirs(), []string{"99"}); err == nil {
		t.Error("want error for missing mod")
	}
	// A malformed ID is named in the error and nothing is linked for it.
	err := EnsureSymlinks(p.DayZ, p.ModDirs(), []string{"1", "../2", "1e5"})
	if err == nil || !strings.Contains(err.Error(), "../2") || !strings.Contains(err.Error(), "1e5") {
		t.Errorf("err = %v, want both bad IDs named", err)
	}
	if _, err := os.Lstat(filepath.Join(p.DayZ, "@../2")); !os.IsNotExist(err) {
		t.Error("link made for a bad ID")
	}
}

func TestFindSteamCMD(t *testing.T) {
	write := func(t *testing.T, path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name       string
		configured func(home, bin string) string
		setup      func(t *testing.T, home, bin string) // bin is a dir placed on PATH
		want       func(home, bin string) string        // "" means error expected
		wantErr    string
	}{
		{
			name:       "configured path exists",
			configured: func(home, _ string) string { return filepath.Join(home, "custom", "sc") },
			setup:      func(t *testing.T, home, _ string) { write(t, filepath.Join(home, "custom", "sc")) },
			want:       func(home, _ string) string { return filepath.Join(home, "custom", "sc") },
		},
		{
			name:       "configured path missing",
			configured: func(home, _ string) string { return filepath.Join(home, "nope", "sc") },
			setup:      func(t *testing.T, _, bin string) { write(t, filepath.Join(bin, "steamcmd")) },
			wantErr:    "nope",
		},
		{
			name:       "configured tilde path",
			configured: func(string, string) string { return "~/x/sc" },
			setup:      func(t *testing.T, home, _ string) { write(t, filepath.Join(home, "x", "sc")) },
			want:       func(home, _ string) string { return filepath.Join(home, "x", "sc") },
		},
		{
			name:       "configured bare name on PATH",
			configured: func(string, string) string { return "mysteam" },
			setup:      func(t *testing.T, _, bin string) { write(t, filepath.Join(bin, "mysteam")) },
			want:       func(_, bin string) string { return filepath.Join(bin, "mysteam") },
		},
		{
			name:       "default finds steamcmd on PATH",
			configured: func(string, string) string { return "steamcmd" },
			setup: func(t *testing.T, home, bin string) {
				write(t, filepath.Join(bin, "steamcmd"))
				write(t, filepath.Join(home, "steamcmd", "steamcmd.sh"))
			},
			want: func(_, bin string) string { return filepath.Join(bin, "steamcmd") },
		},
		{
			name:       "empty finds steamcmd.sh on PATH only",
			configured: func(string, string) string { return "" },
			setup:      func(t *testing.T, _, bin string) { write(t, filepath.Join(bin, "steamcmd.sh")) },
			want:       func(_, bin string) string { return filepath.Join(bin, "steamcmd.sh") },
		},
		{
			name:       "home tarball location",
			configured: func(string, string) string { return "steamcmd" },
			setup:      func(t *testing.T, home, _ string) { write(t, filepath.Join(home, "steamcmd", "steamcmd.sh")) },
			want:       func(home, _ string) string { return filepath.Join(home, "steamcmd", "steamcmd.sh") },
		},
		{
			name:       "non-executable file is skipped",
			configured: func(string, string) string { return "steamcmd" },
			setup: func(t *testing.T, home, _ string) {
				p := filepath.Join(home, "steamcmd", "steamcmd.sh")
				write(t, p)
				if err := os.Chmod(p, 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: "steamcmd not found",
		},
		{
			name:       "nothing found",
			configured: func(string, string) string { return "steamcmd" },
			setup:      func(*testing.T, string, string) {},
			wantErr:    "steamcmd not found",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home, bin := t.TempDir(), t.TempDir()
			t.Setenv("PATH", bin)
			tt.setup(t, home, bin)
			got, err := findSteamCMD(tt.configured(home, bin), home, exec.LookPath, nil)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if want := tt.want(home, bin); got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}
}

func TestValidateSteamLogin(t *testing.T) {
	for _, ok := range []string{"anonymous", "some_user", "User_1"} {
		if err := ValidateSteamLogin(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "+quit", "a b", "a\tb", "a\nb", "a\x00b", " x", "User-1.x", "a\"b", "bob@x", "jörg"} {
		if err := ValidateSteamLogin(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := LoginCmd("/bin/true", "x +quit"); err == nil {
		t.Error("LoginCmd accepted a bad login")
	}
	if err := DownloadMod("/bin/true", "+x", t.TempDir(), "123"); err == nil {
		t.Error("DownloadMod accepted a bad login")
	} else if !strings.Contains(err.Error(), "letters, digits and '_'") {
		t.Errorf("error does not say what is allowed: %v", err)
	}
}

func TestValidateContentDir(t *testing.T) {
	if err := ValidateContentDir("/home/me/.local/share/dzman/steamcmd-content"); err != nil {
		t.Error(err)
	}
	for _, bad := range []string{"", "/home/my name/x", "/x\ty", "/a\"b", "/a+b", "/a\nb"} {
		if err := ValidateContentDir(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	dir := filepath.Join(t.TempDir(), "with space")
	if err := DownloadMod("/bin/true", "bob", dir, "123"); err == nil || !strings.Contains(err.Error(), "whitespace") {
		t.Errorf("DownloadMod: %v", err)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("DownloadMod created the refused dir")
	}
}

func TestFindSteamCMDSkipsNonIsolated(t *testing.T) {
	home, bin := t.TempDir(), t.TempDir()
	t.Setenv("PATH", bin)
	script := func(p string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	root := filepath.Join(home, ".steam", "steam")
	inClient := filepath.Join(root, "steamcmd", "steamcmd.sh")
	good := filepath.Join(home, "steamcmd", "steamcmd.sh")
	script(inClient)
	script(filepath.Join(bin, "steamcmd.sh"))
	if err := os.Remove(filepath.Join(bin, "steamcmd.sh")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(inClient, filepath.Join(bin, "steamcmd.sh")); err != nil {
		t.Fatal(err)
	}
	// Only a refused candidate: its refusal is the error.
	if _, err := findSteamCMD("steamcmd", home, exec.LookPath, []string{root}); err == nil ||
		!strings.Contains(err.Error(), "inside the Steam client's folder") {
		t.Fatalf("err = %v", err)
	}
	script(good)
	got, err := findSteamCMD("steamcmd", home, exec.LookPath, []string{root})
	if err != nil || got != good {
		t.Errorf("got %q, %v, want %q", got, err, good)
	}
	// An explicit path is returned as is, for the caller to refuse.
	got, err = findSteamCMD(inClient, home, exec.LookPath, []string{root})
	if err != nil || got != inClient {
		t.Errorf("explicit: got %q, %v", got, err)
	}
}

// The folder name is the mod's ID; meta.cpp's publishedid is display only.
func TestScanUsesFolderNameAsID(t *testing.T) {
	p := testPaths(t)
	write := func(folder, meta string) {
		t.Helper()
		d := filepath.Join(p.Workshop, folder)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "meta.cpp"), []byte(meta), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("111", "name = \"Moved\";\npublishedid = 999;\n")
	write("222", "name = \"Zero\";\npublishedid = 0;\n")
	write("333", "name = \"NoID\";\n")
	write("0444", "name = \"Padded\";\npublishedid = 444;\n")
	write("notanid", "name = \"Junk\";\npublishedid = 555;\n")

	mods, err := ScanAllMods(p)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]InstalledMod{}
	for _, m := range mods {
		got[m.ID] = m
	}
	if len(got) != 3 {
		t.Fatalf("got %+v, want mods 111, 222, 333", mods)
	}
	if m := got["111"]; m.Name != "Moved" || m.MetaID != "999" || m.Dir != filepath.Join(p.Workshop, "111") {
		t.Errorf("111 = %+v", m)
	}
	if m := got["222"]; m.MetaID != "0" {
		t.Errorf("222 = %+v", m)
	}
	if m := got["333"]; m.Name != "NoID" || m.MetaID != "" {
		t.Errorf("333 = %+v", m)
	}
	for _, m := range mods {
		if d, ok := p.FindMod(m.ID); !ok || d != m.Dir {
			t.Errorf("FindMod(%s) = %q %v, want %s", m.ID, d, ok, m.Dir)
		}
	}
}

func TestRemoveMarkedRefusesClientRoot(t *testing.T) {
	p := testPaths(t)
	makeMod(t, p.Workshop, "1", "Client", true)
	// A symlink to the client's workshop dir must resolve and be refused.
	link := filepath.Join(t.TempDir(), "ws")
	if err := os.Symlink(p.Workshop, link); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{p.Workshop, link} {
		if n, err := removeMarked(dir, []string{p.Root()}); err == nil || n != 0 {
			t.Errorf("removeMarked(%s) = %d, %v; want refusal", dir, n, err)
		}
	}
	if _, err := os.Stat(filepath.Join(p.Workshop, "1")); err != nil {
		t.Error("client mod removed")
	}
}

// mkTree creates dirs and files (a trailing "/" marks a dir) under base.
func mkTree(t *testing.T, base string, paths ...string) {
	t.Helper()
	for _, p := range paths {
		full := filepath.Join(base, p)
		if strings.HasSuffix(p, "/") {
			if err := os.MkdirAll(full, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFindClientRoot(t *testing.T) {
	t.Run("custom root is a secondary library", func(t *testing.T) {
		home := t.TempDir()
		mkTree(t, home, ".local/share/Steam/steam.sh", "games/steamapps/common/DayZ/")
		if err := os.MkdirAll(filepath.Join(home, ".steam"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(home, ".local/share/Steam"), filepath.Join(home, ".steam", "root")); err != nil {
			t.Fatal(err)
		}
		got := findClientRoot(home, filepath.Join(home, "games"))
		if want := resolvePath(filepath.Join(home, ".local/share/Steam")); got != want {
			t.Errorf("got %q, want the resolved ~/.steam/root %q", got, want)
		}
	})
	t.Run("custom root that is a client install wins", func(t *testing.T) {
		home := t.TempDir()
		mkTree(t, home, ".local/share/Steam/steam.sh", "opt/steam/ubuntu12_32/")
		if got, want := findClientRoot(home, filepath.Join(home, "opt/steam")), filepath.Join(home, "opt/steam"); got != resolvePath(want) {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	t.Run("order and libraryfolders marker", func(t *testing.T) {
		home := t.TempDir()
		mkTree(t, home, ".steam/steam/steamapps/libraryfolders.vdf", ".local/share/Steam/steam.sh")
		if got, want := findClientRoot(home, ""), filepath.Join(home, ".steam/steam"); got != resolvePath(want) {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	t.Run("flatpak", func(t *testing.T) {
		home := t.TempDir()
		mkTree(t, home, FlatpakSteamRoot+"/steamapps/libraryfolders.vdf")
		if got, want := findClientRoot(home, ""), filepath.Join(home, FlatpakSteamRoot); got != resolvePath(want) {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	t.Run("custom with only libraryfolders is the last resort", func(t *testing.T) {
		home := t.TempDir()
		mkTree(t, home, "lib/steamapps/libraryfolders.vdf")
		if got, want := findClientRoot(home, filepath.Join(home, "lib")), filepath.Join(home, "lib"); got != resolvePath(want) {
			t.Errorf("got %q, want %q", got, want)
		}
		if got := findClientRoot(t.TempDir(), ""); got != "" {
			t.Errorf("empty home: got %q", got)
		}
	})
}

func TestPathsClientRootAndFlatpak(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	lib := filepath.Join(home, "games", "steamapps")
	p := &Paths{SteamApps: lib}
	if p.ClientRoot() != filepath.Join(home, "games") || p.Flatpak() {
		t.Errorf("no client: root %q flatpak %v", p.ClientRoot(), p.Flatpak())
	}
	p.Client = filepath.Join(home, ".local", "share", "Steam")
	if p.ClientRoot() != p.Client || p.Flatpak() {
		t.Errorf("native client: root %q flatpak %v", p.ClientRoot(), p.Flatpak())
	}
	// A DayZ library inside the Flatpak tree means Flatpak Steam, even
	// when the client root found is another one.
	p.SteamApps = filepath.Join(home, FlatpakSteamRoot, "steamapps")
	if !p.Flatpak() {
		t.Error("library in the Flatpak tree not detected as Flatpak")
	}
	p = &Paths{SteamApps: lib, Client: filepath.Join(home, FlatpakSteamRoot)}
	if !p.Flatpak() {
		t.Error("Flatpak client root not detected")
	}
}

func TestDetectPathsSecondaryLibrary(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	mkTree(t, home, FlatpakSteamRoot+"/steam.sh", "games/steamapps/common/DayZ/")
	p, err := DetectPaths(filepath.Join(home, "games"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Root() != filepath.Join(home, "games") || p.ClientRoot() != resolvePath(filepath.Join(home, FlatpakSteamRoot)) {
		t.Errorf("root %q client %q", p.Root(), p.ClientRoot())
	}
	if want := filepath.Join(home, ".var", "app", "com.valvesoftware.Steam", "data", "dzman", "steamcmd-content"); p.CmdContent != want {
		t.Errorf("content %q, want the Flatpak one %q", p.CmdContent, want)
	}
}

const registryFixture = `"Registry"
{
	"HKCU"
	{
		"Software"
		{
			"Valve"
			{
				"Steam"
				{
					"AutoLoginUser"		"bob"
					"SourceModInstallPath"		"/home/bob/.steam/steam/steamapps/sourcemods"
					// a comment
					"apps"
					{
						"7"
						{
							"Running"		"1"
						}
						"221100"
						{
							"installed"		"1"
							"Running"		"%s"
							"name"		"say \"hi\" {not a block}"
						}
					}
				}
			}
		}
	}
}
`

func TestAppRunning(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	on := write("on.vdf", strings.Replace(registryFixture, "%s", "1", 1))
	off := write("off.vdf", strings.Replace(registryFixture, "%s", "0", 1))
	noKey := write("nokey.vdf", strings.Replace(registryFixture, `"Running"		"%s"`, "", 1))
	broken := write("broken.vdf", `"Registry" { "HKCU" { "Software" {`)
	missing := filepath.Join(dir, "missing.vdf")
	for _, tc := range []struct {
		name           string
		files          []string
		running, known bool
	}{
		{"running", []string{on}, true, true},
		{"not running", []string{off}, false, true},
		{"no key", []string{noKey}, false, false},
		{"missing file", []string{missing}, false, false},
		{"broken file", []string{broken}, false, false},
		{"any file running", []string{missing, off, on}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			running, known := AppRunning(tc.files, GameID)
			if running != tc.running || known != tc.known {
				t.Errorf("got running=%v known=%v, want %v %v", running, known, tc.running, tc.known)
			}
		})
	}
}

func TestDownloadModContextCancelStopsSteamCMD(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	t.Setenv("DZMAN_TEST_PIDFILE", pidFile)
	// Like steamcmd.sh: the real work runs as a child, not via exec.
	script := filepath.Join(t.TempDir(), "steamcmd.sh")
	body := "#!/bin/sh\nsleep 60 &\necho $! > \"$DZMAN_TEST_PIDFILE\"\nwait\necho done\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := DownloadModContext(ctx, script, "bob", t.TempDir(), "123")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want deadline exceeded", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("returned after %v, want well under the kill grace period", d)
	}
	raw, rerr := os.ReadFile(pidFile)
	if rerr != nil {
		t.Fatalf("the wrapper never started its child: %v", rerr)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	deadline := time.Now().Add(3 * time.Second)
	for !processGone(pid) {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("child %d of the wrapper is still running", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// processGone reports whether pid no longer exists or is a zombie.
func processGone(pid int) bool {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	_, after, _ := strings.Cut(string(stat), ") ")
	return strings.HasPrefix(after, "Z")
}

func TestDownloadModContextKillsChildIgnoringTerm(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	t.Setenv("DZMAN_TEST_PIDFILE", pidFile)
	// The child ignores SIGTERM and has its output away from the pipe, so the
	// pipe closes when the wrapper dies and only the group kill can stop it.
	script := filepath.Join(t.TempDir(), "steamcmd.sh")
	body := "#!/bin/sh\n(trap '' TERM; exec sleep 60) >/dev/null 2>&1 &\necho $! > \"$DZMAN_TEST_PIDFILE\"\nwait\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	err := DownloadModContext(ctx, script, "bob", t.TempDir(), "123")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want deadline exceeded", err)
	}
	raw, rerr := os.ReadFile(pidFile)
	if rerr != nil {
		t.Fatalf("the wrapper never started its child: %v", rerr)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	deadline := time.Now().Add(3 * time.Second)
	for !processGone(pid) {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("child %d that ignores SIGTERM is still running", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
