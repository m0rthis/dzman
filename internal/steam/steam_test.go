package steam

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
	cmd := LoginCmd("steamcmd", "bob")
	if got := strings.Join(cmd.Args, " "); got != "steamcmd +login bob +quit" {
		t.Errorf("args = %q", got)
	}
	if cmd.Stdin != nil || cmd.Stdout != nil || cmd.Stderr != nil {
		t.Error("stdio must be left for ExecProcess")
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
			got, err := findSteamCMD(tt.configured(home, bin), home, exec.LookPath)
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
