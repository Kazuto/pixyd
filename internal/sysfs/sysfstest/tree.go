// Package sysfstest builds fake sysfs trees for tests.
package sysfstest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Paths of the USB devices on the development machine: the PIXY behind a
// dock, the integrated camera and a YubiKey.
const (
	Pixy       = "sys/devices/pci0000:00/0000:00:08.3/0000:c6:00.4/usb7/7-1/7-1.2/7-1.2.4"
	Integrated = "sys/devices/pci0000:00/0000:00:08.1/0000:c4:00.4/usb1/1-1"
	YubiKey    = "sys/devices/pci0000:00/0000:00:08.3/0000:c6:00.4/usb7/7-1/7-1.2/7-1.2.1/7-1.2.1.1"
)

// PixyDescriptor starts like the PIXY's vendor report descriptor.
var PixyDescriptor = []byte{0x05, 0x83, 0x09, 0x83, 0xa1, 0x01, 0x85, 0x09}

// Tree is a fake sysfs below Root.
type Tree struct {
	t    testing.TB
	Root string
}

// New returns an empty tree in a temp dir.
func New(t testing.TB) *Tree {
	return &Tree{t: t, Root: t.TempDir()}
}

// Machine returns a tree shaped like the development machine.
func Machine(t testing.TB) *Tree {
	tr := New(t)
	tr.USB(Integrated, "174f", "11b4", "Integrated RGB Camera", "480")
	tr.Video(Integrated+"/1-1:1.0", "video0")
	tr.Video(Integrated+"/1-1:1.0", "video1")
	tr.USB(YubiKey, "1050", "0406", "YubiKey", "12")
	tr.HIDRaw(YubiKey+"/7-1.2.1.1:1.0", "0003:1050:0406.0002", "hidraw2", []byte{0x06, 0xd0, 0xf1})
	tr.AddPixy("4")
	return tr
}

// AddPixy adds the PIXY with USB device number devnum.
func (tr *Tree) AddPixy(devnum string) {
	tr.USB(Pixy, "328f", "00c0", "EMEET PIXY", "480")
	tr.Write(Pixy+"/devnum", devnum+"\n")
	tr.Video(Pixy+"/7-1.2.4:1.0", "video4")
	tr.Video(Pixy+"/7-1.2.4:1.0", "video5")
	tr.HIDRaw(Pixy+"/7-1.2.4:1.4", "0003:328F:00C0.0004", "hidraw3", PixyDescriptor)
}

// RemovePixy unplugs the PIXY.
func (tr *Tree) RemovePixy() {
	tr.t.Helper()
	for _, p := range []string{Pixy, "sys/class/video4linux/video4", "sys/class/video4linux/video5", "sys/class/hidraw/hidraw3"} {
		if err := os.RemoveAll(filepath.Join(tr.Root, p)); err != nil {
			tr.t.Fatal(err)
		}
	}
}

// Write creates a file with its parent directories.
func (tr *Tree) Write(path, content string) {
	tr.t.Helper()
	full := filepath.Join(tr.Root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		tr.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		tr.t.Fatal(err)
	}
}

// Link creates a symlink at path pointing to target.
func (tr *Tree) Link(target, path string) {
	tr.t.Helper()
	full := filepath.Join(tr.Root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		tr.t.Fatal(err)
	}
	if err := os.Symlink(target, full); err != nil {
		tr.t.Fatal(err)
	}
}

// USB adds a USB device directory.
func (tr *Tree) USB(dir, vendor, product, name, speed string) {
	tr.Write(dir+"/idVendor", vendor+"\n")
	tr.Write(dir+"/idProduct", product+"\n")
	tr.Write(dir+"/product", name+"\n")
	tr.Write(dir+"/speed", speed+"\n")
}

// Video adds /sys/class/video4linux/<name> for a node on interface intf.
func (tr *Tree) Video(intf, name string) {
	dev := intf + "/video4linux/" + name
	tr.Write(dev+"/name", "camera\n")
	tr.Link("../..", dev+"/device") // .../video4linux/videoN/device -> interface
	tr.Link("../../"+strings.TrimPrefix(dev, "sys/"), "sys/class/video4linux/"+name)
}

// HIDRaw adds /sys/class/hidraw/<name> for a HID device below intf.
func (tr *Tree) HIDRaw(intf, hidDev, name string, descriptor []byte) {
	hid := intf + "/" + hidDev
	tr.Write(hid+"/report_descriptor", string(descriptor))
	dev := hid + "/hidraw/" + name
	tr.Write(dev+"/dev", "241:3\n")
	tr.Link("../..", dev+"/device") // .../hidraw/hidrawN/device -> HID device
	tr.Link("../../"+strings.TrimPrefix(dev, "sys/"), "sys/class/hidraw/"+name)
}
