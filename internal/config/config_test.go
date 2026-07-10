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
