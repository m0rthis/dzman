// Package a2s implements the Valve Source A2S_INFO server query over UDP.
// DayZ servers answer this on their Steam query port, which gives us a real
// ping measurement plus live server state without any external tools.
// Protocol reference: https://developer.valvesoftware.com/wiki/Server_queries
package a2s

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"time"
)

const (
	requestInfo    = 0x54 // 'T'
	replyInfo      = 0x49 // 'I'
	replyChallenge = 0x41 // 'A'

	// edf* are A2S_INFO Extra Data Flag bits.
	edfPort      = 0x80
	edfSteamID   = 0x10
	edfSourceTV  = 0x40
	edfKeywords  = 0x20
	edfGameID    = 0x01
)

var infoRequest = append(
	[]byte{0xFF, 0xFF, 0xFF, 0xFF, requestInfo},
	[]byte("Source Engine Query\x00")...,
)

// Info is a parsed A2S_INFO reply plus the measured round-trip time.
type Info struct {
	Name        string
	Map         string
	Folder      string
	Game        string
	Players     int
	MaxPlayers  int
	Bots        int
	Environment byte // 'l' linux, 'w' windows, 'm' mac
	Password    bool
	VAC         bool
	Version     string
	Keywords    string
	GamePort    int
	Ping        time.Duration
}

// Query sends an A2S_INFO request to ip:queryPort and parses the reply,
// handling the challenge handshake newer servers require.
func Query(ip string, queryPort int, timeout time.Duration) (*Info, error) {
	addr := net.JoinHostPort(ip, fmt.Sprint(queryPort))
	conn, err := net.Dial("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("a2s dial %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return nil, fmt.Errorf("a2s deadline: %w", err)
	}

	buf := make([]byte, 4096)
	request := infoRequest
	// Normally two rounds (plain request, then request + challenge token), but
	// some servers re-issue a fresh challenge, so allow a couple of retries.
	for range 3 {
		start := time.Now()
		if _, err := conn.Write(request); err != nil {
			return nil, fmt.Errorf("a2s send %s: %w", addr, err)
		}
		n, err := conn.Read(buf)
		if err != nil {
			return nil, fmt.Errorf("a2s recv %s: %w", addr, err)
		}
		rtt := time.Since(start)

		if n < 5 || !bytes.Equal(buf[:4], []byte{0xFF, 0xFF, 0xFF, 0xFF}) {
			return nil, fmt.Errorf("a2s %s: malformed reply", addr)
		}
		switch buf[4] {
		case replyChallenge:
			if n < 9 {
				return nil, fmt.Errorf("a2s %s: short challenge reply", addr)
			}
			request = append(append([]byte{}, infoRequest...), buf[5:9]...)
		case replyInfo:
			info, err := parseInfo(buf[5:n])
			if err != nil {
				return nil, fmt.Errorf("a2s %s: %w", addr, err)
			}
			info.Ping = rtt
			return info, nil
		default:
			return nil, fmt.Errorf("a2s %s: unexpected reply type 0x%02x", addr, buf[4])
		}
	}
	return nil, fmt.Errorf("a2s %s: challenge handshake did not resolve", addr)
}

// parseInfo decodes the A2S_INFO payload (everything after the type byte).
func parseInfo(data []byte) (*Info, error) {
	r := &reader{data: data}
	info := &Info{}

	r.byte() // protocol version, unused
	info.Name = r.string()
	info.Map = r.string()
	info.Folder = r.string()
	info.Game = r.string()
	r.uint16() // steam app id, unused
	info.Players = int(r.byte())
	info.MaxPlayers = int(r.byte())
	info.Bots = int(r.byte())
	r.byte() // server type, unused
	info.Environment = r.byte()
	info.Password = r.byte() == 1
	info.VAC = r.byte() == 1
	info.Version = r.string()
	if r.err != nil {
		return nil, fmt.Errorf("truncated info reply")
	}

	// Extra Data Flag section is optional.
	if r.remaining() == 0 {
		return info, nil
	}
	edf := r.byte()
	if edf&edfPort != 0 {
		info.GamePort = int(r.uint16())
	}
	if edf&edfSteamID != 0 {
		r.skip(8)
	}
	if edf&edfSourceTV != 0 {
		r.uint16()
		r.string()
	}
	if edf&edfKeywords != 0 {
		info.Keywords = r.string()
	}
	if edf&edfGameID != 0 {
		r.skip(8)
	}
	if r.err != nil {
		return nil, fmt.Errorf("truncated extra data")
	}
	return info, nil
}

// reader is a tiny cursor over the reply payload that latches the first error.
type reader struct {
	data []byte
	pos  int
	err  error
}

func (r *reader) remaining() int { return len(r.data) - r.pos }

func (r *reader) byte() byte {
	if r.err != nil || r.pos >= len(r.data) {
		r.err = fmt.Errorf("read past end")
		return 0
	}
	b := r.data[r.pos]
	r.pos++
	return b
}

func (r *reader) uint16() uint16 {
	if r.err != nil || r.pos+2 > len(r.data) {
		r.err = fmt.Errorf("read past end")
		return 0
	}
	v := binary.LittleEndian.Uint16(r.data[r.pos:])
	r.pos += 2
	return v
}

func (r *reader) string() string {
	if r.err != nil {
		return ""
	}
	end := bytes.IndexByte(r.data[r.pos:], 0)
	if end < 0 {
		r.err = fmt.Errorf("unterminated string")
		return ""
	}
	s := string(r.data[r.pos : r.pos+end])
	r.pos += end + 1
	return s
}

func (r *reader) skip(n int) {
	if r.err != nil || r.pos+n > len(r.data) {
		r.err = fmt.Errorf("read past end")
		return
	}
	r.pos += n
}
