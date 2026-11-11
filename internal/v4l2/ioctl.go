// Package v4l2 reads and sets V4L2 controls through raw ioctls, without cgo.
package v4l2

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Structs from <linux/videodev2.h>. Their sizes are part of the ioctl
// numbers, which the tests compare with the values the kernel headers give.

type capability struct {
	Driver       [16]byte
	Card         [32]byte
	BusInfo      [32]byte
	Version      uint32
	Capabilities uint32
	DeviceCaps   uint32
	Reserved     [3]uint32
}

type queryctrl struct {
	ID           uint32
	Type         uint32
	Name         [32]byte
	Minimum      int32
	Maximum      int32
	Step         int32
	DefaultValue int32
	Flags        uint32
	Reserved     [2]uint32
}

// querymenu is packed in C: id, index, a 32-byte union of name and __s64
// value, and a reserved word, 44 bytes.
type querymenu struct {
	ID       uint32
	Index    uint32
	Union    [32]byte
	Reserved uint32
}

type control struct {
	ID    uint32
	Value int32
}

const (
	iocWrite = 1
	iocRead  = 2
)

func ioc(dir, nr, size uintptr) uintptr {
	return dir<<30 | size<<16 | uintptr('V')<<8 | nr
}

var (
	vidiocQueryCap  = ioc(iocRead, 0, unsafe.Sizeof(capability{}))
	vidiocGCtrl     = ioc(iocRead|iocWrite, 27, unsafe.Sizeof(control{}))
	vidiocSCtrl     = ioc(iocRead|iocWrite, 28, unsafe.Sizeof(control{}))
	vidiocQueryCtrl = ioc(iocRead|iocWrite, 36, unsafe.Sizeof(queryctrl{}))
	vidiocQueryMenu = ioc(iocRead|iocWrite, 37, unsafe.Sizeof(querymenu{}))
	ioctlNames      = map[uintptr]string{
		vidiocQueryCap:  "VIDIOC_QUERYCAP",
		vidiocGCtrl:     "VIDIOC_G_CTRL",
		vidiocSCtrl:     "VIDIOC_S_CTRL",
		vidiocQueryCtrl: "VIDIOC_QUERYCTRL",
		vidiocQueryMenu: "VIDIOC_QUERYMENU",
	}
)

// capVideoCapture is the capture bit in device_caps.
const capVideoCapture = 0x00000001

// Control types.
const (
	typeInteger     = 1
	typeBoolean     = 2
	typeMenu        = 3
	typeButton      = 4
	typeInteger64   = 5
	typeCtrlClass   = 6
	typeString      = 7
	typeBitmask     = 8
	typeIntegerMenu = 9
)

// Control flags.
const (
	flagDisabled  = 0x0001
	flagGrabbed   = 0x0002
	flagReadOnly  = 0x0004
	flagUpdate    = 0x0008
	flagInactive  = 0x0010
	flagSlider    = 0x0020
	flagWriteOnly = 0x0040
	flagVolatile  = 0x0080
	flagNextCtrl  = 0x80000000
)

// IoctlError is a failed ioctl. errors.Is works with the errno, e.g.
// unix.EACCES for a control whose automatic mode is on.
type IoctlError struct {
	Op    string
	Path  string
	Errno unix.Errno
}

func (e *IoctlError) Error() string {
	return fmt.Sprintf("%s on %s: %v", e.Op, e.Path, e.Errno)
}

func (e *IoctlError) Unwrap() error { return e.Errno }

func (d *Device) ioctl(req uintptr, arg unsafe.Pointer) error {
	for {
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(d.fd), req, uintptr(arg))
		switch {
		case errno == 0:
			return nil
		case errors.Is(errno, unix.EINTR):
			continue
		}
		return &IoctlError{Op: ioctlNames[req], Path: d.path, Errno: errno}
	}
}

func cString(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}
