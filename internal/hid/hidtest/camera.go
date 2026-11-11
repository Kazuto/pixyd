// Package hidtest simulates the EMEET PIXY's vendor HID interface, including
// the quirks observed on real hardware, so the protocol code can be tested
// without a camera.
package hidtest

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/Kazuto/pixyd/internal/hid"
)

// Camera is a simulated camera. Its quirks are switched on by New and can be
// turned off individually.
type Camera struct {
	// DropFirstWrite drops the first write on every new Transport.
	DropFirstWrite bool
	// DropColdSet drops the first mode set after Rest.
	DropColdSet bool
	// DiscardIdleWrites loses standard/tracking writes while nothing streams
	// (privacy still works, in both directions).
	DiscardIdleWrites bool
	// IgnorePrivacyToTracking ignores a direct privacy → tracking switch.
	IgnorePrivacyToTracking bool
	// Silent stops the camera from answering anything.
	Silent bool
	// IgnoreSets acknowledges mode sets but never applies them.
	IgnoreSets bool

	mu          sync.Mutex
	base        hid.Mode // standard or tracking; kept while in privacy
	privacy     bool
	streaming   bool
	cold        bool
	staged      *hid.Mode
	unplugged   bool
	writes      [][]byte
	unsolicited [][]byte
}

// New returns a camera in mode m (standard, tracking or privacy) with every
// quirk enabled. streaming says whether an app is streaming video from it.
func New(m hid.Mode, streaming bool) *Camera {
	c := &Camera{
		DropFirstWrite:          true,
		DropColdSet:             true,
		DiscardIdleWrites:       true,
		IgnorePrivacyToTracking: true,
		base:                    hid.Standard,
		streaming:               streaming,
	}
	if m == hid.Privacy {
		c.privacy = true
	} else {
		c.base = m
	}
	return c
}

// Open returns a new descriptor on the camera.
func (c *Camera) Open() *Transport { return &Transport{cam: c} }

// Mode returns the mode the camera is really in.
func (c *Camera) Mode() hid.Mode {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.privacy {
		return hid.Privacy
	}
	return c.base
}

// SetStreaming starts or stops the simulated video stream.
func (c *Camera) SetStreaming(on bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.streaming = on
}

// Rest simulates a period of inactivity: the next mode set is dropped.
func (c *Camera) Rest() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cold = true
}

// Unplug makes every descriptor fail like a disconnected device.
func (c *Camera) Unplug() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.unplugged = true
}

// Inject queues a report every descriptor receives before its next reply,
// like the unsolicited status reports the camera sends.
func (c *Camera) Inject(report []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.unsolicited = append(c.unsolicited, append([]byte(nil), report...))
}

// Writes returns every report written so far, dropped ones included.
func (c *Camera) Writes() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]byte(nil), c.writes...)
}

// ModeSets returns the modes of every mode set report written so far.
func (c *Camera) ModeSets() []hid.Mode {
	var sets []hid.Mode
	for _, w := range c.Writes() {
		if isModeSet(w) {
			sets = append(sets, hid.Mode(w[8]))
		}
	}
	return sets
}

func isModeSet(r []byte) bool   { return r[1] == 0x01 && r[2] == 0x01 && r[3] == 0x00 && r[5] == 1 }
func isModeQuery(r []byte) bool { return r[1] == 0x01 && r[2] == 0x01 && r[3] == 0x01 }

// handle processes one report that reached the camera and returns replies.
func (c *Camera) handle(r []byte) [][]byte {
	switch {
	case isModeSet(r):
		if c.DropColdSet && c.cold {
			c.cold = false
			return nil
		}
		m := hid.Mode(r[8])
		c.staged = &m
		return [][]byte{reply(r, 0x20)} // sets are acknowledged with 0x20
	case isModeQuery(r):
		if c.staged != nil {
			c.commit(*c.staged)
			c.staged = nil
		}
		return [][]byte{reply(r, byte(c.reported()))}
	}
	return nil
}

func (c *Camera) commit(m hid.Mode) {
	switch {
	case c.IgnoreSets:
	case m == hid.Privacy:
		c.privacy = true
	case c.privacy && m == hid.Tracking && c.IgnorePrivacyToTracking:
		// Stays in privacy.
	default:
		c.privacy = false // leaving privacy works even while idle
		if c.streaming || !c.DiscardIdleWrites {
			c.base = m
		}
	}
}

func (c *Camera) reported() hid.Mode {
	switch {
	case c.privacy:
		return hid.Privacy
	case !c.streaming:
		return hid.Ambiguous
	}
	return c.base
}

// reply builds an answer to request with one value byte. The group byte gets
// high bits set, as the real camera's replies do.
func reply(request []byte, value byte) []byte {
	r, err := hid.Build(request[1]|0x40, request[2], request[3], value)
	if err != nil {
		panic(err)
	}
	return r
}

// Transport is one simulated descriptor. Reads never block: an empty queue
// returns os.ErrDeadlineExceeded at once.
type Transport struct {
	cam    *Camera
	queue  [][]byte
	wrote  bool
	closed bool
}

func (t *Transport) Write(report []byte) error {
	c := t.cam
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := t.check("write", syscall.ENODEV); err != nil {
		return err
	}
	if len(report) != hid.ReportSize || report[0] != hid.ReportID {
		return fmt.Errorf("hidtest: malformed report % x", report)
	}
	c.writes = append(c.writes, append([]byte(nil), report...))
	if c.DropFirstWrite && !t.wrote {
		t.wrote = true
		return nil
	}
	t.wrote = true
	if c.Silent {
		return nil
	}
	t.queue = append(t.queue, c.unsolicited...)
	c.unsolicited = nil
	t.queue = append(t.queue, c.handle(report)...)
	return nil
}

func (t *Transport) ReadReport(buf []byte, _ time.Time) (int, error) {
	t.cam.mu.Lock()
	defer t.cam.mu.Unlock()
	if err := t.check("read", syscall.EIO); err != nil {
		return 0, err
	}
	if len(t.queue) == 0 {
		return 0, os.ErrDeadlineExceeded
	}
	n := copy(buf, t.queue[0])
	t.queue = t.queue[1:]
	return n, nil
}

func (t *Transport) Close() error {
	t.cam.mu.Lock()
	defer t.cam.mu.Unlock()
	t.closed = true
	return nil
}

func (t *Transport) check(op string, unplugged syscall.Errno) error {
	if t.closed {
		return errors.New("hidtest: use of closed transport")
	}
	if t.cam.unplugged {
		return &os.PathError{Op: op, Path: "/dev/hidtest", Err: unplugged}
	}
	return nil
}
