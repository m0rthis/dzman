package launcher

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	offlineRepo    = "Arkensor/DayZCommunityOfflineMode"
	offlineTimeout = 120 * time.Second
)

// OfflineMission is an available DayZCommunityOfflineMode mission.
type OfflineMission struct {
	Name string
	Path string
}

// AvailableMissions lists installed offline missions under dayzPath/Missions.
func AvailableMissions(dayzPath string) ([]OfflineMission, error) {
	missionsPath := filepath.Join(dayzPath, "Missions")
	entries, err := os.ReadDir(missionsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read missions: %w", err)
	}

	var out []OfflineMission
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "DayZCommunityOfflineMode") {
			out = append(out, OfflineMission{
				Name: e.Name(),
				Path: filepath.Join(missionsPath, e.Name()),
			})
		}
	}
	return out, nil
}

// OfflineVersions returns the installed version and the latest GitHub release tag.
func OfflineVersions(dayzPath string) (installed, latest string, err error) {
	versionFile := filepath.Join(dayzPath, "Missions",
		"DayZCommunityOfflineMode.ChernarusPlus", ".version")
	if data, readErr := os.ReadFile(versionFile); readErr == nil {
		installed = strings.TrimSpace(string(data))
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(
		"https://api.github.com/repos/" + offlineRepo + "/releases/latest",
	)
	if err != nil {
		return installed, "", fmt.Errorf("check latest release: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var release struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return installed, "", fmt.Errorf("decode release: %w", err)
	}
	return installed, release.TagName, nil
}

// InstallOffline downloads and extracts DayZCommunityOfflineMode into dayzPath/Missions.
func InstallOffline(dayzPath, version string) error {
	missionsPath := filepath.Join(dayzPath, "Missions")
	if err := os.MkdirAll(missionsPath, 0o755); err != nil {
		return fmt.Errorf("create missions dir: %w", err)
	}

	tarURL := "https://api.github.com/repos/" + offlineRepo + "/tarball/" + version
	client := &http.Client{Timeout: offlineTimeout}
	resp, err := client.Get(tarURL)
	if err != nil {
		return fmt.Errorf("download offline mode: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := extractTarGz(resp.Body, missionsPath, 2); err != nil {
		return fmt.Errorf("extract offline mode: %w", err)
	}

	// Write .version marker.
	vf := filepath.Join(missionsPath,
		"DayZCommunityOfflineMode.ChernarusPlus", ".version")
	if err := os.WriteFile(vf, []byte(version), 0o644); err != nil {
		return fmt.Errorf("write version file: %w", err)
	}
	return nil
}

// SetHiveEnabled toggles the HIVE_ENABLED flag in the offline client script.
func SetHiveEnabled(missionPath string, enabled bool) error {
	clientFile := filepath.Join(missionPath, "core", "CommunityOfflineClient.c")
	data, err := os.ReadFile(clientFile)
	if err != nil {
		return fmt.Errorf("read offline client: %w", err)
	}

	newVal := "HIVE_ENABLED = false;"
	if enabled {
		newVal = "HIVE_ENABLED = true;"
	}

	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		if strings.Contains(line, "HIVE_ENABLED") && strings.Contains(line, "=") {
			lines[i] = newVal
		}
	}
	return os.WriteFile(clientFile, []byte(strings.Join(lines, "\n")), 0o644)
}

func extractTarGz(r io.Reader, destPath string, stripComponents int) error {
	gr, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("gzip reader: %w", err)
	}
	defer func() { _ = gr.Close() }()

	tr := tar.NewReader(gr)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}

		parts := strings.Split(filepath.ToSlash(header.Name), "/")
		if len(parts) <= stripComponents {
			continue
		}
		rel := filepath.Join(parts[stripComponents:]...)
		// Refuse entries that would escape destPath (e.g. "../" components).
		if !filepath.IsLocal(rel) {
			continue
		}
		target := filepath.Join(destPath, rel)

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("mkdir %s: %w", target, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("mkdir parent %s: %w", target, err)
			}
			f, err := os.Create(target)
			if err != nil {
				return fmt.Errorf("create %s: %w", target, err)
			}
			if _, err := io.Copy(f, tr); err != nil { //nolint:gosec // tar extraction controlled by us
				_ = f.Close()
				return fmt.Errorf("write %s: %w", target, err)
			}
			if err := f.Close(); err != nil {
				return fmt.Errorf("close %s: %w", target, err)
			}
		}
	}
	return nil
}
