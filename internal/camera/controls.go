package camera

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Kazuto/pixyd/internal/v4l2"
)

var (
	// ErrNoVideo means the camera's capture node is not available.
	ErrNoVideo = errors.New("EMEET PIXY video node not found")
	// ErrUnknownControl means no control has the requested name.
	ErrUnknownControl = errors.New("unknown control")
)

// ValueError is a value a control does not accept.
type ValueError struct{ Err error }

func (e *ValueError) Error() string { return e.Err.Error() }
func (e *ValueError) Unwrap() error { return e.Err }

// Error kinds for control operations, besides ErrNotFound and ErrPermission.
const (
	ErrKindUnknownControl = "unknown-control"
	ErrKindInvalidValue   = "invalid-value"
	ErrKindRefused        = "refused" // e.g. inactive while its automatic mode is on
	ErrKindBusy           = "busy"
	ErrKindIO             = "io"
)

// Classify names the kind of a control operation's error.
func Classify(err error) string {
	var ie *v4l2.IoctlError
	var ve *ValueError
	switch {
	case errors.Is(err, ErrNoVideo):
		return ErrNotFound
	case errors.Is(err, ErrUnknownControl):
		return ErrKindUnknownControl
	case errors.As(err, &ve):
		return ErrKindInvalidValue
	case errors.As(err, &ie):
		switch ie.Errno {
		case unix.EACCES, unix.EPERM:
			return ErrKindRefused
		case unix.EBUSY:
			return ErrKindBusy
		case unix.EINVAL, unix.ERANGE:
			return ErrKindInvalidValue
		}
		return ErrKindIO
	case errors.Is(err, fs.ErrPermission):
		return ErrPermission
	case errors.Is(err, fs.ErrNotExist), isGone(err):
		return ErrNotFound
	}
	return ErrKindIO
}

// refreshControls reads the controls for subscribers only; errors are
// part of what is published.
func (c *Camera) refreshControls() { _, _ = c.Controls() }

// Controls reads every control and publishes the result.
func (c *Camera) Controls() ([]v4l2.Control, error) {
	var list []v4l2.Control
	err := c.withVideo(func(d *v4l2.Device) error {
		var err error
		list, err = c.readControlsLocked(d)
		return err
	})
	c.publishControls(list, err)
	if err != nil {
		return nil, err
	}
	return list, nil
}

// SetControl sets a control by name and returns all controls read back:
// one control can change others' flags, like auto exposure making the
// exposure time inactive.
func (c *Camera) SetControl(name string, value int64) ([]v4l2.Control, error) {
	err := c.withVideo(func(d *v4l2.Device) error {
		ctl, err := c.lookupLocked(d, name)
		if err != nil {
			return err
		}
		v, err := ctl.Coerce(value)
		if err != nil {
			return &ValueError{err}
		}
		err = d.Set(ctl.ID, v)
		if ctl.Inactive() && errors.Is(err, unix.EACCES) {
			return fmt.Errorf("%s is inactive while its automatic mode is on: %w", name, err)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	c.scheduleFollowUp()
	return c.Controls()
}

// ResetControls sets the named controls to their defaults. Manual values
// go first, while they are still active, then the automatic switches and
// menus, then manual values the switches just made active.
func (c *Camera) ResetControls(names []string) ([]v4l2.Control, error) {
	err := c.withVideo(func(d *v4l2.Device) error {
		var ctls []v4l2.Control
		for _, name := range names {
			ctl, err := c.lookupLocked(d, name)
			if err != nil {
				return err
			}
			if ctl.Settable() && ctl.Type != "button" {
				ctls = append(ctls, ctl)
			}
		}
		var errs []error
		set := func(ctl v4l2.Control) {
			if err := d.Set(ctl.ID, ctl.Default); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", ctl.Name, err))
			}
		}
		var later []v4l2.Control
		for _, ctl := range ctls {
			switch {
			case !ctl.IsInteger():
			case ctl.Inactive():
				later = append(later, ctl)
			default:
				set(ctl)
			}
		}
		for _, ctl := range ctls {
			if !ctl.IsInteger() {
				set(ctl)
			}
		}
		for _, ctl := range later {
			if now, err := d.Control(ctl.ID); err == nil && !now.Inactive() {
				set(now)
			}
		}
		return errors.Join(errs...)
	})
	if err != nil {
		c.refreshControls() // some resets may have landed
		return nil, err
	}
	c.scheduleFollowUp()
	return c.Controls()
}

func (c *Camera) readControlsLocked(d *v4l2.Device) ([]v4l2.Control, error) {
	list, err := d.Controls()
	if err != nil {
		return nil, err
	}
	c.names = make(map[string]uint32, len(list))
	for _, ctl := range list {
		c.names[ctl.Name] = ctl.ID
	}
	return list, nil
}

// lookupLocked returns a control by name with fresh flags and range.
func (c *Camera) lookupLocked(d *v4l2.Device, name string) (v4l2.Control, error) {
	id, ok := c.names[name]
	if !ok {
		if _, err := c.readControlsLocked(d); err != nil {
			return v4l2.Control{}, err
		}
		if id, ok = c.names[name]; !ok {
			return v4l2.Control{}, fmt.Errorf("%w %q", ErrUnknownControl, name)
		}
	}
	return d.Control(id)
}

// withVideo runs fn on the capture node, opening it if needed. The node
// stays open while in use and is closed after VideoIdle.
func (c *Camera) withVideo(fn func(*v4l2.Device) error) error {
	c.mu.RLock()
	path := c.capture
	c.mu.RUnlock()
	if path == "" {
		return ErrNoVideo
	}

	c.vidMu.Lock()
	defer c.vidMu.Unlock()
	if c.video != nil && c.video.Path() != path {
		c.closeVideoLocked()
	}
	if c.video == nil {
		d, err := v4l2.Open(path)
		if err != nil {
			return err
		}
		c.video = d
	}
	err := fn(c.video)
	if err != nil && isGone(err) {
		c.closeVideoLocked()
	}
	if c.videoTimer == nil {
		c.videoTimer = time.AfterFunc(c.opts.VideoIdle, c.closeIdleVideo)
	} else {
		c.videoTimer.Reset(c.opts.VideoIdle)
	}
	return err
}

func (c *Camera) closeIdleVideo() {
	c.vidMu.Lock()
	defer c.vidMu.Unlock()
	c.closeVideoLocked()
}

func (c *Camera) closeVideoLocked() {
	if c.video != nil {
		c.video.Close()
		c.video = nil
	}
}

// scheduleFollowUp reads the controls again once moving parts settled.
func (c *Camera) scheduleFollowUp() {
	c.vidMu.Lock()
	defer c.vidMu.Unlock()
	if c.followUp == nil {
		c.followUp = time.AfterFunc(c.opts.ControlsSettle, c.refreshControls)
	} else {
		c.followUp.Reset(c.opts.ControlsSettle)
	}
}

func (c *Camera) publishControls(list []v4l2.Control, err error) {
	var body any
	if err != nil {
		body = struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}{Classify(err), err.Error()}
	} else {
		if list == nil {
			list = []v4l2.Control{}
		}
		body = struct {
			Controls []v4l2.Control `json:"controls"`
		}{list}
	}
	data, mErr := json.Marshal(body)
	if mErr != nil {
		c.log.Error("marshal controls", "err", mErr)
		return
	}
	c.hub.publish("controls", data)
}

// isGone reports errors of a device that went away.
func isGone(err error) bool {
	for _, errno := range []unix.Errno{unix.ENODEV, unix.ENXIO, unix.EIO, unix.ENOENT, unix.EBADF} {
		if errors.Is(err, errno) {
			return true
		}
	}
	return false
}
