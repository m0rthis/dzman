package config

import (
	"testing"
)

func TestLaunchArgs(t *testing.T) {
	p := DefaultProfile()
	// Defaults enable nosplash, skipintro, high, USEALLAVAILABLECORES, world=empty.
	args := p.LaunchArgs()

	want := map[string]bool{
		"-nosplash":             true,
		"-skipintro":            true,
		"-high":                 true,
		"-USEALLAVAILABLECORES": true,
		"-world=empty":          true,
	}
	got := map[string]bool{}
	for _, a := range args {
		got[a] = true
	}
	for w := range want {
		if !got[w] {
			t.Errorf("missing expected arg %q in %v", w, args)
		}
	}
	// A disabled option must not appear.
	for _, a := range args {
		if a == "-window" {
			t.Errorf("disabled option -window present in %v", args)
		}
	}
}

func TestLaunchArgsValueOption(t *testing.T) {
	p := DefaultProfile()
	opt := p.Options["maxMem"]
	opt.Enabled = true
	v := "4096"
	opt.Value = &v
	p.Options["maxMem"] = opt

	found := false
	for _, a := range p.LaunchArgs() {
		if a == "-maxMem=4096" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected -maxMem=4096 in %v", p.LaunchArgs())
	}
}

func TestAddToHistoryDedupeAndTrim(t *testing.T) {
	p := DefaultProfile()
	p.AddToHistory("A", "1.1.1.1", 2302, 3)
	p.AddToHistory("B", "2.2.2.2", 2302, 3)
	p.AddToHistory("C", "3.3.3.3", 2302, 3)
	// Re-adding A moves it to front, no duplicate.
	p.AddToHistory("A", "1.1.1.1", 2302, 3)

	if len(p.History) != 3 {
		t.Fatalf("history len = %d, want 3", len(p.History))
	}
	if p.History[0].IP != "1.1.1.1" {
		t.Errorf("front = %s, want 1.1.1.1 (most recent)", p.History[0].IP)
	}

	// Exceeding max trims the oldest.
	p.AddToHistory("D", "4.4.4.4", 2302, 3)
	if len(p.History) != 3 {
		t.Fatalf("after trim len = %d, want 3", len(p.History))
	}
	for _, h := range p.History {
		if h.IP == "2.2.2.2" {
			t.Error("oldest entry B should have been trimmed")
		}
	}
}

func TestFavorites(t *testing.T) {
	p := DefaultProfile()
	if p.IsFavorite("1.1.1.1", 2302) {
		t.Error("empty profile should have no favorites")
	}
	p.AddFavorite("Server", "1.1.1.1", 2302)
	p.AddFavorite("Server", "1.1.1.1", 2302) // duplicate no-op
	if len(p.Favorites) != 1 {
		t.Fatalf("favorites len = %d, want 1", len(p.Favorites))
	}
	if !p.IsFavorite("1.1.1.1", 2302) {
		t.Error("expected IsFavorite true after add")
	}
	p.RemoveFavorite("1.1.1.1", 2302)
	if p.IsFavorite("1.1.1.1", 2302) {
		t.Error("expected IsFavorite false after remove")
	}
}
