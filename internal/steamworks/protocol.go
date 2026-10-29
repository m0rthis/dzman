package steamworks

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
)

// The helper reports to its parent as JSON lines on stdout, one Event each.

// EventType names an Event.
type EventType string

const (
	// EventStart: Steam is initialised and the helper is working on ID.
	EventStart EventType = "start"
	// EventProgress: Downloaded of Total bytes for ID, State is EItemState.
	EventProgress EventType = "progress"
	// EventDone: ID is installed in Folder.
	EventDone EventType = "done"
	// EventSkipped: ID was left alone, Error says why (see NotSubscribedMsg).
	EventSkipped EventType = "skipped"
	// EventError: ID failed with Error.
	EventError EventType = "error"
	// EventFatal: the helper could not run at all (e.g. init failed).
	EventFatal EventType = "fatal"
	// EventSummary: the last event, OK, Failed and Skipped are counts.
	EventSummary EventType = "summary"
)

// Event is one line of helper output.
type Event struct {
	Type       EventType `json:"type"`
	ID         string    `json:"id,omitempty"`
	State      uint32    `json:"state,omitempty"`
	Downloaded uint64    `json:"downloaded,omitempty"`
	Total      uint64    `json:"total,omitempty"`
	Folder     string    `json:"folder,omitempty"`
	Error      string    `json:"error,omitempty"`
	OK         int       `json:"ok,omitempty"`
	Failed     int       `json:"failed,omitempty"`
	Skipped    int       `json:"skipped,omitempty"`
}

// Encoder writes Events as JSON lines. Each event is a single Write, so
// lines stay whole on a pipe. It is safe for concurrent use.
type Encoder struct {
	mu sync.Mutex
	w  io.Writer
}

// NewEncoder returns an Encoder writing to w.
func NewEncoder(w io.Writer) *Encoder { return &Encoder{w: w} }

// Encode writes ev followed by a newline.
func (e *Encoder) Encode(ev Event) error {
	data, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("encode event: %w", err)
	}
	data = append(data, '\n')
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.w.Write(data); err != nil {
		return fmt.Errorf("write event: %w", err)
	}
	return nil
}

// maxLine bounds one protocol line; real events are a few hundred bytes.
const maxLine = 1 << 20

// Decode reads JSON-line Events from r and calls fn for each, until EOF.
// Lines over maxLine are discarded. Lines that are not a JSON object with a "type" are skipped, since
// libsteam_api.so may print its own chatter to the same stream.
func Decode(r io.Reader, fn func(Event)) error {
	return decode(r, func(ev Event) bool { fn(ev); return true })
}

// decode is Decode, stopping early once fn returns false.
func decode(r io.Reader, fn func(Event) bool) error {
	br := bufio.NewReaderSize(r, 4096)
	for {
		line, err := readLine(br)
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "{") {
			var ev Event
			if json.Unmarshal([]byte(t), &ev) == nil && ev.Type != "" && !fn(ev) {
				return nil
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read helper output: %w", err)
		}
	}
}

// readLine returns the next line without its newline. A line longer than
// maxLine comes back as "" with the rest of it discarded, so one oversized
// line of library chatter does not end decoding. At end of input it returns
// what was read and io.EOF.
func readLine(br *bufio.Reader) (string, error) {
	var buf []byte
	tooLong := false
	for {
		chunk, err := br.ReadSlice('\n')
		if !tooLong {
			if len(buf)+len(chunk) > maxLine {
				tooLong, buf = true, nil
			} else {
				buf = append(buf, chunk...)
			}
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if tooLong {
			return "", err
		}
		return string(buf), err
	}
}
