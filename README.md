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
- **Mod handling**: detects mods a server needs that you don't have and asks
  your running Steam client to subscribe to and download them, the way DZSA
  Launcher does. SteamCMD and opening the Workshop pages are fallbacks. See
  [Mods](#mods).
- **Offline mode**: install/update and launch
  [DayZCommunityOfflineMode](https://github.com/Arkensor/DayZCommunityOfflineMode).
- **Launch options** editor, installed-mods view, DayZ news, and desktop-shortcut
  creation for one-click server connect.

## Requirements

- Linux with a recent Go toolchain (see `go.mod`) to build from source.
- A glibc-based distro to run the prebuilt binary. Steamworks support loads
  `libsteam_api.so` without cgo, which makes the release binary dynamically
  link glibc, so musl (Alpine) and NixOS without nix-ld cannot run it. Steam
  itself needs glibc, so this only rules out systems that could not run the
  Steam client anyway.
- The Steam client (`steam`) — required to launch the game, and the normal way
  mods are downloaded.
- A 64-bit Linux `libsteam_api.so` — for downloading mods through the Steam
  client. Usually found automatically, see [Mods](#mods).
- `steamcmd` — optional fallback for mod installs. It needs a one-time interactive login
  (password / Steam Guard), which dzman can run for you. dzman finds `steamcmd` or `steamcmd.sh` on `PATH`
  or in the usual install folders (`~/steamcmd`, `~/.steam/steamcmd`, `/usr/games`).
- `xdg-open` — optional, to open Workshop pages in a browser.

## Install

```bash
go build -o dzman ./cmd
# or install to $GOBIN
go install github.com/m0rthis/dzman/cmd@latest
```

Prebuilt Linux binaries (amd64/arm64) are attached to each
[release](https://github.com/m0rthis/dzman/releases).

## Usage

```bash
dzman                      # launch the TUI
dzman connect <ip> <port>  # sync the server's mods, then connect (used by desktop shortcuts)
dzman rejoin               # same, for the most recent server in the history
dzman version              # print version and build info
dzman help                 # show usage
```

`connect` (the port is the game port) and `rejoin` do not open the TUI. They look
the server up in the server list, install any missing mods through the
configured [mod backend](#mods), record the join in the history, and launch
DayZ. Progress is printed as plain lines, and if the mods cannot be installed
(backend `manual`, SteamCMD not logged in, Steam not running) dzman says why and
exits non-zero without launching. Like the TUI, they only install missing mods;
they do not update installed ones. A server password is read from the
`DZMAN_PASSWORD` environment variable. `rejoin` needs the server list and the setup wizard to have run once, so run
`dzman` first on a new install. `connect` still launches (without mods) when the
server list cannot be loaded or the server is not in it. The main menu also has a
"Rejoin <server>" item at the top once the history is not empty.

In the browser: `↑↓`/`jk` move · `enter` select · `f` toggle favorite ·
`/` search · `esc` back · `q` quit.

## Configuration

State lives under `~/.config/dzman/`:

| File | Purpose |
| --- | --- |
| `dzman.json` | app settings (API URL, cache TTLs, mod backend, SteamCMD, filter defaults, `theme`) |
| `profile.json` | player name, Steam login, favorites, history, launch options |
| `servers.json` | cached server list (refreshed per `server_db_ttl`) |
| `news.json` | cached DayZ news |

On first run a short setup wizard collects your player name and, if you want
the SteamCMD fallback, your SteamCMD login. DayZ also needs `vm.max_map_count` ≥ 1048576; dzman warns and
offers to set it.

Set `"theme"` in `dzman.json` to `blue` (default), `purple`, `orange`, or any
`#RRGGBB` hex to change the accent colour.

## Mods

When a server needs mods you don't have, the missing-mods page lists them and
shows the active mod backend. Set it under Config > Mod Backend or with
`mod_backend` in `dzman.json`:

| Value | What it does |
| --- | --- |
| `auto` (default) | `steamworks` if `libsteam_api.so` is found and Steam is running (not Flatpak Steam), else `steamcmd` if enabled and set up, else `manual` |
| `steamworks` | the running Steam client subscribes to and downloads the mods |
| `steamcmd` | SteamCMD downloads the mods into dzman's own folder (needs `steamcmd_enabled`; otherwise it acts as `manual`) |
| `manual` | opens the Workshop pages so you subscribe in Steam yourself |

If Steamworks fails in `auto` mode, dzman shows the error and offers SteamCMD
or the Workshop pages instead of switching on its own.

Config > Update Mods asks the Steam client to update its workshop mods
(Steamworks), or re-downloads dzman's SteamCMD mods (SteamCMD). dzman never
updates the game itself; the Steam client does that.

With Steamworks, Update Mods only updates mods your account is subscribed to.
A mod in the client's workshop folder that is not subscribed (for example one
left there by an older dzman's SteamCMD runs) is skipped and listed as "not
subscribed", because Steam may uninstall an unsubscribed item once the "game"
exits. Install it from a server's missing-mods page, which subscribes, or
subscribe in Steam.

Workshop IDs from the server list must be plain digits with no leading zero.
A mod with any other ID is shown as invalid and is never passed to Steam,
SteamCMD or the DayZ folder. An installed mod's ID is its folder name; the
`publishedid` in its `meta.cpp` is only shown when it differs.

### Steamworks

dzman loads the Steamworks API from `libsteam_api.so` and talks to the Steam
client as DayZ (app 221100), so mods land in the client's normal workshop folder
under your own account. No separate login is needed. This runs in a short-lived
helper process (`dzman steamworks-download`), because while it is connected
Steam shows DayZ as running. dzman waits for the helper to exit, and a few
seconds more for Steam to let go of DayZ, before it launches anything, and
stops the helper if you quit. A run gives up after
`steamworks_timeout` (default `30m`) or after `steamworks_stall` (default `5m`)
without progress. Both are capped at `24h`. A mod dzman subscribed to that
Steam neither marks subscribed nor starts downloading within 45 seconds fails
early, since the item may have been removed or made private or hidden.

dzman looks for `libsteam_api.so` in this order:

1. `steam_api_path` in `dzman.json` (Config > Change Steam API Path)
2. `~/.config/dzman/libsteam_api.so`
3. the native Linux games in your Steam libraries (`steamapps/common/*/`, plus
   `bin`, `linux64`, `x86_64` and Unity `Plugins` subfolders), including other
   library folders from `libraryfolders.vdf`. A copy that exports more than
   three SteamUGC versions is skipped as a likely Steam emulator (Goldberg,
   gbe_fork); set `steam_api_path` to use one anyway. Of the rest, the copy
   with the newest SteamUGC version is used, the first in sorted path order
   on a tie.

It must be the 64-bit Linux build. If none of your games ship one, download the
Steamworks SDK from partner.steamgames.com (log in with your Steam account) and
copy `redistributable_bin/linux64/libsteam_api.so` to `~/.config/dzman/`.
The missing-mods page and Config show which copy is in use.

With Flatpak Steam, `auto` skips Steamworks: the helper runs outside the
sandbox and most likely can't reach the sandboxed client. Set `mod_backend` to
`steamworks` to try it anyway. dzman treats the client as Flatpak Steam when
the client's root, or the library DayZ is in, is under
`~/.var/app/com.valvesoftware.Steam`.

The Steam client's root is found separately from the DayZ library, so a
Steam root setting that points at a secondary library still works: dzman
takes the first of `~/.steam/root`, `~/.steam/steam`, `~/.local/share/Steam`
and Flatpak Steam's root that has `steamapps/libraryfolders.vdf`, `steam.sh`
or `ubuntu12_32`. A Steam root setting that is itself a client install
(`steam.sh` or `ubuntu12_32`) wins.

Mods the Steam client downloads belong to the client. dzman doesn't mark them,
and Remove Managed Mods never deletes them. Unsubscribe in Steam to remove them.

### SteamCMD

SteamCMD downloads into `~/.local/share/dzman/steamcmd-content`, and dzman runs
it with `HOME` set to `~/.local/share/dzman/steamcmd-home` (and the
`XDG_DATA_HOME`, `XDG_CONFIG_HOME`, `XDG_CACHE_HOME` and `XDG_STATE_HOME`
dirs under it). That's because
`steamcmd.sh` writes its own `steamclient.so` links and `registry.vdf` into
`~/.steam`, which the Steam client and every game read, and without an install
dir it writes workshop content and ACF files into its own steamapps. Older dzman
versions let it do both next to the Steam client, which could break cloud sync.
dzman also refuses a `steamcmd` that lives inside any Steam client folder
(`~/.steam/root`, `~/.steam/steam`, `~/.local/share/Steam`, Flatpak Steam's
`~/.var/app/com.valvesoftware.Steam/data/Steam`, or the detected library).

With Flatpak Steam the content dir is
`~/.var/app/com.valvesoftware.Steam/data/dzman/steamcmd-content` instead,
because the sandboxed game can't see `~/.local/share/dzman` and the mod links
in the DayZ folder would break. It is still outside the client's own
`steamapps`. Mods an older dzman downloaded to `~/.local/share/dzman` are not
moved, so install them again. dzman refuses a content dir with whitespace,
`"` or `+` in it, because steamcmd may split or misread it.

The Steam login must be a Steam account name: letters, digits and `_` only.

SteamCMD keeps its login in its own install folder, so an existing login still
works. The Debian/Ubuntu `/usr/games/steamcmd` wrapper installs itself under
`~/.steam/steamcmd`, so with the new `HOME` it sets itself up once more and may
ask you to log in again (press `l` on the missing-mods page).

Mods downloaded this way get a `.dzman` marker. Remove Managed Mods deletes
those, and only inside dzman's SteamCMD folder. When dzman links mods into the
DayZ folder it looks in the Steam client's workshop folder first, then in
dzman's SteamCMD folder.

dzman owns the `@<id>` symlinks in the DayZ folder. It replaces one that
points somewhere else, including a link you made yourself. A real folder
named `@<id>` is left alone.

### Config keys

| Key | Default | Meaning |
| --- | --- | --- |
| `mod_backend` | `auto` | `auto`, `steamworks`, `steamcmd` or `manual` |
| `steam_api_path` | empty | explicit `libsteam_api.so` (`~` is expanded; a relative path is taken relative to `~/.config/dzman/`); empty means search |
| `steamworks_timeout` | `30m` | overall limit for one Steamworks run |
| `steamworks_stall` | `5m` | give up after this long without download progress |
| `steamcmd_enabled` | `true` | allow SteamCMD (the `steamcmd` backend and `auto`'s fallback) |
| `steamcmd_path` | `steamcmd` | SteamCMD binary; `steamcmd` means auto-detect |

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
