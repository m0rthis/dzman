package steam

import (
	"os"
	"path/filepath"
	"strings"
)

// The Steam client keeps per-app state in ~/.steam/registry.vdf (Flatpak
// Steam: ~/.var/app/com.valvesoftware.Steam/.steam/registry.vdf), including
// whether it counts a game as running:
//
//	"Registry" { "HKCU" { "Software" { "Valve" { "Steam" {
//		"Apps" { "221100" { "Running" "1" ... } } } } } } }
//
// The key path and the "Running" value are from community notes and could
// not be checked against a real client here, so a missing file or key means
// "unknown", never "not running", and keys match case-insensitively.

// registryAppPath is the key path to an app's entry, before its ID.
var registryAppPath = []string{"registry", "hkcu", "software", "valve", "steam", "apps"}

// RegistryFiles returns the registry.vdf files the Steam client may use.
func RegistryFiles() []string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	return []string{
		filepath.Join(home, ".steam", "registry.vdf"),
		filepath.Join(home, flatpakSteamApp, ".steam", "registry.vdf"),
	}
}

// AppRunning reports what the registry files say about appID: running is
// true when any of them has its "Running" value set to "1", and known is
// false when none of them has the value at all.
func AppRunning(files []string, appID string) (running, known bool) {
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		v, ok := registryValue(parseVDF(string(data)), append(append([]string{}, registryAppPath...), appID, "running"))
		if !ok {
			continue
		}
		known = true
		if strings.TrimSpace(v) == "1" {
			return true, true
		}
	}
	return false, known
}

// vdfNode is a parsed KeyValues block. Keys are lowercased; a repeated key
// keeps its first value.
type vdfNode struct {
	values map[string]string
	blocks map[string]*vdfNode
}

func newVDFNode() *vdfNode {
	return &vdfNode{values: map[string]string{}, blocks: map[string]*vdfNode{}}
}

func registryValue(n *vdfNode, path []string) (string, bool) {
	for _, k := range path[:len(path)-1] {
		if n = n.blocks[k]; n == nil {
			return "", false
		}
	}
	v, ok := n.values[path[len(path)-1]]
	return v, ok
}

// parseVDF parses Valve's text KeyValues format tolerantly: quoted or bare
// tokens, // comments, and [$CONDITION] tags are accepted, and anything
// malformed ends the block it is in instead of failing the whole file.
func parseVDF(s string) *vdfNode {
	toks := vdfTokens(s)
	root := newVDFNode()
	parseVDFBlock(root, toks, 0)
	return root
}

// vdfToken is a string (quoted or bare) or one of the braces.
type vdfToken struct {
	text  string
	brace byte // '{', '}' or 0 for a string
}

func parseVDFBlock(n *vdfNode, toks []vdfToken, i int) int {
	for i < len(toks) {
		t := toks[i]
		if t.brace == '}' {
			return i + 1
		}
		if t.brace == '{' { // a block with no key; skip it
			i = parseVDFBlock(newVDFNode(), toks, i+1)
			continue
		}
		key := strings.ToLower(t.text)
		i++
		if i >= len(toks) {
			return i
		}
		switch next := toks[i]; next.brace {
		case '{':
			child := newVDFNode()
			i = parseVDFBlock(child, toks, i+1)
			if _, dup := n.blocks[key]; !dup {
				n.blocks[key] = child
			}
		case '}':
			return i + 1
		default:
			if _, dup := n.values[key]; !dup {
				n.values[key] = next.text
			}
			i++
		}
	}
	return i
}

func vdfTokens(s string) []vdfToken {
	var out []vdfToken
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			i++
		case c == '/' && i+1 < len(s) && s[i+1] == '/':
			for i < len(s) && s[i] != '\n' {
				i++
			}
		case c == '{' || c == '}':
			out = append(out, vdfToken{brace: c})
			i++
		case c == '[': // a conditional tag like [$WIN32]; ignored
			for i < len(s) && s[i] != ']' && s[i] != '\n' {
				i++
			}
			i++
		case c == '"':
			var b strings.Builder
			i++
			for i < len(s) && s[i] != '"' {
				if s[i] == '\\' && i+1 < len(s) {
					i++
					switch s[i] {
					case 'n':
						b.WriteByte('\n')
					case 't':
						b.WriteByte('\t')
					default:
						b.WriteByte(s[i])
					}
				} else {
					b.WriteByte(s[i])
				}
				i++
			}
			i++ // closing quote (or end of input)
			out = append(out, vdfToken{text: b.String()})
		default:
			j := i
			for j < len(s) && !strings.ContainsRune(" \t\r\n{}\"", rune(s[j])) {
				j++
			}
			out = append(out, vdfToken{text: s[i:j]})
			i = j
		}
	}
	return out
}
