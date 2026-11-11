package hid_test

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Kazuto/pixyd/internal/hid"
	"github.com/Kazuto/pixyd/internal/sysfs"
)

// Hardware tests, skipped unless asked for:
//
//	PIXYD_HW=read go test -v -run Hardware ./internal/hid   # queries only
//	PIXYD_HW=mode go test -v -run Hardware ./internal/hid   # also switches modes
//
// The mode test needs an app streaming from the camera (otherwise standard
// and tracking can't be told apart) and restores the mode it found. It
// passes through privacy, which closes the lens for a moment.

func hardware(t *testing.T, level string) *hid.Conn {
	t.Helper()
	want := os.Getenv("PIXYD_HW")
	if want == "" || (level == "mode" && want != "mode") {
		t.Skipf("set PIXYD_HW=%s to run", level)
	}
	devices := sysfs.Find("/")
	if len(devices) == 0 || devices[0].HIDRaw == "" {
		t.Skip("no EMEET PIXY connected")
	}
	tr, err := hid.Open(devices[0].HIDRaw)
	if err != nil {
		t.Fatal(err)
	}
	conn := hid.NewConn(tr, hid.Timing{})
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestHardwareRead(t *testing.T) {
	conn := hardware(t, "read")

	start := time.Now()
	m, err := conn.ReadMode()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("mode on a fresh descriptor: %v (raw %#02x) in %v", m, byte(m), time.Since(start))

	start = time.Now()
	if m, err = conn.ReadMode(); err != nil {
		t.Fatal(err)
	}
	t.Logf("mode on a warm descriptor: %v in %v", m, time.Since(start))

	// Device/build identifier, an ASCII payload (PixyPilot's device_info).
	info, err := hid.Build(0x01, 0x00, 0x03)
	if err != nil {
		t.Fatal(err)
	}
	reply, err := conn.Query(info)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("device info: % x (%q)", reply[:8], strings.TrimRight(string(reply[8:8+int(reply[5])]), "\x00"))
}

func TestHardwareMode(t *testing.T) {
	conn := hardware(t, "mode")
	orig, err := conn.ReadMode()
	if err != nil {
		t.Fatal(err)
	}
	if !orig.Settable() {
		t.Skipf("camera reports %v: start a video stream first", orig)
	}
	t.Logf("starting in %v", orig)
	t.Cleanup(func() {
		got, err := conn.SetMode(orig)
		t.Logf("restored %v: camera reports %v, %v", orig, got, err)
		if err != nil || got != orig {
			t.Errorf("could not restore %v", orig)
		}
	})

	for _, target := range []hid.Mode{hid.Standard, hid.Tracking, hid.Standard, hid.Privacy, hid.Tracking, hid.Privacy, hid.Standard} {
		start := time.Now()
		got, err := conn.SetMode(target)
		if err != nil {
			t.Fatalf("SetMode(%v): %v", target, err)
		}
		t.Logf("SetMode(%v) = %v in %v", target, got, time.Since(start).Round(time.Millisecond))
		if got != target {
			t.Errorf("SetMode(%v) = %v", target, got)
		}
		// A fresh read must agree a moment later.
		time.Sleep(300 * time.Millisecond)
		if now, err := conn.ReadMode(); err != nil || now != target {
			t.Errorf("read back %v, %v after setting %v", now, err, target)
		}
	}
}
