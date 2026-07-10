package a2s

import (
	"encoding/binary"
	"testing"
)

// buildInfoPayload assembles an A2S_INFO payload (everything after the 0x49
// reply-type byte) so we can exercise parseInfo without a network. When
// gamePort > 0 an Extra Data Flag section carrying that port is appended.
func buildInfoPayload(name, mapName, version string, players, max byte, env byte, password, vac bool, gamePort uint16) []byte {
	var b []byte
	b = append(b, 0x11) // protocol
	b = append(b, []byte(name)...)
	b = append(b, 0)
	b = append(b, []byte(mapName)...)
	b = append(b, 0)
	b = append(b, []byte("dayz")...) // folder
	b = append(b, 0)
	b = append(b, []byte("DayZ")...) // game
	b = append(b, 0)
	b = append(b, 0x6C, 0x00) // app id (uint16, truncated is fine)
	b = append(b, players, max, 0 /*bots*/, 0x64 /*type 'd'*/, env)
	b = append(b, boolByte(password), boolByte(vac))
	b = append(b, []byte(version)...)
	b = append(b, 0)
	if gamePort > 0 {
		// Extra Data Flag with only the game-port bit set.
		b = append(b, edfPort)
		b = binary.LittleEndian.AppendUint16(b, gamePort)
	}
	return b
}

func boolByte(v bool) byte {
	if v {
		return 1
	}
	return 0
}

func TestParseInfo(t *testing.T) {
	payload := buildInfoPayload("Test Server", "ChernarusPlus", "1.26.0",
		42, 60, 'l', true, false, 2302)

	info, err := parseInfo(payload)
	if err != nil {
		t.Fatalf("parseInfo returned error: %v", err)
	}
	if info.Name != "Test Server" {
		t.Errorf("Name = %q, want %q", info.Name, "Test Server")
	}
	if info.Map != "ChernarusPlus" {
		t.Errorf("Map = %q, want %q", info.Map, "ChernarusPlus")
	}
	if info.Version != "1.26.0" {
		t.Errorf("Version = %q, want %q", info.Version, "1.26.0")
	}
	if info.Players != 42 || info.MaxPlayers != 60 {
		t.Errorf("Players = %d/%d, want 42/60", info.Players, info.MaxPlayers)
	}
	if info.Environment != 'l' {
		t.Errorf("Environment = %q, want 'l'", info.Environment)
	}
	if !info.Password {
		t.Error("Password = false, want true")
	}
	if info.VAC {
		t.Error("VAC = true, want false")
	}
	if info.GamePort != 2302 {
		t.Errorf("GamePort = %d, want 2302", info.GamePort)
	}
}

func TestParseInfoNoExtraData(t *testing.T) {
	// gamePort 0 => no EDF section; parse must still succeed.
	payload := buildInfoPayload("S", "M", "v", 1, 2, 'w', false, true, 0)
	info, err := parseInfo(payload)
	if err != nil {
		t.Fatalf("parseInfo error: %v", err)
	}
	if info.Environment != 'w' || !info.VAC || info.Password {
		t.Errorf("flags mismatch: env=%q vac=%v pw=%v", info.Environment, info.VAC, info.Password)
	}
	if info.GamePort != 0 {
		t.Errorf("GamePort = %d, want 0 (no EDF)", info.GamePort)
	}
}

func TestParseInfoTruncated(t *testing.T) {
	// A payload that ends mid-string must error, not panic.
	bad := []byte{0x11, 'n', 'a', 'm', 'e'} // no null terminator
	if _, err := parseInfo(bad); err == nil {
		t.Error("expected error for truncated payload, got nil")
	}
}
