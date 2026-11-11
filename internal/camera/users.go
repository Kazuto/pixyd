package camera

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Process is a process that has the camera's video node open.
type Process struct {
	PID  int    `json:"pid"`
	Name string `json:"name"`
}

// openers lists the processes below procRoot that have node open, except
// self. Only processes of the same user are visible, which covers the apps
// one would want to name.
func openers(procRoot, node string, self int) []Process {
	out := []Process{}
	if node == "" {
		return out
	}
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return out
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		dir := filepath.Join(procRoot, e.Name())
		fds, err := os.ReadDir(filepath.Join(dir, "fd"))
		if err != nil {
			continue
		}
		for _, fd := range fds {
			if target, err := os.Readlink(filepath.Join(dir, "fd", fd.Name())); err == nil && target == node {
				comm, _ := os.ReadFile(filepath.Join(dir, "comm"))
				out = append(out, Process{PID: pid, Name: strings.TrimSpace(string(comm))})
				break
			}
		}
	}
	slices.SortFunc(out, func(a, b Process) int { return a.PID - b.PID })
	return out
}

// watcher reports opens and closes of a file through inotify.
type watcher struct {
	f      *os.File
	events chan struct{}
}

func watch(path string) (*watcher, error) {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return nil, err
	}
	mask := uint32(unix.IN_OPEN | unix.IN_CLOSE_WRITE | unix.IN_CLOSE_NOWRITE | unix.IN_DELETE_SELF)
	if _, err := unix.InotifyAddWatch(fd, path, mask); err != nil {
		unix.Close(fd)
		return nil, &os.PathError{Op: "inotify_add_watch", Path: path, Err: err}
	}
	w := &watcher{f: os.NewFile(uintptr(fd), "inotify"), events: make(chan struct{}, 1)}
	go w.loop()
	return w, nil
}

func (w *watcher) loop() {
	buf := make([]byte, 4096)
	for {
		if _, err := w.f.Read(buf); err != nil {
			if errors.Is(err, os.ErrClosed) {
				return
			}
			if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) {
				continue
			}
			return
		}
		select {
		case w.events <- struct{}{}:
		default: // one pending notification is enough
		}
	}
}

func (w *watcher) Close() error { return w.f.Close() }
