package hid

import (
	"errors"
	"os"
	"sync"
	"time"
)

// Transport is one open hidraw descriptor.
type Transport interface {
	// Write sends one output report.
	Write(report []byte) error
	// ReadReport reads one input report into buf. It waits until deadline at
	// most and returns os.ErrDeadlineExceeded if nothing arrived; with a
	// deadline in the past it only returns a report that is already queued.
	ReadReport(buf []byte, deadline time.Time) (int, error)
	Close() error
}

// ErrNoAnswer means the camera did not answer a query.
var ErrNoAnswer = errors.New("camera did not answer")

// Timing holds the delays the camera needs; see DefaultTiming.
type Timing struct {
	// Gap separates consecutive writes and query attempts. The camera
	// acknowledges a report before it is ready for the next one.
	Gap time.Duration
	// QueryTimeout is how long one query attempt waits for its reply.
	QueryTimeout time.Duration
	// QueryAttempts bounds the retries. The first write on a freshly
	// opened descriptor is silently dropped, so one attempt is not enough.
	QueryAttempts int
	// Settle is the wait after committing a mode before reading it back.
	Settle time.Duration
	// Retry is the wait before repeating a set that did not land; the
	// first set after the camera was idle can be dropped.
	Retry time.Duration
	// PrivacyExit is the wait after leaving privacy before setting
	// tracking, which the camera ignores when sent from privacy directly.
	PrivacyExit time.Duration
}

// DefaultTiming are the delays verified against the camera by pixy-track.
var DefaultTiming = Timing{
	Gap:           25 * time.Millisecond,
	QueryTimeout:  600 * time.Millisecond,
	QueryAttempts: 4,
	Settle:        200 * time.Millisecond,
	Retry:         200 * time.Millisecond,
	PrivacyExit:   150 * time.Millisecond,
}

// Conn serializes requests on one Transport. Keep a Conn open: a fresh
// descriptor drops its first write, which costs a full QueryTimeout.
type Conn struct {
	mu     sync.Mutex
	t      Transport
	timing Timing

	// Replaceable in tests.
	now   func() time.Time
	sleep func(time.Duration)
}

// NewConn wraps t. A zero Timing means DefaultTiming.
func NewConn(t Transport, timing Timing) *Conn {
	if timing == (Timing{}) {
		timing = DefaultTiming
	}
	if timing.QueryAttempts < 1 {
		timing.QueryAttempts = 1
	}
	return &Conn{t: t, timing: timing, now: time.Now, sleep: time.Sleep}
}

// Close closes the transport.
func (c *Conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t.Close()
}

// Query sends request and returns the first reply that matches it. It drains
// stale input before every attempt, because an unread reply would otherwise
// be taken as the answer to the next query.
func (c *Conn) Query(request []byte) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.query(request)
}

func (c *Conn) query(request []byte) ([]byte, error) {
	buf := make([]byte, readSize)
	for attempt := range c.timing.QueryAttempts {
		if attempt > 0 {
			c.sleep(c.timing.Gap)
		}
		if err := c.drain(buf); err != nil {
			return nil, err
		}
		if err := c.t.Write(request); err != nil {
			return nil, err
		}
		deadline := c.now().Add(c.timing.QueryTimeout)
		for {
			n, err := c.t.ReadReport(buf, deadline)
			if errors.Is(err, os.ErrDeadlineExceeded) {
				break
			}
			if err != nil {
				return nil, err
			}
			// A reply carries at least one payload byte after the header.
			if n > headerSize && Matches(request, buf[:n]) {
				return append([]byte(nil), buf[:n]...), nil
			}
		}
	}
	return nil, ErrNoAnswer
}

// drain discards input that is already queued: unsolicited status reports
// and replies nobody read.
func (c *Conn) drain(buf []byte) error {
	for {
		_, err := c.t.ReadReport(buf, time.Time{})
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
