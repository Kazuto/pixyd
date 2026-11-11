package hid

import "fmt"

// Mode is the camera's control mode as the mode query reports it.
type Mode byte

const (
	Standard Mode = 0x00
	Tracking Mode = 0x01
	// Privacy physically closes the lens.
	Privacy Mode = 0x02
	// Ambiguous is what an idle camera, one no app is streaming from,
	// answers for both Standard and Tracking. Privacy is always reported.
	Ambiguous Mode = 0x03
)

// String returns the mode's name as used in the API.
func (m Mode) String() string {
	switch m {
	case Standard:
		return "standard"
	case Tracking:
		return "tracking"
	case Privacy:
		return "privacy"
	case Ambiguous:
		return "idle"
	}
	return fmt.Sprintf("0x%02x", byte(m))
}

// Settable reports whether m is a mode that can be set.
func (m Mode) Settable() bool { return m <= Privacy }

// ParseMode parses "standard", "tracking" or "privacy".
func ParseMode(s string) (Mode, error) {
	for _, m := range []Mode{Standard, Tracking, Privacy} {
		if s == m.String() {
			return m, nil
		}
	}
	return 0, fmt.Errorf("unknown mode %q (want standard, tracking or privacy)", s)
}

// Landed reports whether reading back got after setting target means the
// switch worked as far as the camera can tell. An idle camera answers
// Ambiguous for standard and tracking but always reports privacy, so
// Ambiguous counts for the former and not for the latter.
func Landed(target, got Mode) bool {
	return got == target || (got == Ambiguous && target != Privacy)
}

// modeValue is the reply byte that holds the mode.
const modeValue = 8

var modeQuery = mustBuild(0x01, 0x01, 0x01)

func modeSet(m Mode) []byte { return mustBuild(0x01, 0x01, 0x00, byte(m)) }

// ReadMode queries the camera's mode. The result may be Ambiguous, or an
// unknown value a different firmware reports.
func (c *Conn) ReadMode() (Mode, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.readMode()
}

func (c *Conn) readMode() (Mode, error) {
	reply, err := c.query(modeQuery)
	if err != nil {
		return 0, err
	}
	return Mode(reply[modeValue]), nil
}

// SetMode switches the camera to target and returns the mode it reports
// afterwards; check the result with Landed. It reads the mode first (which
// also absorbs the write a fresh descriptor drops), leaves privacy through
// standard when going to tracking, and repeats a set that did not land
// once, since the first set after the camera was idle can be dropped.
func (c *Conn) SetMode(target Mode) (Mode, error) {
	if !target.Settable() {
		return 0, fmt.Errorf("hid: mode %v cannot be set", target)
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	current, err := c.readMode()
	if err != nil {
		return 0, err
	}
	if current == target {
		return current, nil
	}
	if target == Tracking && current == Privacy {
		if _, err := c.writeMode(Standard); err != nil {
			return 0, err
		}
		c.sleep(c.timing.PrivacyExit)
	}
	got, err := c.writeMode(target)
	if err != nil || Landed(target, got) {
		return got, err
	}
	c.sleep(c.timing.Retry)
	return c.writeMode(target)
}

// writeMode sends the set report and then the mode query, which is what
// makes the camera commit the change, and reads the result back.
func (c *Conn) writeMode(m Mode) (Mode, error) {
	if err := c.t.Write(modeSet(m)); err != nil {
		return 0, err
	}
	c.sleep(c.timing.Gap)
	if err := c.t.Write(modeQuery); err != nil {
		return 0, err
	}
	c.sleep(c.timing.Settle)
	return c.readMode()
}
