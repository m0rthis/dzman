// Package api provides clients for the DayZ server list, news, Steam stats,
// and auxiliary network utilities.
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"
)

const (
	// BaseURL is the default dayzsalauncher API base.
	BaseURL = "https://dayzsalauncher.com/api/v1"
	// GameID is the Steam app ID for DayZ.
	GameID = "221100"
	// Repo is the GitHub repository used for release update checks.
	Repo = "braydencw1/dzman"

	reqTimeout    = 10 * time.Second
	serverTimeout = 30 * time.Second
)

// Mod is a Steam Workshop mod attached to a server.
type Mod struct {
	Name            string `json:"name"`
	SteamWorkshopID string `json:"-"`
}

// UnmarshalJSON handles steamWorkshopId being either a string or a number.
func (m *Mod) UnmarshalJSON(data []byte) error {
	var raw struct {
		Name            string      `json:"name"`
		SteamWorkshopID json.Number `json:"steamWorkshopId"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	m.Name = raw.Name
	m.SteamWorkshopID = raw.SteamWorkshopID.String()
	return nil
}

// MarshalJSON keeps steamWorkshopId in the output so the server list survives
// a disk-cache round trip.
func (m Mod) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Name            string `json:"name"`
		SteamWorkshopID string `json:"steamWorkshopId"`
	}{m.Name, m.SteamWorkshopID})
}

// WorkshopURL returns the Steam Workshop page for the mod.
func (m Mod) WorkshopURL() string {
	return "https://steamcommunity.com/sharedfiles/filedetails/?id=" + m.SteamWorkshopID
}

// Endpoint is the network address of a server.
type Endpoint struct {
	IP   string `json:"ip"`
	Port int    `json:"port"`
}

// Server represents a single DayZ server from the API.
type Server struct {
	Name             string   `json:"name"`
	Map              string   `json:"map"`
	Players          int      `json:"players"`
	MaxPlayers       int      `json:"maxPlayers"`
	Time             string   `json:"time"`
	TimeAcceleration float64  `json:"timeAcceleration"`
	Password         bool     `json:"password"`
	BattlEye         bool     `json:"battlEye"`
	VAC              bool     `json:"vac"`
	FirstPersonOnly  bool     `json:"firstPersonOnly"`
	Shard            string   `json:"shard"`
	Environment      string   `json:"environment"`
	Version          string   `json:"version"`
	Endpoint         Endpoint `json:"endpoint"`
	GamePort         int      `json:"gamePort"`
	Mods             []Mod    `json:"mods"`
}

// ServerList is the full response from the launcher/servers endpoint.
type ServerList struct {
	Status        int      `json:"status"`
	Result        []Server `json:"result"`
	PlayersOnline int      `json:"playersOnline"`
}

// NewsArticle is a single article from dayz.com.
type NewsArticle struct {
	Title           string `json:"title"`
	Slug            string `json:"slug"`
	ArticleCategory struct {
		Slug string `json:"slug"`
	} `json:"ArticleCategory"`
}

// NewsResponse is the response from the dayz.com news API.
type NewsResponse struct {
	Rows []NewsArticle `json:"rows"`
}

func newClient(timeout time.Duration, userAgent string) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &userAgentTransport{
			ua:   userAgent,
			base: http.DefaultTransport,
		},
	}
}

type userAgentTransport struct {
	ua   string
	base http.RoundTripper
}

func (t *userAgentTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("User-Agent", t.ua)
	return t.base.RoundTrip(req)
}

// FetchServers downloads the full server list from the API.
func FetchServers(apiURL, userAgent string) (*ServerList, error) {
	client := newClient(serverTimeout, userAgent)
	resp, err := client.Get(apiURL + "/launcher/servers/dayz")
	if err != nil {
		return nil, fmt.Errorf("fetch servers: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var sl ServerList
	if err := json.NewDecoder(resp.Body).Decode(&sl); err != nil {
		return nil, fmt.Errorf("decode servers: %w", err)
	}
	if sl.Status != 0 {
		return nil, fmt.Errorf("API error status %d", sl.Status)
	}
	return &sl, nil
}

// FetchPlayersOnline returns the number of DayZ players online from Steam.
func FetchPlayersOnline(userAgent string) (int, error) {
	u := fmt.Sprintf(
		"https://api.steampowered.com/ISteamUserStats/GetNumberOfCurrentPlayers/v1/?appid=%s",
		GameID,
	)
	client := newClient(reqTimeout, userAgent)
	resp, err := client.Get(u)
	if err != nil {
		return 0, fmt.Errorf("fetch players online: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var result struct {
		Response struct {
			PlayerCount int `json:"player_count"`
		} `json:"response"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, fmt.Errorf("decode players online: %w", err)
	}
	return result.Response.PlayerCount, nil
}

// FetchNews downloads the latest DayZ news articles from dayz.com.
func FetchNews(userAgent string) (*NewsResponse, error) {
	client := newClient(reqTimeout, userAgent)
	resp, err := client.Get("https://dayz.com/api/article?rowsPerPage=100")
	if err != nil {
		return nil, fmt.Errorf("fetch news: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var nr NewsResponse
	if err := json.NewDecoder(resp.Body).Decode(&nr); err != nil {
		return nil, fmt.Errorf("decode news: %w", err)
	}
	return &nr, nil
}

// FetchCountry looks up the country for an IP using ip-api.com (no key needed).
func FetchCountry(ip string) (string, error) {
	client := newClient(reqTimeout, "dzman")
	resp, err := client.Get("http://ip-api.com/json/" + ip + "?fields=country,countryCode")
	if err != nil {
		return "", fmt.Errorf("fetch country: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var result struct {
		Country     string `json:"country"`
		CountryCode string `json:"countryCode"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode country: %w", err)
	}
	if result.Country == "" {
		return "Unknown", nil
	}
	return result.Country, nil
}

// LatestRelease returns the newest release tag from the dzman GitHub repo.
// Returns "" (no error) when the repo has no published releases yet.
func LatestRelease(userAgent string) (string, error) {
	client := newClient(reqTimeout, userAgent)
	resp, err := client.Get("https://api.github.com/repos/" + Repo + "/releases/latest")
	if err != nil {
		return "", fmt.Errorf("fetch latest release: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return "", nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch latest release: HTTP %d", resp.StatusCode)
	}
	var release struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return "", fmt.Errorf("decode latest release: %w", err)
	}
	return release.TagName, nil
}

// LoadCache reads a cached JSON value from path if the file is newer than ttl.
func LoadCache[T any](path string, ttl time.Duration) (*T, bool) {
	fi, err := os.Stat(path)
	if err != nil || time.Since(fi.ModTime()) > ttl {
		return nil, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, false
	}
	return &v, true
}

// SaveCache writes v as JSON to path atomically (tmp file + rename).
func SaveCache[T any](path string, v *T) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal cache: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write cache: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename cache: %w", err)
	}
	return nil
}

// BattlemetricsURL finds the Battlemetrics page for a DayZ server.
// Returns empty string if not found.
func BattlemetricsURL(ip string, queryPort int, name, userAgent string) (string, error) {
	params := url.Values{}
	params.Set("page[size]", "10")
	params.Set("filter[game]", "dayz")
	params.Set("filter[search]", ip+" "+name)

	reqURL := "https://api.battlemetrics.com/servers?" + params.Encode()
	client := newClient(reqTimeout, userAgent)
	resp, err := client.Get(reqURL)
	if err != nil {
		return "", fmt.Errorf("fetch battlemetrics: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var result struct {
		Data []struct {
			ID         string `json:"id"`
			Attributes struct {
				IP        string `json:"ip"`
				PortQuery int    `json:"portQuery"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode battlemetrics: %w", err)
	}
	for _, s := range result.Data {
		if s.Attributes.IP == ip && s.Attributes.PortQuery == queryPort {
			return "https://www.battlemetrics.com/servers/dayz/" + s.ID, nil
		}
	}
	return "", nil
}
