package hid

import (
	"errors"
	"io"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// hidraw is a Transport on a /dev/hidrawN node.
type hidraw struct {
	fd   int
	path string
}

// Open opens a hidraw node for reading and writing. Errors are
// *os.PathError, so os.IsPermission and errors.Is(err, fs.ErrNotExist)
// work on them.
func Open(path string) (Transport, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return &hidraw{fd: fd, path: path}, nil
}

func (h *hidraw) Write(report []byte) error {
	for {
		_, err := unix.Write(h.fd, report)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return &os.PathError{Op: "write", Path: h.path, Err: err}
		}
		return nil
	}
}

func (h *hidraw) ReadReport(buf []byte, deadline time.Time) (int, error) {
	for {
		n, err := unix.Read(h.fd, buf)
		switch {
		case err == nil && n == 0:
			return 0, &os.PathError{Op: "read", Path: h.path, Err: io.EOF}
		case err == nil:
			return n, nil
		case errors.Is(err, unix.EINTR):
			continue
		case !errors.Is(err, unix.EAGAIN):
			return 0, &os.PathError{Op: "read", Path: h.path, Err: err}
		}

		wait := time.Until(deadline)
		if wait <= 0 {
			return 0, os.ErrDeadlineExceeded
		}
		fds := []unix.PollFd{{Fd: int32(h.fd), Events: unix.POLLIN}}
		ms := int((wait + time.Millisecond - 1) / time.Millisecond)
		if _, err := unix.Poll(fds, ms); err != nil && !errors.Is(err, unix.EINTR) {
			return 0, &os.PathError{Op: "poll", Path: h.path, Err: err}
		}
		// After an unplug, poll reports a hangup instead of input.
		if r := fds[0].Revents; r&unix.POLLIN == 0 && r&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
			return 0, &os.PathError{Op: "poll", Path: h.path, Err: unix.ENODEV}
		}
	}
}

func (h *hidraw) Close() error {
	if h.fd < 0 {
		return nil
	}
	err := unix.Close(h.fd)
	h.fd = -1
	return err
}
