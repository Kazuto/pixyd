package camera

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"time"

	"github.com/Kazuto/pixyd/internal/hid"
	"github.com/Kazuto/pixyd/internal/hid/hidtest"
	"github.com/Kazuto/pixyd/internal/sysfs/sysfstest"
)

var fast = hid.Timing{
	Gap:           time.Microsecond,
	QueryTimeout:  time.Millisecond,
	QueryAttempts: 4,
	Settle:        time.Microsecond,
	Retry:         time.Microsecond,
	PrivacyExit:   time.Microsecond,
}

func TestTrackingJSON(t *testing.T) {
	for _, tt := range []struct {
		name string
		in   Tracking
		want string
	}{
		{"confirmed", Tracking{Mode: "tracking", Confirmed: true}, `{"mode":"tracking","confirmed":true,"idle":false}`},
		{"idle, last known", Tracking{Mode: "standard", Idle: true}, `{"mode":"standard","confirmed":false,"idle":true}`},
		{"idle, unknown", Tracking{Idle: true}, `{"mode":null,"confirmed":false,"idle":true}`},
		{"pending", Tracking{Mode: "tracking", Idle: true, Pending: "tracking"}, `{"mode":"tracking","confirmed":false,"idle":true,"pending":"tracking"}`},
		{"not switched", Tracking{Mode: "privacy", Confirmed: true, Error: ErrNotSwitched, Message: "camera did not switch to tracking"},
			`{"mode":"privacy","confirmed":true,"idle":false,"error":"not-switched","message":"camera did not switch to tracking"}`},
		{"failure", failure(ErrPermission, "no permission"), `{"error":"permission","message":"no permission"}`},
	} {
		got, err := json.Marshal(tt.in)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != tt.want {
			t.Errorf("%s:\n got %s\nwant %s", tt.name, got, tt.want)
		}
	}
}

func TestTracker(t *testing.T) {
	var tr tracker
	if v := tr.view(); v.Mode != "" || v.Confirmed || v.Idle {
		t.Fatalf("empty tracker: %+v", v)
	}

	tr.observe(hid.Tracking)
	if v := tr.view(); v.Mode != "tracking" || !v.Confirmed {
		t.Fatalf("after reading tracking: %+v", v)
	}

	// The stream stops: the mode is a guess from now on.
	tr.observe(hid.Ambiguous)
	if v := tr.view(); v.Mode != "tracking" || v.Confirmed || !v.Idle {
		t.Fatalf("idle: %+v", v)
	}

	// Set standard while idle: unconfirmed, so it stays pending.
	if !tr.sent(hid.Standard, hid.Ambiguous) {
		t.Fatal("standard on an idle camera did not count as landed")
	}
	if v := tr.view(); v.Mode != "standard" || v.Pending != "standard" {
		t.Fatalf("pending: %+v", v)
	}

	// Streaming again, but the camera kept tracking: apply once.
	if m, ok := tr.observe(hid.Tracking); !ok || m != hid.Standard {
		t.Fatalf("observe = %v, %v; want to apply standard", m, ok)
	}
	if _, ok := tr.observe(hid.Tracking); ok {
		t.Fatal("pending mode applied twice")
	}

	// A pending mode the camera already has is just confirmed.
	tr.sent(hid.Standard, hid.Ambiguous)
	if _, ok := tr.observe(hid.Standard); ok {
		t.Fatal("applied a mode the camera already has")
	}

	// Privacy after the request makes it stale.
	tr.sent(hid.Tracking, hid.Ambiguous)
	if _, ok := tr.observe(hid.Privacy); ok {
		t.Fatal("applied a pending mode over a later privacy")
	}
	if v := tr.view(); v.Pending != "" || v.Mode != "privacy" {
		t.Fatalf("after privacy: %+v", v)
	}

	// Privacy that doesn't land is not pending.
	tr.observe(hid.Ambiguous)
	if tr.sent(hid.Privacy, hid.Ambiguous) {
		t.Fatal("privacy counted as landed on an ambiguous readback")
	}
	if v := tr.view(); v.Pending != "" {
		t.Fatalf("privacy left pending: %+v", v)
	}
}

func TestCommandTarget(t *testing.T) {
	for _, tt := range []struct {
		cmd     string
		current hid.Mode
		known   bool
		want    hid.Mode
	}{
		{"toggle", hid.Tracking, true, hid.Standard},
		{"toggle", hid.Standard, true, hid.Tracking},
		{"toggle", hid.Privacy, true, hid.Tracking},
		{"toggle", 0, false, hid.Tracking},
		{"on", hid.Tracking, true, hid.Tracking},
		{"off", hid.Privacy, true, hid.Standard},
	} {
		if got, ok := commandTarget(tt.cmd, tt.current, tt.known); !ok || got != tt.want {
			t.Errorf("commandTarget(%s, %v, %v) = %v, %v; want %v", tt.cmd, tt.current, tt.known, got, ok, tt.want)
		}
	}
	if _, ok := commandTarget("status", 0, false); ok {
		t.Error("commandTarget accepted status")
	}
}

// fixture is a Camera on a fake sysfs and a simulated HID camera.
type fixture struct {
	t    *testing.T
	tree *sysfstest.Tree
	hid  *hidtest.Camera
	cam  *Camera
	open func(string) (hid.Transport, error)
}

func newFixture(t *testing.T, mode hid.Mode, streaming bool) *fixture {
	f := &fixture{t: t, tree: sysfstest.Machine(t), hid: hidtest.New(mode, streaming)}
	f.open = func(string) (hid.Transport, error) { return f.hid.Open(), nil }
	f.cam = New(Options{
		SysRoot:  f.tree.Root,
		ProcRoot: t.TempDir(),
		Timing:   fast,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		OpenHID:  func(path string) (hid.Transport, error) { return f.open(path) },
		ProbeVideo: func([]string) (string, string, error) {
			return "", "", ErrNoVideo // no V4L2 in unit tests
		},
	})
	t.Cleanup(f.cam.shutdown)
	return f
}

func (f *fixture) tracking() Tracking {
	f.t.Helper()
	f.cam.poll()
	return f.cam.State().Tracking
}

func TestCameraReportsMode(t *testing.T) {
	f := newFixture(t, hid.Tracking, true)
	st := f.cam.State()
	if st.Connected || st.Tracking.Error != ErrNotFound {
		t.Fatalf("before the first poll: %+v", st)
	}
	f.cam.poll()
	st = f.cam.State()
	if !st.Connected || st.Camera == nil || st.Camera.USB != "7-1.2.4" || st.Camera.Speed != "480M" || st.Camera.HIDRaw != "/dev/hidraw3" {
		t.Fatalf("state: %+v camera %+v", st, st.Camera)
	}
	if want := (Tracking{Mode: "tracking", Confirmed: true}); st.Tracking != want {
		t.Fatalf("tracking = %+v, want %+v", st.Tracking, want)
	}
}

func TestCameraPendingMode(t *testing.T) {
	f := newFixture(t, hid.Standard, false)
	if tr := f.tracking(); !tr.Idle || tr.Mode != "" {
		t.Fatalf("idle camera: %+v", tr)
	}

	got := f.cam.SetMode(hid.Tracking)
	if want := (Tracking{Mode: "tracking", Idle: true, Pending: "tracking"}); got != want {
		t.Fatalf("SetMode while idle = %+v, want %+v", got, want)
	}
	if f.hid.Mode() != hid.Standard {
		t.Fatal("the simulated camera should have discarded the idle write")
	}

	// Still idle: nothing changes.
	if tr := f.tracking(); tr.Pending != "tracking" {
		t.Fatalf("pending lost while idle: %+v", tr)
	}

	// An app starts streaming: the next poll applies the pending mode.
	f.hid.SetStreaming(true)
	if tr := f.tracking(); tr != (Tracking{Mode: "tracking", Confirmed: true}) {
		t.Fatalf("after streaming started: %+v", tr)
	}
	if f.hid.Mode() != hid.Tracking {
		t.Fatalf("camera is in %v, want tracking", f.hid.Mode())
	}
}

func TestCameraCommand(t *testing.T) {
	f := newFixture(t, hid.Tracking, true)
	f.cam.poll()
	for _, want := range []string{"standard", "tracking", "standard"} {
		got, err := f.cam.Command("toggle")
		if err != nil {
			t.Fatal(err)
		}
		if got != (Tracking{Mode: want, Confirmed: true}) {
			t.Fatalf("toggle = %+v, want %s", got, want)
		}
	}
	if _, err := f.cam.Command("explode"); err == nil {
		t.Fatal("unknown command accepted")
	}
}

func TestCameraToggleWhileIdleUsesLastKnown(t *testing.T) {
	f := newFixture(t, hid.Tracking, true)
	f.cam.poll()
	f.hid.SetStreaming(false)
	if tr := f.tracking(); tr != (Tracking{Mode: "tracking", Idle: true}) {
		t.Fatalf("idle after tracking: %+v", tr)
	}
	got, err := f.cam.Command("toggle")
	if err != nil {
		t.Fatal(err)
	}
	if got != (Tracking{Mode: "standard", Idle: true, Pending: "standard"}) {
		t.Fatalf("toggle while idle = %+v", got)
	}
}

func TestCameraNotSwitched(t *testing.T) {
	f := newFixture(t, hid.Standard, true)
	f.hid.IgnoreSets = true
	f.cam.poll()
	got := f.cam.SetMode(hid.Privacy)
	if got.Error != ErrNotSwitched || got.Mode != "standard" || !got.Confirmed {
		t.Fatalf("SetMode = %+v, want not-switched in standard", got)
	}
	// The snapshot keeps the camera's state, not the request's error.
	if st := f.cam.State().Tracking; st.Error != "" {
		t.Fatalf("state carries the error: %+v", st)
	}
}

func TestCameraUnplugAndReplug(t *testing.T) {
	f := newFixture(t, hid.Tracking, true)
	if tr := f.tracking(); !tr.Confirmed {
		t.Fatalf("before unplug: %+v", tr)
	}

	// The descriptor fails first; sysfs follows a moment later.
	f.hid.Unplug()
	if tr := f.tracking(); !tr.Failed || tr.Error != ErrNotFound {
		t.Fatalf("after unplug: %+v", tr)
	}
	f.tree.RemovePixy()
	f.cam.poll()
	if st := f.cam.State(); st.Connected || st.Camera != nil {
		t.Fatalf("after removal: %+v", st)
	}

	// Plugged back in: a new device number and a new camera behind it.
	f.hid = hidtest.New(hid.Standard, true)
	f.tree.AddPixy("9")
	if tr := f.tracking(); tr != (Tracking{Mode: "standard", Confirmed: true}) {
		t.Fatalf("after replug: %+v", tr)
	}
	if !f.cam.State().Connected {
		t.Fatal("not connected after replug")
	}
}

func TestCameraQuickReplugSamePort(t *testing.T) {
	f := newFixture(t, hid.Tracking, true)
	f.cam.poll()
	opened := 0
	f.open = func(string) (hid.Transport, error) { opened++; return f.hid.Open(), nil }

	// Unplugged and replugged between two polls: same nodes, new devnum.
	f.tree.RemovePixy()
	f.tree.AddPixy("5")
	f.cam.poll()
	if opened != 1 {
		t.Fatalf("hidraw opened %d times after a quick replug, want 1", opened)
	}
}

func TestCameraPermission(t *testing.T) {
	f := newFixture(t, hid.Standard, true)
	f.open = func(path string) (hid.Transport, error) {
		return nil, &os.PathError{Op: "open", Path: path, Err: syscall.EACCES}
	}
	// Within the grace period the error is not reported yet.
	if tr := f.tracking(); tr.Error == ErrPermission {
		t.Fatalf("permission reported during the grace period: %+v", tr)
	}
	f.cam.opts.PermissionGrace = time.Nanosecond
	if tr := f.tracking(); tr.Error != ErrPermission {
		t.Fatalf("after the grace period: %+v", tr)
	}

	// The udev rule arrives.
	f.open = func(string) (hid.Transport, error) { return f.hid.Open(), nil }
	if tr := f.tracking(); !tr.Confirmed {
		t.Fatalf("after permission was granted: %+v", tr)
	}
}

func TestOpeners(t *testing.T) {
	proc := t.TempDir()
	mk := func(pid, comm string, fds map[string]string) {
		dir := filepath.Join(proc, pid)
		if err := os.MkdirAll(filepath.Join(dir, "fd"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "comm"), []byte(comm+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		for fd, target := range fds {
			if err := os.Symlink(target, filepath.Join(dir, "fd", fd)); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk("4242", "chrome", map[string]string{"0": "/dev/null", "41": "/dev/video4", "42": "/dev/video4"})
	mk("17", "zoom", map[string]string{"9": "/dev/video4"})
	mk("99", "pixyd", map[string]string{"5": "/dev/video4"}) // ourselves
	mk("100", "other", map[string]string{"3": "/dev/video0"})
	if err := os.MkdirAll(filepath.Join(proc, "sys"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := openers(proc, "/dev/video4", 99)
	want := []Process{{17, "zoom"}, {4242, "chrome"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("openers = %v, want %v", got, want)
	}
	if got := openers(proc, "", 99); len(got) != 0 {
		t.Fatalf("openers without a node = %v", got)
	}
}

func TestHub(t *testing.T) {
	h := newHub()
	h.publish("state", []byte("1"))
	s := h.subscribe()
	defer s.Close()

	<-s.Ready()
	if got := s.Take(); len(got) != 1 || string(got[0].Data) != "1" {
		t.Fatalf("initial snapshot: %v", got)
	}
	if h.publish("state", []byte("1")) {
		t.Fatal("published an unchanged snapshot")
	}

	// A slow subscriber only sees the latest snapshot per name.
	h.publish("state", []byte("2"))
	h.publish("controls", []byte("a"))
	h.publish("state", []byte("3"))
	<-s.Ready()
	got := s.Take()
	if len(got) != 2 || got[0].Name != "state" || string(got[0].Data) != "3" || string(got[1].Data) != "a" {
		t.Fatalf("coalesced: %+v", got)
	}

	s.Close()
	if h.count() != 0 {
		t.Fatal("subscription not removed")
	}
}
