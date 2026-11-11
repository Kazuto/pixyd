package v4l2

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Control is one V4L2 control with its metadata and current value.
type Control struct {
	ID    uint32 `json:"id"`
	Name  string `json:"name"`  // v4l2-ctl style, e.g. "white_balance_automatic"
	Label string `json:"label"` // as the driver names it, e.g. "White Balance, Automatic"
	Class string `json:"class"` // "user", "camera", …
	Type  string `json:"type"`  // "int", "bool", "menu", "intmenu", "button", "int64", "string", "bitmask"

	Min     int32 `json:"min"`
	Max     int32 `json:"max"`
	Step    int32 `json:"step"`
	Default int32 `json:"default"`
	// Value is nil when the control has no readable value (buttons,
	// write-only controls, unsupported types) or reading it failed.
	Value *int32 `json:"value"`

	// Flags: "inactive", "read-only", "write-only", "volatile", "grabbed",
	// "slider", "update".
	Flags []string   `json:"flags"`
	Menu  []MenuItem `json:"menu,omitempty"`
	// Error is set when reading the value failed.
	Error string `json:"error,omitempty"`

	flags uint32
	typ   uint32
}

// MenuItem is one entry of a menu control. Value is what to set; for an
// integer menu Label shows the integer the entry stands for.
type MenuItem struct {
	Value int32  `json:"value"`
	Label string `json:"label"`
}

// Inactive reports whether the control currently has no effect, e.g.
// exposure time while auto exposure is on.
func (c Control) Inactive() bool { return c.flags&flagInactive != 0 }

// Settable reports whether the control can be written at all.
func (c Control) Settable() bool {
	if c.flags&(flagReadOnly|flagDisabled) != 0 {
		return false
	}
	switch c.typ {
	case typeInteger, typeBoolean, typeMenu, typeIntegerMenu, typeButton:
		return true
	}
	return false
}

// IsInteger reports whether the control takes a range of integers.
func (c Control) IsInteger() bool { return c.typ == typeInteger }

// Coerce turns a requested value into one the control accepts: integers
// are clamped to the range and snapped onto a step boundary (the driver
// would round them anyway and the readback would disagree), booleans become
// 0 or 1, and menu values must name an entry.
func (c Control) Coerce(v int64) (int32, error) {
	if !c.Settable() {
		return 0, fmt.Errorf("%s cannot be set", c.Name)
	}
	switch c.typ {
	case typeBoolean:
		if v != 0 {
			return 1, nil
		}
		return 0, nil
	case typeButton:
		return 1, nil
	case typeMenu, typeIntegerMenu:
		for _, item := range c.Menu {
			if int64(item.Value) == v {
				return item.Value, nil
			}
		}
		return 0, fmt.Errorf("%s has no menu entry %d", c.Name, v)
	}

	lo, hi := int64(c.Min), int64(c.Max)
	v = max(lo, min(hi, v))
	if step := int64(c.Step); step > 1 {
		offset := v - lo
		v = lo + (offset+step/2)/step*step
		if v > hi {
			v -= step
		}
	}
	return int32(v), nil
}

// Controls lists the node's controls, skipping class headings and
// disabled controls.
func (d *Device) Controls() ([]Control, error) {
	var out []Control
	var last uint32
	for {
		q := queryctrl{ID: last | flagNextCtrl}
		if err := d.ioctl(vidiocQueryCtrl, unsafe.Pointer(&q)); err != nil {
			if errors.Is(err, unix.EINVAL) {
				return out, nil // no more controls
			}
			return nil, err
		}
		if q.ID <= last { // a broken driver; don't loop forever
			return out, nil
		}
		last = q.ID
		if q.Type == typeCtrlClass || q.Flags&flagDisabled != 0 {
			continue
		}
		out = append(out, d.describe(q))
	}
}

// Control returns one control by ID.
func (d *Device) Control(id uint32) (Control, error) {
	q := queryctrl{ID: id}
	if err := d.ioctl(vidiocQueryCtrl, unsafe.Pointer(&q)); err != nil {
		return Control{}, err
	}
	return d.describe(q), nil
}

// Set writes a control's value as is; see Control.Coerce.
func (d *Device) Set(id uint32, value int32) error {
	c := control{ID: id, Value: value}
	return d.ioctl(vidiocSCtrl, unsafe.Pointer(&c))
}

// Get reads a control's value.
func (d *Device) Get(id uint32) (int32, error) {
	c := control{ID: id}
	if err := d.ioctl(vidiocGCtrl, unsafe.Pointer(&c)); err != nil {
		return 0, err
	}
	return c.Value, nil
}

func (d *Device) describe(q queryctrl) Control {
	label := cString(q.Name[:])
	c := Control{
		ID:      q.ID,
		Name:    Name(label),
		Label:   label,
		Class:   className(q.ID),
		Type:    typeName(q.Type),
		Min:     q.Minimum,
		Max:     q.Maximum,
		Step:    q.Step,
		Default: q.DefaultValue,
		Flags:   flagNames(q.Flags),
		flags:   q.Flags,
		typ:     q.Type,
	}
	if q.Type == typeMenu || q.Type == typeIntegerMenu {
		c.Menu = d.menu(q)
	}
	readable := q.Flags&flagWriteOnly == 0
	switch q.Type {
	case typeInteger, typeBoolean, typeMenu, typeIntegerMenu, typeBitmask:
	default:
		readable = false
	}
	if readable {
		v, err := d.Get(q.ID)
		if err != nil {
			c.Error = err.Error()
		} else {
			c.Value = &v
		}
	}
	return c
}

// menu lists a menu's entries. Menus can have gaps; QUERYMENU fails with
// EINVAL for the missing indices.
func (d *Device) menu(q queryctrl) []MenuItem {
	var items []MenuItem
	if q.Maximum-q.Minimum > 255 || q.Minimum < 0 {
		return nil
	}
	for i := q.Minimum; i <= q.Maximum; i++ {
		m := querymenu{ID: q.ID, Index: uint32(i)}
		if err := d.ioctl(vidiocQueryMenu, unsafe.Pointer(&m)); err != nil {
			continue
		}
		label := cString(m.Union[:])
		if q.Type == typeIntegerMenu {
			label = fmt.Sprint(int64(binary.LittleEndian.Uint64(m.Union[:8])))
		}
		items = append(items, MenuItem{Value: i, Label: label})
	}
	return items
}

// Name derives v4l2-ctl's name for a control label: lowercase letters and
// digits, with runs of anything else turned into one underscore.
// "White Balance, Automatic" becomes "white_balance_automatic".
func Name(label string) string {
	var b strings.Builder
	gap := false
	for _, r := range label {
		if r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			if gap && b.Len() > 0 {
				b.WriteByte('_')
			}
			gap = false
			b.WriteRune(unicode.ToLower(r))
			continue
		}
		gap = true
	}
	return b.String()
}

func className(id uint32) string {
	switch id & 0x0fff0000 {
	case 0x00980000:
		return "user"
	case 0x009a0000:
		return "camera"
	case 0x009e0000:
		return "jpeg"
	case 0x009f0000:
		return "image-source"
	case 0x00a10000:
		return "image-processing"
	}
	return fmt.Sprintf("0x%08x", id&0x0fff0000)
}

func typeName(t uint32) string {
	switch t {
	case typeInteger:
		return "int"
	case typeBoolean:
		return "bool"
	case typeMenu:
		return "menu"
	case typeIntegerMenu:
		return "intmenu"
	case typeButton:
		return "button"
	case typeInteger64:
		return "int64"
	case typeString:
		return "string"
	case typeBitmask:
		return "bitmask"
	}
	return fmt.Sprintf("type-%d", t)
}

func flagNames(f uint32) []string {
	names := []string{}
	for _, flag := range []struct {
		bit  uint32
		name string
	}{
		{flagInactive, "inactive"},
		{flagReadOnly, "read-only"},
		{flagWriteOnly, "write-only"},
		{flagVolatile, "volatile"},
		{flagGrabbed, "grabbed"},
		{flagSlider, "slider"},
		{flagUpdate, "update"},
	} {
		if f&flag.bit != 0 {
			names = append(names, flag.name)
		}
	}
	return names
}
