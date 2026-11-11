package sysfs

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Kazuto/pixyd/internal/sysfs/sysfstest"
)

func TestFind(t *testing.T) {
	tr := sysfstest.Machine(t)
	got := Find(tr.Root)
	want := []Device{{
		USBPath: filepath.Join(tr.Root, sysfstest.Pixy),
		Port:    "7-1.2.4",
		Product: "EMEET PIXY",
		Speed:   "480",
		DevNum:  "4",
		HIDRaw:  "/dev/hidraw3",
		Video:   []string{"/dev/video4", "/dev/video5"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Find =\n%+v\nwant\n%+v", got, want)
	}
}

func TestFindPrefersVendorUsage(t *testing.T) {
	tr := sysfstest.Machine(t)
	// A second PIXY HID interface without the vendor usage, numbered lower.
	tr.HIDRaw(sysfstest.Pixy+"/7-1.2.4:1.3", "0003:328F:00C0.0003", "hidraw1", []byte{0x05, 0x0c, 0x09, 0x01})
	got := Find(tr.Root)
	if len(got) != 1 || got[0].HIDRaw != "/dev/hidraw3" {
		t.Fatalf("Find = %+v, want hidraw3", got)
	}
}

func TestFindFallsBackWithoutVendorUsage(t *testing.T) {
	tr := sysfstest.New(t)
	tr.USB(sysfstest.Pixy, "328f", "00c0", "EMEET PIXY", "480")
	tr.HIDRaw(sysfstest.Pixy+"/7-1.2.4:1.4", "0003:328F:00C0.0004", "hidraw3", []byte{0x05, 0x0c})
	got := Find(tr.Root)
	if len(got) != 1 || got[0].HIDRaw != "/dev/hidraw3" || got[0].Video != nil {
		t.Fatalf("Find = %+v, want only hidraw3", got)
	}
}

func TestFindNothing(t *testing.T) {
	tr := sysfstest.New(t)
	tr.USB(sysfstest.Integrated, "174f", "11b4", "Integrated RGB Camera", "480")
	tr.Video(sysfstest.Integrated+"/1-1:1.0", "video0")
	if got := Find(tr.Root); len(got) != 0 {
		t.Fatalf("Find = %+v, want nothing", got)
	}
	if got := Find(filepath.Join(tr.Root, "missing")); len(got) != 0 {
		t.Fatalf("Find on a missing root = %+v", got)
	}
}

func TestVideoNodeOrder(t *testing.T) {
	tr := sysfstest.New(t)
	tr.USB(sysfstest.Pixy, "328f", "00c0", "EMEET PIXY", "480")
	tr.Video(sysfstest.Pixy+"/7-1.2.4:1.0", "video11")
	tr.Video(sysfstest.Pixy+"/7-1.2.4:1.0", "video10")
	tr.Video(sysfstest.Pixy+"/7-1.2.4:1.0", "video9")
	got := Find(tr.Root)
	want := []string{"/dev/video9", "/dev/video10", "/dev/video11"}
	if len(got) != 1 || !reflect.DeepEqual(got[0].Video, want) {
		t.Fatalf("Video = %v, want %v", got, want)
	}
}

func TestOwner(t *testing.T) {
	tr := sysfstest.Machine(t)
	if d, ok := Owner(tr.Root, "/dev/video5"); !ok || d.Port != "7-1.2.4" {
		t.Errorf("Owner(video5) = %+v, %v", d, ok)
	}
	if d, ok := Owner(tr.Root, "/dev/hidraw3"); !ok || d.Port != "7-1.2.4" {
		t.Errorf("Owner(hidraw3) = %+v, %v", d, ok)
	}
	if _, ok := Owner(tr.Root, "/dev/video0"); ok {
		t.Error("Owner(video0) found the integrated camera")
	}
}

func TestKeyChangesOnReplug(t *testing.T) {
	tr := sysfstest.Machine(t)
	before := Find(tr.Root)[0].Key()
	tr.RemovePixy()
	if got := Find(tr.Root); len(got) != 0 {
		t.Fatalf("Find after unplug = %+v", got)
	}
	tr.AddPixy("5")
	if after := Find(tr.Root)[0].Key(); after == before {
		t.Errorf("Key unchanged after replug: %s", after)
	}
}

// TestFindLive runs against the real sysfs and only checks what can be said
// without knowing the machine: a PIXY, if present, has its nodes paired.
func TestFindLive(t *testing.T) {
	for _, d := range Find("/") {
		t.Logf("found %+v", d)
		if d.Port == "" || d.Product == "" {
			t.Errorf("incomplete device %+v", d)
		}
	}
}
