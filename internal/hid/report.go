// Package hid talks to the EMEET PIXY's vendor HID interface (usage page
// 0x83, report ID 0x09) through a hidraw node.
//
// The protocol was reverse-engineered by the community; see
// https://github.com/nille/omarchy-emeet-pixy (docs/PROTOCOL.md, MIT) and
// https://github.com/Romonaga/PixyPilot. Every report is 32 bytes including
// the report ID:
//
//	09 GG AA BB 00 LL 00 LL <payload>   (zero-padded)
//
// GG is the command group, AA BB the command, and LL the payload length,
// which appears twice. Replies echo bytes 1–3; the camera sets high bits in
// the group byte, so it is compared masked with 0x1f.
package hid

import "fmt"

const (
	// ReportID is the vendor report's ID, the first byte of every report.
	ReportID = 0x09
	// ReportSize is the length of an output report including its ID.
	ReportSize = 32
	// readSize is the buffer size for input reports; they are 32 bytes, but
	// a larger buffer costs nothing and avoids truncation surprises.
	readSize = 64

	headerSize = 8
	maxPayload = ReportSize - headerSize
	groupMask  = 0x1f
)

// Build returns a report for command group/a/b with the given payload:
// 09 group a b 00 len 00 len payload…, zero-padded to ReportSize. A query
// without payload (e.g. 09 01 01 01) is the same report with length 0.
func Build(group, a, b byte, payload ...byte) ([]byte, error) {
	if len(payload) > maxPayload {
		return nil, fmt.Errorf("hid: payload of %d bytes exceeds %d", len(payload), maxPayload)
	}
	r := make([]byte, ReportSize)
	r[0], r[1], r[2], r[3] = ReportID, group, a, b
	r[5] = byte(len(payload))
	r[7] = byte(len(payload))
	copy(r[headerSize:], payload)
	return r, nil
}

// mustBuild is Build for the package's own constant reports.
func mustBuild(group, a, b byte, payload ...byte) []byte {
	r, err := Build(group, a, b, payload...)
	if err != nil {
		panic(err)
	}
	return r
}

// Matches reports whether reply answers request: same report ID, same group
// (masked with 0x1f, since replies set high bits) and the same two command
// bytes. Matching the command bytes matters: a group holds several commands,
// and a set's acknowledgement must not be taken as a query's answer.
func Matches(request, reply []byte) bool {
	if len(request) < 4 || len(reply) < 4 {
		return false
	}
	return reply[0] == ReportID &&
		reply[1]&groupMask == request[1]&groupMask &&
		reply[2] == request[2] && reply[3] == request[3]
}
