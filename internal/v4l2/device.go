package v4l2

import (
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Device is an open video node. Opening one does not take the stream:
// controls work while another app streams from the camera.
type Device struct {
	fd   int
	path string
}

// Open opens a video node. Errors are *os.PathError.
func Open(path string) (*Device, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return &Device{fd: fd, path: path}, nil
}

// Path returns the node's path.
func (d *Device) Path() string { return d.path }

// Close closes the node.
func (d *Device) Close() error {
	if d.fd < 0 {
		return nil
	}
	err := unix.Close(d.fd)
	d.fd = -1
	return err
}

// Capability describes a video node.
type Capability struct {
	Driver  string
	Card    string // e.g. "EMEET PIXY: EMEET PIXY"
	BusInfo string
	// DeviceCaps are the capabilities of this node. The device-wide
	// "capabilities" field would make a metadata node look like a capture
	// node, so it is not exposed.
	DeviceCaps uint32
}

// IsCapture reports whether the node captures video (as opposed to the
// metadata node a UVC camera also has).
func (c Capability) IsCapture() bool { return c.DeviceCaps&capVideoCapture != 0 }

// QueryCap runs VIDIOC_QUERYCAP.
func (d *Device) QueryCap() (Capability, error) {
	var c capability
	if err := d.ioctl(vidiocQueryCap, unsafe.Pointer(&c)); err != nil {
		return Capability{}, err
	}
	return Capability{
		Driver:     cString(c.Driver[:]),
		Card:       cString(c.Card[:]),
		BusInfo:    cString(c.BusInfo[:]),
		DeviceCaps: c.DeviceCaps,
	}, nil
}
