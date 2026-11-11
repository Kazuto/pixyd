// Package sysfs finds EMEET PIXY cameras through sysfs and pairs each one's
// vendor hidraw node with its video nodes, without opening any device.
package sysfs

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// USB IDs of the EMEET PIXY.
const (
	VendorID  = "328f"
	ProductID = "00c0"
)

// vendorUsage is "Usage Page 0x83, Usage 0x83" in a HID report descriptor:
// the interface that answers the vendor reports.
var vendorUsage = []byte{0x05, 0x83, 0x09, 0x83}

// Device is one PIXY.
type Device struct {
	// USBPath is the USB device's sysfs directory; Port its name, e.g.
	// "7-1.2.4". Both identify the physical device while it stays plugged in.
	USBPath string
	Port    string
	Product string // e.g. "EMEET PIXY"
	Speed   string // in Mbit/s, e.g. "480"
	// DevNum is the USB device number, which changes on every
	// re-enumeration, so a quick replug into the same port still counts as
	// a new device.
	DevNum string
	// HIDRaw is the vendor hidraw node, e.g. /dev/hidraw3, or "".
	HIDRaw string
	// Video lists the device's video nodes by index; one is the capture
	// node, the other a metadata node. Telling them apart needs
	// VIDIOC_QUERYCAP.
	Video []string
}

// Find returns the PIXYs below root ("/" on a real system), ordered by USB
// path. Unreadable entries are skipped: sysfs changes under our feet while a
// device is being plugged in or out.
func Find(root string) []Device {
	devices := map[string]*Device{}
	get := func(usbPath string) *Device {
		d := devices[usbPath]
		if d == nil {
			d = &Device{
				USBPath: usbPath,
				Port:    filepath.Base(usbPath),
				Product: readAttr(usbPath, "product"),
				Speed:   readAttr(usbPath, "speed"),
				DevNum:  readAttr(usbPath, "devnum"),
			}
			devices[usbPath] = d
		}
		return d
	}

	// hidraw: prefer the node advertising the vendor usage; fall back to
	// any hidraw node of the device if none does.
	fallback := map[string]string{}
	for _, entry := range classEntries(root, "hidraw") {
		usb, ok := pixyUSBDevice(filepath.Join(entry, "device"))
		if !ok {
			continue
		}
		node := "/dev/" + filepath.Base(entry)
		descriptor, _ := os.ReadFile(filepath.Join(entry, "device", "report_descriptor"))
		d := get(usb)
		switch {
		case bytes.Contains(descriptor, vendorUsage) && d.HIDRaw == "":
			d.HIDRaw = node
		case fallback[usb] == "":
			fallback[usb] = node
		}
	}
	for usb, node := range fallback {
		if d := devices[usb]; d.HIDRaw == "" {
			d.HIDRaw = node
		}
	}

	for _, entry := range classEntries(root, "video4linux") {
		usb, ok := pixyUSBDevice(filepath.Join(entry, "device"))
		if !ok {
			continue
		}
		d := get(usb)
		d.Video = append(d.Video, "/dev/"+filepath.Base(entry))
	}

	out := make([]Device, 0, len(devices))
	for _, d := range devices {
		slices.SortFunc(d.Video, byNodeNumber)
		out = append(out, *d)
	}
	slices.SortFunc(out, func(a, b Device) int { return strings.Compare(a.USBPath, b.USBPath) })
	return out
}

// Owner returns the PIXY a /dev node (hidraw or video) belongs to, for
// nodes given explicitly instead of discovered.
func Owner(root, node string) (Device, bool) {
	name := filepath.Base(node)
	for _, d := range Find(root) {
		if filepath.Base(d.HIDRaw) == name {
			return d, true
		}
		for _, v := range d.Video {
			if filepath.Base(v) == name {
				return d, true
			}
		}
	}
	return Device{}, false
}

func classEntries(root, class string) []string {
	entries, _ := filepath.Glob(filepath.Join(root, "sys", "class", class, "*"))
	return entries
}

// pixyUSBDevice walks up from a class device's "device" link to the USB
// device directory (the first ancestor with idVendor) and reports whether
// it is a PIXY.
func pixyUSBDevice(link string) (string, bool) {
	dir, err := filepath.EvalSymlinks(link)
	if err != nil {
		return "", false
	}
	for ; dir != "/" && dir != "."; dir = filepath.Dir(dir) {
		vendor := readAttr(dir, "idVendor")
		if vendor == "" {
			continue
		}
		return dir, strings.EqualFold(vendor, VendorID) &&
			strings.EqualFold(readAttr(dir, "idProduct"), ProductID)
	}
	return "", false
}

func readAttr(dir, name string) string {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// byNodeNumber orders /dev/video10 after /dev/video9.
func byNodeNumber(a, b string) int {
	return nodeNumber(a) - nodeNumber(b)
}

func nodeNumber(path string) int {
	base := filepath.Base(path)
	n, _ := strconv.Atoi(strings.TrimLeft(base, "abcdefghijklmnopqrstuvwxyz"))
	return n
}

// Key identifies the device and its nodes; it changes when the camera is
// re-plugged or its nodes are renumbered.
func (d Device) Key() string {
	return d.USBPath + "#" + d.DevNum + "#" + d.HIDRaw + "#" + strings.Join(d.Video, ",")
}
