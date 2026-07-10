package tui

import (
	"fmt"
	"strings"

	"github.com/braydencw1/dzman/internal/api"
)

// filterKind identifies a server-list filter.
type filterKind int

const (
	filterOfficial filterKind = iota
	filterNotOfficial
	filterModded
	filterNotModded
	filterModsLT      // mods < N
	filterModsGT      // mods > N
	filterNoPassword
	filterWithPassword
	filterFirstPerson
	filterThirdPerson
	filterDaytime
	filterNighttime
	filterBattleEye
	filterNoBattleEye
	filterHasPlayers
	filterNoPlayers
	filterNotFull
	filterFull
	filterPlayersAbovePct
	filterPlayersBelowPct
	filterMinSlots
	filterByMap
	filterByModName
	filterByModID
	filterLinux
	filterWindows
)

// filterDef is a single active filter together with an optional parameter.
type filterDef struct {
	kind  filterKind
	label string
	value string // numeric or text value for parameterised filters
}

// matches reports whether a server passes the filter.
func (f filterDef) matches(s api.Server, modLimit, playersLimitPct, playerSlots int) bool {
	switch f.kind {
	case filterOfficial:
		return s.Shard == "public"
	case filterNotOfficial:
		return s.Shard != "public"
	case filterModded:
		return len(s.Mods) > 0
	case filterNotModded:
		return len(s.Mods) == 0
	case filterModsLT:
		return len(s.Mods) > 0 && len(s.Mods) < modLimit
	case filterModsGT:
		return len(s.Mods) > 0 && len(s.Mods) > modLimit
	case filterNoPassword:
		return !s.Password
	case filterWithPassword:
		return s.Password
	case filterFirstPerson:
		return s.FirstPersonOnly
	case filterThirdPerson:
		return !s.FirstPersonOnly
	case filterDaytime:
		return isDay(s.Time)
	case filterNighttime:
		return !isDay(s.Time)
	case filterBattleEye:
		return s.BattlEye
	case filterNoBattleEye:
		return !s.BattlEye
	case filterHasPlayers:
		return s.Players > 0
	case filterNoPlayers:
		return s.Players == 0
	case filterNotFull:
		return s.Players < s.MaxPlayers
	case filterFull:
		return s.Players >= s.MaxPlayers
	case filterPlayersAbovePct:
		if s.MaxPlayers == 0 {
			return false
		}
		return (s.Players*100)/s.MaxPlayers > playersLimitPct
	case filterPlayersBelowPct:
		if s.MaxPlayers == 0 {
			return false
		}
		return (s.Players*100)/s.MaxPlayers < playersLimitPct
	case filterMinSlots:
		return s.MaxPlayers >= playerSlots
	case filterByMap:
		return strings.EqualFold(s.Map, f.value) ||
			strings.Contains(strings.ToLower(s.Map), strings.ToLower(f.value))
	case filterByModName:
		for _, m := range s.Mods {
			if strings.Contains(strings.ToLower(m.Name), strings.ToLower(f.value)) {
				return true
			}
		}
		return false
	case filterByModID:
		for _, m := range s.Mods {
			if strings.Contains(m.SteamWorkshopID, f.value) {
				return true
			}
		}
		return false
	case filterLinux:
		return s.Environment == "l"
	case filterWindows:
		return s.Environment != "l"
	}
	return true
}

// applyFilters returns a subset of servers that pass all active filters.
func applyFilters(servers []api.Server, filters []filterDef, modLimit, playersLimitPct, playerSlots int) []api.Server {
	if len(filters) == 0 {
		return servers
	}
	out := make([]api.Server, 0, len(servers))
	for _, s := range servers {
		pass := true
		for _, f := range filters {
			if !f.matches(s, modLimit, playersLimitPct, playerSlots) {
				pass = false
				break
			}
		}
		if pass {
			out = append(out, s)
		}
	}
	return out
}

// allFilterOptions returns all available filter definitions (without values).
func allFilterOptions() []filterDef {
	return []filterDef{
		{kind: filterOfficial, label: "Official servers"},
		{kind: filterNotOfficial, label: "Community servers"},
		{kind: filterModded, label: "Modded"},
		{kind: filterNotModded, label: "Vanilla (no mods)"},
		{kind: filterModsLT, label: "Mods < default limit"},
		{kind: filterModsGT, label: "Mods > default limit"},
		{kind: filterNoPassword, label: "No password"},
		{kind: filterWithPassword, label: "Password protected"},
		{kind: filterFirstPerson, label: "First-person only"},
		{kind: filterThirdPerson, label: "Third-person (default)"},
		{kind: filterDaytime, label: "Daytime (6:00-18:00)"},
		{kind: filterNighttime, label: "Nighttime (<6 or >18)"},
		{kind: filterBattleEye, label: "BattlEye enabled"},
		{kind: filterNoBattleEye, label: "BattlEye disabled"},
		{kind: filterHasPlayers, label: "Has players"},
		{kind: filterNoPlayers, label: "Empty server"},
		{kind: filterNotFull, label: "Not full"},
		{kind: filterFull, label: "Full"},
		{kind: filterPlayersAbovePct, label: "Players > limit %"},
		{kind: filterPlayersBelowPct, label: "Players < limit %"},
		{kind: filterMinSlots, label: "Min player slots"},
		{kind: filterByMap, label: "Filter by map name…"},
		{kind: filterByModName, label: "Filter by mod name…"},
		{kind: filterByModID, label: "Filter by mod ID…"},
		{kind: filterLinux, label: "Linux servers"},
		{kind: filterWindows, label: "Windows servers"},
	}
}

// isDay returns true if the game time is between 06:00 and 18:00.
func isDay(t string) bool {
	if len(t) < 2 {
		return true
	}
	var hour int
	_, _ = fmt.Sscan(t[:2], &hour)
	return hour >= 6 && hour <= 18
}
