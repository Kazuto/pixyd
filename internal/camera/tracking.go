package camera

import (
	"encoding/json"

	"github.com/Kazuto/pixyd/internal/hid"
)

// Error kinds, as pixy-track reports them.
const (
	ErrNotFound    = "not-found"
	ErrPermission  = "permission"
	ErrNoAnswer    = "no-answer"
	ErrNotSwitched = "not-switched"
)

// Tracking is the camera's mode in the shape `pixy-track --json` prints:
//
//	{"mode":"tracking","confirmed":true,"idle":false}
//	{"error":"permission","message":"…"}
//
// plus "pending" while a mode waits for the camera to start streaming.
type Tracking struct {
	// Mode is "standard", "tracking" or "privacy": what the camera reports,
	// or while it is idle, the best guess (pending or last confirmed
	// mode). Empty if unknown.
	Mode string
	// Confirmed means the camera itself reported Mode just now.
	Confirmed bool
	// Idle means no app streams video, so the camera can't tell standard
	// and tracking apart (and may ignore switching between them).
	Idle bool
	// Pending is a mode requested while the camera was idle; it is applied
	// once the camera reports a mode again.
	Pending string

	Error   string
	Message string
	// Failed means the camera could not be asked at all; the JSON then
	// carries only error and message.
	Failed bool
}

func failure(kind, message string) Tracking {
	return Tracking{Failed: true, Error: kind, Message: message}
}

// MarshalJSON writes pixy-track's shape.
func (t Tracking) MarshalJSON() ([]byte, error) {
	if t.Failed {
		return json.Marshal(struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}{t.Error, t.Message})
	}
	var mode *string
	if t.Mode != "" {
		mode = &t.Mode
	}
	return json.Marshal(struct {
		Mode      *string `json:"mode"`
		Confirmed bool    `json:"confirmed"`
		Idle      bool    `json:"idle"`
		Pending   string  `json:"pending,omitempty"`
		Error     string  `json:"error,omitempty"`
		Message   string  `json:"message,omitempty"`
	}{mode, t.Confirmed, t.Idle, t.Pending, t.Error, t.Message})
}

// tracker turns mode reads and sets into a Tracking, remembering the last
// confirmed mode and a mode requested while the camera was idle.
type tracker struct {
	raw         hid.Mode
	haveRaw     bool
	last        hid.Mode
	haveLast    bool
	pending     hid.Mode
	havePending bool
}

// observe records a mode the camera reported. If a pending mode should be
// applied now, it returns it (and forgets it: it is applied once).
func (t *tracker) observe(raw hid.Mode) (hid.Mode, bool) {
	t.raw, t.haveRaw = raw, true
	if !raw.Settable() { // idle or unknown: nothing confirmed
		return 0, false
	}
	t.last, t.haveLast = raw, true
	if !t.havePending {
		return 0, false
	}
	pending := t.pending
	t.havePending = false
	// Privacy was set by someone after the request, which makes the
	// request stale; and a camera already in the pending mode is done.
	if raw == hid.Privacy || raw == pending {
		return 0, false
	}
	return pending, true
}

// sent records that target was set and the camera then reported got, and
// returns whether it landed. A standard/tracking switch on an idle camera
// can't be confirmed and may have been ignored, so it stays pending.
func (t *tracker) sent(target, got hid.Mode) bool {
	t.havePending = false
	t.observe(got)
	landed := hid.Landed(target, got)
	if landed && got == hid.Ambiguous {
		t.pending, t.havePending = target, true
	}
	return landed
}

// current is the best guess at the camera's mode.
func (t *tracker) current() (hid.Mode, bool) {
	switch {
	case t.haveRaw && t.raw.Settable():
		return t.raw, true
	case t.havePending:
		return t.pending, true
	case t.haveLast:
		return t.last, true
	}
	return 0, false
}

// forget drops what was read from a camera that went away. The last
// confirmed mode stays as a guess for when it comes back.
func (t *tracker) forget() {
	t.haveRaw = false
	t.havePending = false
}

func (t *tracker) view() Tracking {
	v := Tracking{
		Confirmed: t.haveRaw && t.raw.Settable(),
		Idle:      t.haveRaw && t.raw == hid.Ambiguous,
	}
	if m, ok := t.current(); ok {
		v.Mode = m.String()
	}
	if t.havePending {
		v.Pending = t.pending.String()
	}
	return v
}

// commandTarget picks the mode for a pixy-track style command: "on",
// "off" or "toggle" (tracking unless tracking is on).
func commandTarget(cmd string, current hid.Mode, known bool) (hid.Mode, bool) {
	switch cmd {
	case "on":
		return hid.Tracking, true
	case "off":
		return hid.Standard, true
	case "toggle":
		if known && current == hid.Tracking {
			return hid.Standard, true
		}
		return hid.Tracking, true
	}
	return 0, false
}
