# dzman

A terminal UI for launching [DayZ](https://dayz.com) and browsing servers on
Linux. It's a Go reimplementation of the [dayz-ctl](https://github.com/WoozyMasta/dayz-ctl)
bash launcher, built on [Bubble Tea](https://github.com/charmbracelet/bubbletea),
with most external-tool dependencies replaced by native Go.

## Features

- **Server browser** with live filters (map, mods, players, first/third person,
  BattlEye, password, platform, …), fuzzy search, favorites, and recently played.
- **Live server info** via a native Valve A2S_INFO query over UDP — real ping and
  current player counts, not just the API snapshot.
- **Direct connect** by IP and port, with a password prompt for protected servers.
- **Mod handling**: detects mods a server needs that you don't have, and either
  installs them via SteamCMD or opens the Workshop pages for manual subscription. If SteamCMD has no cached credentials, press `l` on
  the missing-mods page (or accept the prompt in setup) to log in interactively.
- **Offline mode**: install/update and launch
  [DayZCommunityOfflineMode](https://github.com/Arkensor/DayZCommunityOfflineMode).
- **Launch options** editor, installed-mods view, DayZ news, and desktop-shortcut
  creation for one-click server connect.

## Requirements

- Linux with a recent Go toolchain (see `go.mod`).
- The Steam client (`steam`) — required to launch the game.
- `steamcmd` — optional, for automatic mod install/update. It needs a one-time interactive login
  (password / Steam Guard), which dzman can run for you. dzman finds `steamcmd` or `steamcmd.sh` on `PATH`
  or in the usual install folders (`~/steamcmd`, `~/.steam/steamcmd`, `/usr/games`).
- `xdg-open` — optional, to open Workshop pages in a browser.

## Install

```bash
go build -o dzman ./cmd
# or install to $GOBIN
go install github.com/braydencw1/dzman/cmd@latest
```

Prebuilt Linux binaries (amd64/arm64) are attached to each
[release](https://github.com/braydencw1/dzman/releases).

## Usage

```bash
dzman                      # launch the TUI
dzman connect <ip> <port>  # connect directly (used by desktop shortcuts)
dzman version              # print version and build info
dzman help                 # show usage
```

In the browser: `↑↓`/`jk` move · `enter` select · `f` toggle favorite ·
`/` search · `esc` back · `q` quit.

## Configuration

State lives under `~/.config/dzman/`:

| File | Purpose |
| --- | --- |
| `dzman.json` | app settings (API URL, cache TTLs, SteamCMD, filter defaults, `theme`) |
| `profile.json` | player name, Steam login, favorites, history, launch options |
| `servers.json` | cached server list (refreshed per `server_db_ttl`) |
| `news.json` | cached DayZ news |

On first run a short setup wizard collects your player name and (optionally)
SteamCMD login. DayZ also needs `vm.max_map_count` ≥ 1048576; dzman warns and
offers to set it.

Set `"theme"` in `dzman.json` to `blue` (default), `purple`, `orange`, or any
`#RRGGBB` hex to change the accent colour.

## Development

```bash
go test ./...        # unit tests (offline)
go vet ./...
golangci-lint run    # lint
```

Version metadata is injected at build time via `-ldflags` into
`internal/version` (see `.github/workflows/release.yml`); plain builds fall back
to the Go toolchain's embedded VCS stamps.

## Credits

- Server and news data from [dayzsalauncher.com](https://dayzsalauncher.com).
- Original concept and UX from [dayz-ctl](https://github.com/WoozyMasta/dayz-ctl).
