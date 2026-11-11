// Package camera supervises the EMEET PIXY: it finds the camera, keeps one
// HID descriptor open, polls the mode, recovers from unplugging, and
// publishes state snapshots to subscribers.
package camera

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/Kazuto/pixyd/internal/hid"
	"github.com/Kazuto/pixyd/internal/sysfs"
	"github.com/Kazuto/pixyd/internal/v4l2"
)

// Options configure a Camera. Zero values mean the defaults.
type Options struct {
	SysRoot  string // where sysfs lives below, "/"
	ProcRoot string // "/proc"
	// HIDRaw and Video replace discovery of that node. A path that does
	// not exist means no camera, not a fallback to discovery.
	HIDRaw string
	Video  string
	Poll   time.Duration // mode poll interval, 1.5 s
	// PermissionGrace tolerates EACCES on a freshly plugged camera's
	// hidraw node, since udev applies the ACL a moment after the node
	// appears. 3 s.
	PermissionGrace time.Duration
	// VideoIdle closes the video node after this long unused. 10 s.
	VideoIdle time.Duration
	// ControlsSettle is the delay of the extra controls read after a set:
	// pan/tilt read back the old position until the motors stop. 600 ms.
	ControlsSettle time.Duration
	Timing         hid.Timing
	Logger         *slog.Logger

	// OpenHID opens a hidraw node; replaceable in tests.
	OpenHID func(path string) (hid.Transport, error)
	// ProbeVideo picks the capture node among a device's video nodes and
	// returns it with its V4L2 card name; replaceable in tests.
	ProbeVideo func(nodes []string) (path, card string, err error)
}

// State is the snapshot GET /api/state returns.
type State struct {
	Connected  bool      `json:"connected"`
	Camera     *Info     `json:"camera"`
	Tracking   Tracking  `json:"tracking"`
	VideoUsers []Process `json:"videoUsers"`
}

// Info describes the connected camera.
type Info struct {
	Name   string `json:"name"` // USB product name
	Card   string `json:"card"` // V4L2 card name, which browsers use as the label
	USB    string `json:"usb"`  // USB port, e.g. "7-1.2.4"
	Speed  string `json:"speed"`
	Video  string `json:"video"` // capture node, "" if not found (yet)
	HIDRaw string `json:"hidraw"`
}

// Camera supervises one PIXY. Create it with New and start Run.
type Camera struct {
	opts Options
	log  *slog.Logger
	hub  *hub
	self int

	// hidMu serializes HID traffic and guards the fields up to vidMu.
	hidMu     sync.Mutex
	conn      *hid.Conn
	permSince time.Time
	track     tracker

	// vidMu serializes video node access and guards the fields up to mu.
	vidMu      sync.Mutex
	video      *v4l2.Device
	videoTimer *time.Timer
	names      map[string]uint32 // control name → ID
	followUp   *time.Timer

	// mu guards the snapshot. Lock order: hidMu or vidMu before mu.
	mu       sync.RWMutex
	dev      *sysfs.Device
	info     *Info
	capture  string
	tracking Tracking
	users    []Process

	// Owned by the Run goroutine.
	watch      *watcher
	warnedMany bool
}

// New returns a Camera; nothing is opened until Run.
func New(opts Options) *Camera {
	opts.SysRoot = cmp.Or(opts.SysRoot, "/")
	opts.ProcRoot = cmp.Or(opts.ProcRoot, "/proc")
	opts.Poll = cmp.Or(opts.Poll, 1500*time.Millisecond)
	opts.PermissionGrace = cmp.Or(opts.PermissionGrace, 3*time.Second)
	opts.VideoIdle = cmp.Or(opts.VideoIdle, 10*time.Second)
	opts.ControlsSettle = cmp.Or(opts.ControlsSettle, 600*time.Millisecond)
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.OpenHID == nil {
		opts.OpenHID = hid.Open
	}
	if opts.ProbeVideo == nil {
		opts.ProbeVideo = probeVideo
	}
	return &Camera{
		opts:     opts,
		log:      opts.Logger,
		hub:      newHub(),
		self:     os.Getpid(),
		tracking: failure(ErrNotFound, "EMEET PIXY not found"),
		users:    []Process{},
	}
}

// Run polls the camera until ctx is done, then closes everything.
func (c *Camera) Run(ctx context.Context) {
	ticker := time.NewTicker(c.opts.Poll)
	defer ticker.Stop()
	debounce := time.NewTimer(time.Hour)
	debounce.Stop()
	defer c.shutdown()

	c.poll()
	for {
		var opened <-chan struct{}
		if c.watch != nil {
			opened = c.watch.events
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.poll()
		case <-opened:
			// Opens come in bursts (probing, retries); scan once they settle.
			debounce.Reset(200 * time.Millisecond)
		case <-debounce.C:
			c.scanUsers()
		}
	}
}

// State returns the current snapshot.
func (c *Camera) State() State {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s := State{Connected: c.dev != nil, Tracking: c.tracking, VideoUsers: c.users}
	if c.info != nil {
		info := *c.info
		s.Camera = &info
	}
	return s
}

// Subscribe returns a subscription to "state" and "controls" snapshots,
// starting with the latest of each. Close it when done.
func (c *Camera) Subscribe() *Subscription {
	s := c.hub.subscribe()
	go c.refreshControls() // so the new subscriber gets fresh controls soon
	return s
}

// Tracking reads the mode now.
func (c *Camera) Tracking() Tracking {
	c.hidMu.Lock()
	c.refreshModeLocked()
	t := c.trackingSnapshot()
	c.hidMu.Unlock()
	c.publishState()
	return t
}

// SetMode switches the camera to target.
func (c *Camera) SetMode(target hid.Mode) Tracking {
	c.hidMu.Lock()
	t := c.setModeLocked(target)
	c.hidMu.Unlock()
	c.publishState()
	return t
}

// Command runs a pixy-track style command: "toggle", "on" or "off".
func (c *Camera) Command(cmd string) (Tracking, error) {
	c.hidMu.Lock()
	defer c.publishState()
	defer c.hidMu.Unlock()

	c.refreshModeLocked() // toggle needs the current mode
	if c.conn == nil {
		return c.trackingSnapshot(), nil
	}
	current, known := c.track.current()
	target, ok := commandTarget(cmd, current, known)
	if !ok {
		return Tracking{}, fmt.Errorf("unknown command %q (want toggle, on or off)", cmd)
	}
	return c.setModeLocked(target), nil
}

func (c *Camera) poll() {
	c.rescan()
	c.hidMu.Lock()
	c.refreshModeLocked()
	c.hidMu.Unlock()
	c.publishState()
	if c.hub.count() > 0 {
		c.refreshControls()
	}
}

// rescan follows the camera through sysfs: attaching when it appears,
// detaching when it goes away or is replugged.
func (c *Camera) rescan() {
	dev, found := c.discover()
	c.mu.RLock()
	var oldKey string
	if c.dev != nil {
		oldKey = c.dev.Key()
	}
	probed := c.capture != ""
	c.mu.RUnlock()

	var newKey string
	if found {
		newKey = dev.Key()
	}
	switch {
	case newKey != oldKey:
		if oldKey != "" {
			c.log.Info("camera disconnected")
		}
		c.detach()
		if found {
			c.attach(dev)
		}
	case found && !probed && len(dev.Video) > 0:
		c.probe(dev) // e.g. no access to the video node a moment ago
	}
}

func (c *Camera) discover() (sysfs.Device, bool) {
	o := c.opts
	if o.HIDRaw == "" && o.Video == "" {
		devices := sysfs.Find(o.SysRoot)
		if len(devices) == 0 {
			return sysfs.Device{}, false
		}
		if len(devices) > 1 && !c.warnedMany {
			c.log.Warn("several EMEET PIXY cameras found; using the first", "usb", devices[0].Port)
			c.warnedMany = true
		}
		return devices[0], true
	}

	dev, ok := sysfs.Owner(o.SysRoot, cmp.Or(o.HIDRaw, o.Video))
	if !ok {
		if all := sysfs.Find(o.SysRoot); len(all) > 0 {
			dev = all[0]
		}
	}
	if o.HIDRaw != "" {
		dev.HIDRaw = ""
		if exists(o.HIDRaw) {
			dev.HIDRaw = o.HIDRaw
		}
	}
	if o.Video != "" {
		dev.Video = nil
		if exists(o.Video) {
			dev.Video = []string{o.Video}
		}
	}
	return dev, dev.HIDRaw != "" || len(dev.Video) > 0
}

func (c *Camera) attach(dev sysfs.Device) {
	speed := dev.Speed
	if speed != "" {
		speed += "M"
	}
	c.mu.Lock()
	c.dev = &dev
	c.info = &Info{Name: dev.Product, USB: dev.Port, Speed: speed, HIDRaw: dev.HIDRaw}
	c.mu.Unlock()
	c.log.Info("camera found", "usb", dev.Port, "speed", speed, "hidraw", dev.HIDRaw, "video", dev.Video)
	c.probe(dev)
}

// probe finds the capture node and starts watching who opens it.
func (c *Camera) probe(dev sysfs.Device) {
	if len(dev.Video) == 0 {
		return
	}
	path, card, err := c.opts.ProbeVideo(dev.Video)
	if err != nil {
		c.log.Warn("no usable video node yet", "nodes", dev.Video, "err", err)
		return
	}
	c.mu.Lock()
	c.capture = path
	if c.info != nil {
		c.info.Video, c.info.Card = path, card
	}
	c.mu.Unlock()
	c.log.Info("capture node", "path", path, "card", card)

	if w, err := watch(path); err != nil {
		c.log.Warn("can't watch the capture node; video users won't update", "err", err)
	} else {
		c.watch = w
	}
	c.scanUsers()
}

func (c *Camera) detach() {
	c.hidMu.Lock()
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
	c.track.forget()
	c.permSince = time.Time{}
	c.setTrackingLocked(failure(ErrNotFound, "EMEET PIXY not found"))
	c.hidMu.Unlock()

	c.vidMu.Lock()
	c.closeVideoLocked()
	c.names = nil
	c.vidMu.Unlock()

	if c.watch != nil {
		c.watch.Close()
		c.watch = nil
	}
	c.mu.Lock()
	wasAttached := c.dev != nil
	c.dev, c.info, c.capture, c.users = nil, nil, "", []Process{}
	c.mu.Unlock()
	if wasAttached {
		c.publishControls(nil, ErrNoVideo)
	}
}

func (c *Camera) shutdown() {
	c.detach()
	c.vidMu.Lock()
	if c.videoTimer != nil {
		c.videoTimer.Stop()
	}
	if c.followUp != nil {
		c.followUp.Stop()
	}
	c.vidMu.Unlock()
}

// ensureConnLocked opens the hidraw node if it isn't open.
func (c *Camera) ensureConnLocked() bool {
	if c.conn != nil {
		return true
	}
	c.mu.RLock()
	dev := c.dev
	c.mu.RUnlock()
	switch {
	case dev == nil:
		c.setTrackingLocked(failure(ErrNotFound, "EMEET PIXY not found"))
		return false
	case dev.HIDRaw == "":
		c.setTrackingLocked(failure(ErrNotFound, "the camera's HID interface was not found"))
		return false
	}

	t, err := c.opts.OpenHID(dev.HIDRaw)
	switch {
	case err == nil:
	case errors.Is(err, fs.ErrPermission):
		if c.permSince.IsZero() {
			c.permSince = time.Now()
		}
		if time.Since(c.permSince) >= c.opts.PermissionGrace {
			c.setTrackingLocked(failure(ErrPermission, fmt.Sprintf(
				"no permission to open %s; install the udev rule from the README", dev.HIDRaw)))
		}
		return false
	default:
		c.setTrackingLocked(failure(ErrNotFound, fmt.Sprintf("can't open %s: %v", dev.HIDRaw, unwrapPath(err))))
		return false
	}
	c.permSince = time.Time{}
	c.conn = hid.NewConn(t, c.opts.Timing)
	c.log.Info("opened HID interface", "path", dev.HIDRaw)
	return true
}

// refreshModeLocked reads the mode, and applies a pending mode once the
// camera reports one again.
func (c *Camera) refreshModeLocked() {
	if !c.ensureConnLocked() {
		return
	}
	m, err := c.conn.ReadMode()
	if err != nil {
		c.hidFailedLocked(err)
		return
	}
	if apply, ok := c.track.observe(m); ok {
		c.log.Info("camera reports a mode again; applying the one requested while it was idle", "mode", apply, "camera", m)
		got, err := c.conn.SetMode(apply)
		if err != nil {
			c.hidFailedLocked(err)
			return
		}
		if !c.track.sent(apply, got) {
			c.log.Warn("pending mode did not land", "mode", apply, "camera", got)
		}
	}
	c.setTrackingLocked(c.track.view())
}

func (c *Camera) setModeLocked(target hid.Mode) Tracking {
	if !c.ensureConnLocked() {
		return c.trackingSnapshot()
	}
	got, err := c.conn.SetMode(target)
	if err != nil {
		c.hidFailedLocked(err)
		return c.trackingSnapshot()
	}
	landed := c.track.sent(target, got)
	t := c.track.view()
	c.setTrackingLocked(t)
	c.log.Info("mode set", "target", target, "camera", got, "landed", landed, "pending", t.Pending)
	if !landed {
		t.Error, t.Message = ErrNotSwitched, fmt.Sprintf("camera did not switch to %v", target)
	}
	return t
}

func (c *Camera) hidFailedLocked(err error) {
	if errors.Is(err, hid.ErrNoAnswer) {
		c.setTrackingLocked(failure(ErrNoAnswer, "camera did not answer"))
		return
	}
	// Most likely unplugged. Drop the descriptor; the next poll rescans.
	c.log.Warn("HID I/O failed", "err", err)
	c.conn.Close()
	c.conn = nil
	c.track.forget()
	c.setTrackingLocked(failure(ErrNotFound, fmt.Sprintf("camera I/O failed: %v", unwrapPath(err))))
}

func (c *Camera) setTrackingLocked(t Tracking) {
	c.mu.Lock()
	c.tracking = t
	c.mu.Unlock()
}

func (c *Camera) trackingSnapshot() Tracking {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.tracking
}

func (c *Camera) scanUsers() {
	c.mu.RLock()
	node := c.capture
	c.mu.RUnlock()
	users := openers(c.opts.ProcRoot, node, c.self)
	c.mu.Lock()
	c.users = users
	c.mu.Unlock()
	c.publishState()
}

func (c *Camera) publishState() {
	data, err := json.Marshal(c.State())
	if err != nil {
		c.log.Error("marshal state", "err", err)
		return
	}
	c.hub.publish("state", data)
}

func probeVideo(nodes []string) (string, string, error) {
	var firstErr error
	for _, path := range nodes {
		d, err := v4l2.Open(path)
		if err != nil {
			firstErr = cmp.Or(firstErr, err)
			continue
		}
		info, err := d.QueryCap()
		d.Close()
		if err != nil {
			firstErr = cmp.Or(firstErr, err)
			continue
		}
		if info.IsCapture() {
			return path, info.Card, nil
		}
	}
	return "", "", cmp.Or(firstErr, ErrNoVideo)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// unwrapPath turns "open /dev/hidraw3: no such device" into "no such
// device" where the path is already in the message.
func unwrapPath(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}
