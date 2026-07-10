package api

import (
	"encoding/json"
	"testing"
)

func TestModUnmarshalNumberOrString(t *testing.T) {
	tests := []struct {
		name string
		json string
		want string
	}{
		{"numeric id", `{"name":"CF","steamWorkshopId":1559212036}`, "1559212036"},
		{"string id", `{"name":"CF","steamWorkshopId":"1559212036"}`, "1559212036"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var m Mod
			if err := json.Unmarshal([]byte(tt.json), &m); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if m.SteamWorkshopID != tt.want {
				t.Errorf("SteamWorkshopID = %q, want %q", m.SteamWorkshopID, tt.want)
			}
		})
	}
}

func TestModRoundTripKeepsID(t *testing.T) {
	// Marshalling a Mod must preserve steamWorkshopId so a cached server list
	// survives a disk round trip.
	orig := Mod{Name: "CF", SteamWorkshopID: "1559212036"}
	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatal(err)
	}
	var back Mod
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.SteamWorkshopID != orig.SteamWorkshopID {
		t.Errorf("round-trip dropped ID: got %q, want %q", back.SteamWorkshopID, orig.SteamWorkshopID)
	}
	if back.Name != orig.Name {
		t.Errorf("round-trip name: got %q, want %q", back.Name, orig.Name)
	}
}

func TestServerListRoundTrip(t *testing.T) {
	orig := &ServerList{
		Status:        0,
		PlayersOnline: 12345,
		Result: []Server{{
			Name:     "Test",
			Map:      "ChernarusPlus",
			Endpoint: Endpoint{IP: "1.2.3.4", Port: 27016},
			GamePort: 2302,
			Mods:     []Mod{{Name: "CF", SteamWorkshopID: "1559212036"}},
		}},
	}
	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatal(err)
	}
	var back ServerList
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Result) != 1 || len(back.Result[0].Mods) != 1 {
		t.Fatalf("structure lost in round trip: %+v", back)
	}
	if back.Result[0].Mods[0].SteamWorkshopID != "1559212036" {
		t.Errorf("mod id lost: %q", back.Result[0].Mods[0].SteamWorkshopID)
	}
}

func TestWorkshopURL(t *testing.T) {
	m := Mod{SteamWorkshopID: "123"}
	want := "https://steamcommunity.com/sharedfiles/filedetails/?id=123"
	if m.WorkshopURL() != want {
		t.Errorf("WorkshopURL = %q, want %q", m.WorkshopURL(), want)
	}
}
