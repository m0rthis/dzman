package config

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestDurationMarshalHumanReadable(t *testing.T) {
	c := defaults()
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, `"server_db_ttl":"5m0s"`) {
		t.Errorf("server_db_ttl not human-readable in:\n%s", s)
	}
	if !strings.Contains(s, `"request_timeout":"10s"`) {
		t.Errorf("request_timeout not human-readable in:\n%s", s)
	}
}

func TestDurationUnmarshal(t *testing.T) {
	tests := []struct {
		name string
		json string
		want time.Duration
	}{
		{"string form", `{"server_db_ttl":"5m"}`, 5 * time.Minute},
		{"legacy nanoseconds", `{"server_db_ttl":300000000000}`, 5 * time.Minute},
		{"hours string", `{"server_db_ttl":"1h30m"}`, 90 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var c Config
			if err := json.Unmarshal([]byte(tt.json), &c); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := c.ServerDBTTL.Std(); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDurationUnmarshalInvalid(t *testing.T) {
	var c Config
	if err := json.Unmarshal([]byte(`{"server_db_ttl":"not-a-duration"}`), &c); err == nil {
		t.Error("expected error for invalid duration string, got nil")
	}
}

func TestOldConfigGetsBackendDefaults(t *testing.T) {
	// A dzman.json from before mod_backend existed.
	c := defaults()
	if err := json.Unmarshal([]byte(`{"steamcmd_enabled":false,"steamcmd_path":"/x/steamcmd.sh"}`), c); err != nil {
		t.Fatal(err)
	}
	if c.Backend() != BackendAuto || c.SteamCMDEnabled || c.SteamworksTimeout.Std() != 30*time.Minute {
		t.Errorf("got backend %q, steamcmd %v, timeout %s", c.Backend(), c.SteamCMDEnabled, c.SteamworksTimeout)
	}
	c.ModBackend = "bogus"
	if c.Backend() != BackendAuto {
		t.Errorf("unknown backend = %q", c.Backend())
	}
}

func TestCapLimits(t *testing.T) {
	c := defaults()
	if err := json.Unmarshal([]byte(`{"steamworks_timeout":"100000h","steamworks_stall":"2h"}`), c); err != nil {
		t.Fatal(err)
	}
	c.capLimits()
	if c.SteamworksTimeout != MaxSteamworksLimit || c.SteamworksStall.Std() != 2*time.Hour {
		t.Errorf("timeout %s stall %s", c.SteamworksTimeout, c.SteamworksStall)
	}
}
