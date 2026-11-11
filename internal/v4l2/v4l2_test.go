package v4l2

import (
	"os"
	"strconv"
	"testing"

	"github.com/Kazuto/pixyd/internal/sysfs"
)

// The struct sizes are encoded in the ioctl numbers; these are the values
// <linux/videodev2.h> gives, so a wrong struct layout fails here instead of
// as a confusing errno on the camera.
func TestIoctlNumbers(t *testing.T) {
	for _, tt := range []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"VIDIOC_QUERYCAP", vidiocQueryCap, 0x80685600},
		{"VIDIOC_G_CTRL", vidiocGCtrl, 0xc008561b},
		{"VIDIOC_S_CTRL", vidiocSCtrl, 0xc008561c},
		{"VIDIOC_QUERYCTRL", vidiocQueryCtrl, 0xc0445624},
		{"VIDIOC_QUERYMENU", vidiocQueryMenu, 0xc02c5625},
	} {
		if tt.got != tt.want {
			t.Errorf("%s = %#x, want %#x", tt.name, tt.got, tt.want)
		}
	}
}

func TestName(t *testing.T) {
	for label, want := range map[string]string{
		"Brightness":                      "brightness",
		"White Balance, Automatic":        "white_balance_automatic",
		"White Balance Temperature, Auto": "white_balance_temperature_auto",
		"Exposure Time, Absolute":         "exposure_time_absolute",
		"Pan, Absolute":                   "pan_absolute",
		"Power Line Frequency":            "power_line_frequency",
		"  Leading, trailing!  ":          "leading_trailing",
		"Focus, Automatic Continuous":     "focus_automatic_continuous",
	} {
		if got := Name(label); got != want {
			t.Errorf("Name(%q) = %q, want %q", label, got, want)
		}
	}
}

func TestCoerce(t *testing.T) {
	pan := Control{Name: "pan_absolute", typ: typeInteger, Min: -540000, Max: 540000, Step: 3600}
	zoom := Control{Name: "zoom_absolute", typ: typeInteger, Min: 100, Max: 150, Step: 1}
	odd := Control{Name: "odd", typ: typeInteger, Min: 1, Max: 10, Step: 4} // 1, 5, 9
	plf := Control{Name: "power_line_frequency", typ: typeMenu, Menu: []MenuItem{{0, "Disabled"}, {1, "50 Hz"}, {2, "60 Hz"}}}
	awb := Control{Name: "white_balance_automatic", typ: typeBoolean, Max: 1, Step: 1}
	ro := Control{Name: "ro", typ: typeInteger, Max: 10, flags: flagReadOnly}

	for _, tt := range []struct {
		c    Control
		in   int64
		want int32
	}{
		{pan, 0, 0},
		{pan, 3600, 3600},
		{pan, 1799, 0},
		{pan, 1800, 3600},
		{pan, -5000, -3600},
		{pan, 1 << 40, 540000},
		{pan, -1 << 40, -540000},
		{zoom, 99, 100},
		{zoom, 151, 150},
		{zoom, 125, 125},
		{odd, 7, 9},
		{odd, 10, 9}, // 10 is the max but not on a step
		{odd, 2, 1},
		{plf, 2, 2},
		{awb, 5, 1},
		{awb, 0, 0},
	} {
		got, err := tt.c.Coerce(tt.in)
		if err != nil || got != tt.want {
			t.Errorf("%s.Coerce(%d) = %d, %v; want %d", tt.c.Name, tt.in, got, err, tt.want)
		}
	}

	if _, err := plf.Coerce(3); err == nil {
		t.Error("menu accepted a value without an entry")
	}
	if _, err := ro.Coerce(1); err == nil {
		t.Error("read-only control accepted a value")
	}
}

// TestLive lists the PIXY's controls if it is plugged in. It only reads.
func TestLive(t *testing.T) {
	devices := sysfs.Find("/")
	if len(devices) == 0 {
		t.Skip("no EMEET PIXY connected")
	}
	for _, path := range devices[0].Video {
		d, err := Open(path)
		if os.IsPermission(err) {
			t.Skipf("no access to %s", path)
		}
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		c, err := d.QueryCap()
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: %q driver=%s capture=%v caps=%#x", path, c.Card, c.Driver, c.IsCapture(), c.DeviceCaps)
		if !c.IsCapture() {
			continue
		}
		controls, err := d.Controls()
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range controls {
			v := "-"
			if c.Value != nil {
				v = strconv.Itoa(int(*c.Value))
			}
			t.Logf("  %-34s %-7s %-6s [%d..%d/%d] def=%d val=%s flags=%v menu=%v %s",
				c.Name, c.Class, c.Type, c.Min, c.Max, c.Step, c.Default, v, c.Flags, c.Menu, c.Error)
		}
		if len(controls) == 0 {
			t.Error("capture node has no controls")
		}
	}
}
