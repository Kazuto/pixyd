package hid_test

import (
	"bytes"
	"errors"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/Kazuto/pixyd/internal/hid"
	"github.com/Kazuto/pixyd/internal/hid/hidtest"
)

// fast keeps the real retry structure but no real waiting.
var fast = hid.Timing{
	Gap:           time.Microsecond,
	QueryTimeout:  time.Millisecond,
	QueryAttempts: 4,
	Settle:        time.Microsecond,
	Retry:         time.Microsecond,
	PrivacyExit:   time.Microsecond,
}

func hexReport(t *testing.T, prefix ...byte) []byte {
	t.Helper()
	r := make([]byte, hid.ReportSize)
	copy(r, prefix)
	return r
}

func TestBuild(t *testing.T) {
	tests := []struct {
		name    string
		group   byte
		a, b    byte
		payload []byte
		want    []byte
	}{
		{"mode set", 0x01, 0x01, 0x00, []byte{0x01}, []byte{0x09, 0x01, 0x01, 0x00, 0x00, 0x01, 0x00, 0x01, 0x01}},
		{"mode query", 0x01, 0x01, 0x01, nil, []byte{0x09, 0x01, 0x01, 0x01}},
		{"gesture read", 0x04, 0x02, 0x01, []byte{0x02}, []byte{0x09, 0x04, 0x02, 0x01, 0x00, 0x01, 0x00, 0x01, 0x02}},
		{"auto-privacy set", 0x02, 0x01, 0x00, []byte{0x0a, 0, 0, 0}, []byte{0x09, 0x02, 0x01, 0x00, 0x00, 0x04, 0x00, 0x04, 0x0a}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := hid.Build(tt.group, tt.a, tt.b, tt.payload...)
			if err != nil {
				t.Fatal(err)
			}
			if want := hexReport(t, tt.want...); !bytes.Equal(got, want) {
				t.Errorf("Build = % x\nwant    % x", got, want)
			}
		})
	}

	if _, err := hid.Build(0x01, 0x01, 0x00, make([]byte, 25)...); err == nil {
		t.Error("Build accepted a payload that does not fit")
	}
}

func TestMatches(t *testing.T) {
	query := hexReport(t, 0x09, 0x01, 0x01, 0x01)
	tests := []struct {
		name  string
		reply []byte
		want  bool
	}{
		{"same group and command", hexReport(t, 0x09, 0x01, 0x01, 0x01, 0, 1, 0, 1, 0x01), true},
		{"group with high bits", hexReport(t, 0x09, 0x41, 0x01, 0x01, 0, 1, 0, 1, 0x01), true},
		{"group 0x61", hexReport(t, 0x09, 0x61, 0x01, 0x01, 0, 1, 0, 1, 0x01), true},
		{"set ack in same group", hexReport(t, 0x09, 0x41, 0x01, 0x00, 0, 1, 0, 1, 0x20), false},
		{"other group", hexReport(t, 0x09, 0x02, 0x01, 0x01, 0, 1, 0, 1, 0x00), false},
		{"other report id", hexReport(t, 0x0a, 0x01, 0x01, 0x01, 0, 1, 0, 1, 0x01), false},
		{"too short", []byte{0x09, 0x01, 0x01}, false},
	}
	for _, tt := range tests {
		if got := hid.Matches(query, tt.reply); got != tt.want {
			t.Errorf("%s: Matches = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestReadModeAbsorbsDroppedFirstWrite(t *testing.T) {
	cam := hidtest.New(hid.Tracking, true)
	conn := hid.NewConn(cam.Open(), fast)

	m, err := conn.ReadMode()
	if err != nil || m != hid.Tracking {
		t.Fatalf("ReadMode = %v, %v; want tracking", m, err)
	}
	if n := len(cam.Writes()); n != 2 {
		t.Errorf("%d writes, want 2 (the first one is dropped)", n)
	}

	// The descriptor is warm now: one write per query.
	if _, err := conn.ReadMode(); err != nil {
		t.Fatal(err)
	}
	if n := len(cam.Writes()); n != 3 {
		t.Errorf("%d writes after the second query, want 3", n)
	}
}

func TestReadModeSkipsUnrelatedReports(t *testing.T) {
	cam := hidtest.New(hid.Standard, true)
	conn := hid.NewConn(cam.Open(), fast)
	if _, err := conn.ReadMode(); err != nil { // warm up
		t.Fatal(err)
	}

	// An unsolicited status report and a stale set acknowledgement in the
	// same group arrive before the real reply.
	cam.Inject(hexReport(t, 0x09, 0x42, 0x00, 0x02, 0, 1, 0, 1, 0x03))
	cam.Inject(hexReport(t, 0x09, 0x41, 0x01, 0x00, 0, 1, 0, 1, 0x20))
	m, err := conn.ReadMode()
	if err != nil || m != hid.Standard {
		t.Fatalf("ReadMode = %v, %v; want standard", m, err)
	}
}

func TestReadModeIdle(t *testing.T) {
	for _, tt := range []struct {
		mode hid.Mode
		want hid.Mode
	}{
		{hid.Standard, hid.Ambiguous},
		{hid.Tracking, hid.Ambiguous},
		{hid.Privacy, hid.Privacy},
	} {
		cam := hidtest.New(tt.mode, false)
		got, err := hid.NewConn(cam.Open(), fast).ReadMode()
		if err != nil || got != tt.want {
			t.Errorf("idle camera in %v: ReadMode = %v, %v; want %v", tt.mode, got, err, tt.want)
		}
	}
}

func TestReadModeNoAnswer(t *testing.T) {
	cam := hidtest.New(hid.Standard, true)
	cam.Silent = true
	_, err := hid.NewConn(cam.Open(), fast).ReadMode()
	if !errors.Is(err, hid.ErrNoAnswer) {
		t.Fatalf("ReadMode error = %v, want ErrNoAnswer", err)
	}
	if n := len(cam.Writes()); n != fast.QueryAttempts {
		t.Errorf("%d writes, want %d attempts", n, fast.QueryAttempts)
	}
}

func TestReadModeUnplugged(t *testing.T) {
	cam := hidtest.New(hid.Standard, true)
	conn := hid.NewConn(cam.Open(), fast)
	cam.Unplug()
	_, err := conn.ReadMode()
	if !errors.Is(err, syscall.ENODEV) && !errors.Is(err, syscall.EIO) {
		t.Fatalf("ReadMode error = %v, want an I/O error", err)
	}
}

func TestSetMode(t *testing.T) {
	tests := []struct {
		name      string
		from      hid.Mode
		streaming bool
		cold      bool
		to        hid.Mode
		got       hid.Mode   // what SetMode returns
		real      hid.Mode   // the mode the camera ends up in
		sets      []hid.Mode // mode set reports written, dropped ones included
	}{
		{"standard to tracking", hid.Standard, true, false, hid.Tracking, hid.Tracking, hid.Tracking, []hid.Mode{hid.Tracking}},
		{"tracking to standard", hid.Tracking, true, false, hid.Standard, hid.Standard, hid.Standard, []hid.Mode{hid.Standard}},
		{"already there", hid.Tracking, true, false, hid.Tracking, hid.Tracking, hid.Tracking, nil},
		{"first set after idle is dropped", hid.Standard, true, true, hid.Tracking, hid.Tracking, hid.Tracking, []hid.Mode{hid.Tracking, hid.Tracking}},
		{"privacy to tracking goes through standard", hid.Privacy, true, false, hid.Tracking, hid.Tracking, hid.Tracking, []hid.Mode{hid.Standard, hid.Tracking}},
		{"privacy to standard", hid.Privacy, true, false, hid.Standard, hid.Standard, hid.Standard, []hid.Mode{hid.Standard}},
		{"into privacy", hid.Tracking, true, false, hid.Privacy, hid.Privacy, hid.Privacy, []hid.Mode{hid.Privacy}},
		{"into privacy while idle", hid.Tracking, false, false, hid.Privacy, hid.Privacy, hid.Privacy, []hid.Mode{hid.Privacy}},
		{"cold privacy while idle is retried", hid.Standard, false, true, hid.Privacy, hid.Privacy, hid.Privacy, []hid.Mode{hid.Privacy, hid.Privacy}},
		{"idle camera discards tracking", hid.Standard, false, false, hid.Tracking, hid.Ambiguous, hid.Standard, []hid.Mode{hid.Tracking}},
		{"leaving privacy while idle keeps the old mode", hid.Privacy, false, false, hid.Standard, hid.Ambiguous, hid.Standard, []hid.Mode{hid.Standard}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cam := hidtest.New(tt.from, tt.streaming)
			if tt.cold {
				cam.Rest()
			}
			got, err := hid.NewConn(cam.Open(), fast).SetMode(tt.to)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.got {
				t.Errorf("SetMode returned %v, want %v", got, tt.got)
			}
			if !hid.Landed(tt.to, got) {
				t.Errorf("Landed(%v, %v) = false", tt.to, got)
			}
			if real := cam.Mode(); real != tt.real {
				t.Errorf("camera is in %v, want %v", real, tt.real)
			}
			if sets := cam.ModeSets(); !slices.Equal(sets, tt.sets) {
				t.Errorf("mode sets written: %v, want %v", sets, tt.sets)
			}
		})
	}
}

func TestSetModeNotLanded(t *testing.T) {
	cam := hidtest.New(hid.Standard, true)
	cam.IgnoreSets = true
	got, err := hid.NewConn(cam.Open(), fast).SetMode(hid.Tracking)
	if err != nil {
		t.Fatal(err)
	}
	if got != hid.Standard || hid.Landed(hid.Tracking, got) {
		t.Errorf("SetMode = %v, want standard (not landed)", got)
	}
	// Retried exactly once.
	if sets := cam.ModeSets(); !slices.Equal(sets, []hid.Mode{hid.Tracking, hid.Tracking}) {
		t.Errorf("mode sets written: %v, want two tracking sets", sets)
	}
}

func TestSetModeRejectsUnsettable(t *testing.T) {
	cam := hidtest.New(hid.Standard, true)
	if _, err := hid.NewConn(cam.Open(), fast).SetMode(hid.Ambiguous); err == nil {
		t.Fatal("SetMode(Ambiguous) succeeded")
	}
	if len(cam.Writes()) != 0 {
		t.Error("SetMode(Ambiguous) wrote to the camera")
	}
}

func TestParseMode(t *testing.T) {
	for _, m := range []hid.Mode{hid.Standard, hid.Tracking, hid.Privacy} {
		got, err := hid.ParseMode(m.String())
		if err != nil || got != m {
			t.Errorf("ParseMode(%q) = %v, %v", m.String(), got, err)
		}
	}
	for _, s := range []string{"idle", "", "Tracking", "0x01"} {
		if _, err := hid.ParseMode(s); err == nil {
			t.Errorf("ParseMode(%q) succeeded", s)
		}
	}
}
