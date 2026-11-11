package server

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Kazuto/pixyd/internal/camera"
	"github.com/Kazuto/pixyd/internal/hid"
	"github.com/Kazuto/pixyd/internal/hid/hidtest"
	"github.com/Kazuto/pixyd/internal/sysfs/sysfstest"
)

const port = 7499

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// startCamera runs a Camera on a fake sysfs and simulated HID camera and
// waits for its first successful poll.
func startCamera(t *testing.T, sim *hidtest.Camera) *camera.Camera {
	t.Helper()
	tree := sysfstest.Machine(t)
	cam := camera.New(camera.Options{
		SysRoot:  tree.Root,
		ProcRoot: t.TempDir(),
		Poll:     5 * time.Millisecond,
		Timing: hid.Timing{
			Gap: time.Microsecond, QueryTimeout: time.Millisecond, QueryAttempts: 4,
			Settle: time.Microsecond, Retry: time.Microsecond, PrivacyExit: time.Microsecond,
		},
		Logger:     discard,
		OpenHID:    func(string) (hid.Transport, error) { return sim.Open(), nil },
		ProbeVideo: func([]string) (string, string, error) { return "", "", camera.ErrNoVideo },
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { cam.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.Now().Add(2 * time.Second)
	for !cam.State().Tracking.Confirmed {
		if time.Now().After(deadline) {
			t.Fatalf("camera never became ready: %+v", cam.State())
		}
		time.Sleep(time.Millisecond)
	}
	return cam
}

func newHandler(t *testing.T) (http.Handler, *hidtest.Camera) {
	sim := hidtest.New(hid.Tracking, true)
	cam := startCamera(t, sim)
	ui := fstest.MapFS{"index.html": {Data: []byte("<!doctype html><title>pixyd</title>")}}
	return New(cam, Options{Port: port, Logger: discard, UI: ui}), sim
}

type req struct {
	method, path, body string
	header             map[string]string
}

func do(h http.Handler, r req) *httptest.ResponseRecorder {
	var body io.Reader
	if r.body != "" {
		body = strings.NewReader(r.body)
	}
	hr := httptest.NewRequest(r.method, r.path, body)
	hr.Host = "127.0.0.1:" + strconv.Itoa(port)
	if r.body != "" {
		hr.Header.Set("Content-Type", "application/json")
	}
	for k, v := range r.header {
		if k == "Host" {
			hr.Host = v
		} else {
			hr.Header.Set(k, v)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, hr)
	return w
}

func TestGuard(t *testing.T) {
	h, _ := newHandler(t)
	p := strconv.Itoa(port)
	for _, tt := range []struct {
		name   string
		r      req
		status int
	}{
		{"plain", req{method: "GET", path: "/api/state"}, 200},
		{"localhost", req{method: "GET", path: "/api/state", header: map[string]string{"Host": "localhost:" + p}}, 200},
		{"host case", req{method: "GET", path: "/api/state", header: map[string]string{"Host": "LocalHost:" + p}}, 200},
		{"rebinding", req{method: "GET", path: "/api/state", header: map[string]string{"Host": "evil.example:" + p}}, 403},
		{"other port", req{method: "GET", path: "/api/state", header: map[string]string{"Host": "127.0.0.1:8080"}}, 403},
		{"no port", req{method: "GET", path: "/api/state", header: map[string]string{"Host": "127.0.0.1"}}, 403},
		{"ipv6", req{method: "GET", path: "/api/state", header: map[string]string{"Host": "[::1]:" + p}}, 403},
		{"same origin", req{method: "POST", path: "/api/tracking/on", header: map[string]string{"Origin": "http://127.0.0.1:" + p}}, 200},
		{"localhost origin", req{method: "POST", path: "/api/tracking/on", header: map[string]string{"Origin": "http://localhost:" + p}}, 200},
		{"foreign origin", req{method: "POST", path: "/api/tracking/on", header: map[string]string{"Origin": "https://evil.example"}}, 403},
		{"other localhost port", req{method: "POST", path: "/api/tracking/on", header: map[string]string{"Origin": "http://localhost:3000"}}, 403},
		{"https origin", req{method: "POST", path: "/api/tracking/on", header: map[string]string{"Origin": "https://localhost:" + p}}, 403},
		{"null origin", req{method: "POST", path: "/api/tracking/on", header: map[string]string{"Origin": "null"}}, 403},
		{"cross-site fetch", req{method: "GET", path: "/api/state", header: map[string]string{"Sec-Fetch-Site": "cross-site"}}, 403},
		{"same-site fetch", req{method: "GET", path: "/api/state", header: map[string]string{"Sec-Fetch-Site": "same-site"}}, 403},
		{"same-origin fetch", req{method: "GET", path: "/api/state", header: map[string]string{"Sec-Fetch-Site": "same-origin"}}, 200},
		{"typed URL", req{method: "GET", path: "/", header: map[string]string{"Sec-Fetch-Site": "none"}}, 200},
		{"form post", req{method: "PUT", path: "/api/mode", body: `{"mode":"standard"}`, header: map[string]string{"Content-Type": "text/plain"}}, 415},
		{"urlencoded post", req{method: "PUT", path: "/api/mode", body: `mode=standard`, header: map[string]string{"Content-Type": "application/x-www-form-urlencoded"}}, 415},
		{"curl without body", req{method: "POST", path: "/api/tracking/off"}, 200},
		{"too large", req{method: "PUT", path: "/api/mode", body: `{"mode":"` + strings.Repeat("x", maxBody) + `"}`}, 413},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := do(h, tt.r)
			if w.Code != tt.status {
				t.Errorf("status %d, want %d: %s", w.Code, tt.status, w.Body)
			}
			if w.Header().Get("Content-Security-Policy") == "" || w.Header().Get("X-Frame-Options") != "DENY" {
				t.Error("security headers missing")
			}
		})
	}
}

func TestTrackingEndpoints(t *testing.T) {
	h, sim := newHandler(t)
	for _, tt := range []struct {
		r      req
		status int
		body   string
	}{
		{req{method: "GET", path: "/api/tracking"}, 200, `{"mode":"tracking","confirmed":true,"idle":false}`},
		{req{method: "POST", path: "/api/tracking/toggle"}, 200, `{"mode":"standard","confirmed":true,"idle":false}`},
		{req{method: "POST", path: "/api/tracking/toggle"}, 200, `{"mode":"tracking","confirmed":true,"idle":false}`},
		{req{method: "POST", path: "/api/tracking/off"}, 200, `{"mode":"standard","confirmed":true,"idle":false}`},
		{req{method: "POST", path: "/api/tracking/on"}, 200, `{"mode":"tracking","confirmed":true,"idle":false}`},
		{req{method: "PUT", path: "/api/mode", body: `{"mode":"privacy"}`}, 200, `{"mode":"privacy","confirmed":true,"idle":false}`},
		{req{method: "PUT", path: "/api/mode", body: `{"mode":"tracking"}`}, 200, `{"mode":"tracking","confirmed":true,"idle":false}`},
		{req{method: "PUT", path: "/api/mode", body: `{"mode":"idle"}`}, 400, ""},
		{req{method: "PUT", path: "/api/mode", body: `{"mode":"standard","extra":1}`}, 400, ""},
		{req{method: "PUT", path: "/api/mode", body: `{"mode":"standard"} {}`}, 400, ""},
		{req{method: "POST", path: "/api/tracking/status"}, 404, ""},
		{req{method: "GET", path: "/api/tracking/toggle"}, 404, ""},
	} {
		w := do(h, tt.r)
		if w.Code != tt.status {
			t.Errorf("%s %s %s: status %d, want %d: %s", tt.r.method, tt.r.path, tt.r.body, w.Code, tt.status, w.Body)
			continue
		}
		if got := strings.TrimSpace(w.Body.String()); tt.body != "" && got != tt.body {
			t.Errorf("%s %s %s:\n got %s\nwant %s", tt.r.method, tt.r.path, tt.r.body, got, tt.body)
		}
	}
	if sim.Mode() != hid.Tracking {
		t.Errorf("camera ended in %v", sim.Mode())
	}

	// The camera stops answering: pixy-track's error shape and a 504.
	sim.Silent = true
	w := do(h, req{method: "GET", path: "/api/tracking"})
	if w.Code != 504 || strings.TrimSpace(w.Body.String()) != `{"error":"no-answer","message":"camera did not answer"}` {
		t.Errorf("silent camera: %d %s", w.Code, w.Body)
	}
}

func TestControlsEndpoints(t *testing.T) {
	h, _ := newHandler(t)
	for _, tt := range []struct {
		r      req
		status int
		kind   string
	}{
		{req{method: "GET", path: "/api/controls"}, 503, `"error":"not-found"`},
		{req{method: "PUT", path: "/api/controls/brightness", body: `{"value":12}`}, 503, `"error":"not-found"`},
		{req{method: "PUT", path: "/api/controls/brightness", body: `{"value":1.5}`}, 400, `"error":"bad-request"`},
		{req{method: "PUT", path: "/api/controls/brightness", body: `{"value":"12"}`}, 400, `"error":"bad-request"`},
		{req{method: "PUT", path: "/api/controls/brightness", body: `{}`}, 400, `"error":"bad-request"`},
		{req{method: "POST", path: "/api/controls/reset", body: `{"names":[]}`}, 400, `"error":"bad-request"`},
		{req{method: "GET", path: "/api/nope"}, 404, `"error":"not-found"`},
	} {
		w := do(h, tt.r)
		if w.Code != tt.status || !strings.Contains(w.Body.String(), tt.kind) {
			t.Errorf("%s %s %s: %d %s, want %d with %s", tt.r.method, tt.r.path, tt.r.body, w.Code, w.Body, tt.status, tt.kind)
		}
	}
}

func TestStaticUI(t *testing.T) {
	h, _ := newHandler(t)
	w := do(h, req{method: "GET", path: "/"})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "<title>pixyd</title>") {
		t.Fatalf("GET / = %d %s", w.Code, w.Body)
	}
	if w.Header().Get("Cache-Control") != "no-cache" {
		t.Error("UI is cacheable")
	}
}

func TestEvents(t *testing.T) {
	sim := hidtest.New(hid.Tracking, true)
	cam := startCamera(t, sim)
	ts := httptest.NewUnstartedServer(nil)
	ts.Config.Handler = New(cam, Options{
		Port:      ts.Listener.Addr().(*net.TCPAddr).Port,
		Logger:    discard,
		Heartbeat: 10 * time.Millisecond,
	})
	ts.Start()
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/events", nil)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type %q", ct)
	}

	// Collect events as they come; wait checks a condition on the latest
	// snapshot of each name.
	lines := bufio.NewScanner(resp.Body)
	latest := map[string]string{}
	wait := func(what string, cond func(map[string]string) bool) {
		t.Helper()
		var event string
		for !cond(latest) {
			if !lines.Scan() {
				t.Fatalf("stream ended waiting for %s: %v (latest %v)", what, lines.Err(), latest)
			}
			line := lines.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				latest[event] = strings.TrimPrefix(line, "data: ")
			}
		}
	}

	wait("the first state", func(m map[string]string) bool { return m["state"] != "" })
	if !strings.Contains(latest["state"], `"tracking":{"mode":"tracking","confirmed":true`) {
		t.Fatalf("first state event: %s", latest["state"])
	}
	wait("controls", func(m map[string]string) bool { return m["controls"] != "" })
	if !strings.Contains(latest["controls"], `"error":"not-found"`) {
		t.Fatalf("controls event: %s", latest["controls"])
	}
	cam.SetMode(hid.Privacy)
	wait("privacy", func(m map[string]string) bool { return strings.Contains(m["state"], `"mode":"privacy"`) })
}
