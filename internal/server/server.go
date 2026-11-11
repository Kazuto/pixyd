// Package server serves pixyd's HTTP API, its event stream and the
// embedded UI on localhost.
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Kazuto/pixyd/internal/camera"
	"github.com/Kazuto/pixyd/internal/hid"
	"github.com/Kazuto/pixyd/internal/v4l2"
)

// Options configure the server.
type Options struct {
	// Port is the port the server listens on, on 127.0.0.1. Requests must
	// name it in their Host header.
	Port   int
	Logger *slog.Logger
	// UI is the embedded web UI.
	UI fs.FS
	// Heartbeat is the interval of SSE keep-alive comments, 20 s.
	Heartbeat time.Duration
}

const maxBody = 4 << 10

const csp = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
	"font-src 'self'; connect-src 'self'; media-src 'self' blob: mediastream:; " +
	"frame-ancestors 'none'; base-uri 'none'; form-action 'none'"

type server struct {
	cam       *camera.Camera
	log       *slog.Logger
	hosts     map[string]bool
	origins   map[string]bool
	heartbeat time.Duration
}

// New returns the handler for everything pixyd serves.
func New(cam *camera.Camera, opts Options) http.Handler {
	s := &server{
		cam:       cam,
		log:       opts.Logger,
		hosts:     map[string]bool{},
		origins:   map[string]bool{},
		heartbeat: opts.Heartbeat,
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.heartbeat <= 0 {
		s.heartbeat = 20 * time.Second
	}
	port := strconv.Itoa(opts.Port)
	for _, host := range []string{"127.0.0.1", "localhost"} {
		s.hosts[host+":"+port] = true
		s.origins["http://"+host+":"+port] = true
		if opts.Port == 80 {
			s.hosts[host] = true
			s.origins["http://"+host] = true
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/state", s.state)
	mux.HandleFunc("GET /api/events", s.events)
	mux.HandleFunc("GET /api/tracking", s.tracking)
	mux.HandleFunc("POST /api/tracking/{cmd}", s.trackingCommand)
	mux.HandleFunc("PUT /api/mode", s.setMode)
	mux.HandleFunc("GET /api/controls", s.controls)
	mux.HandleFunc("PUT /api/controls/{name}", s.setControl)
	mux.HandleFunc("POST /api/controls/reset", s.resetControls)
	var files http.Handler = http.NotFoundHandler()
	if opts.UI != nil {
		files = http.FileServerFS(opts.UI)
	}
	// Everything else: unknown API paths get a JSON 404 (a wrong method on
	// a known one gets the mux's 405), the rest is the UI.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/"):
			writeError(w, http.StatusNotFound, "not-found", "no such endpoint: "+r.Method+" "+r.URL.Path)
		case r.Method != http.MethodGet && r.Method != http.MethodHead:
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		default:
			w.Header().Set("Cache-Control", "no-cache")
			files.ServeHTTP(w, r)
		}
	})
	return s.guard(mux)
}

// guard rejects requests that other websites could make through the
// browser: a foreign Host (DNS rebinding), a foreign Origin or a
// cross-site fetch (CSRF), and bodies that are not JSON (form posts, which
// skip the CORS preflight). Requests without Origin, like curl's, pass.
func (s *server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Content-Security-Policy", csp)
		h.Set("Permissions-Policy", "camera=(self), microphone=(), geolocation=()")

		if !s.hosts[strings.ToLower(r.Host)] {
			s.reject(w, r, "unexpected Host header")
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && !s.origins[strings.ToLower(origin)] {
			s.reject(w, r, "cross-origin request")
			return
		}
		switch r.Header.Get("Sec-Fetch-Site") {
		case "", "same-origin", "none":
		default:
			s.reject(w, r, "cross-site request")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && (r.ContentLength != 0) {
			mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if mt != "application/json" {
				writeError(w, http.StatusUnsupportedMediaType, "bad-request", "request bodies must be application/json")
				return
			}
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		next.ServeHTTP(w, r)
	})
}

func (s *server) reject(w http.ResponseWriter, r *http.Request, why string) {
	s.log.Warn("rejected request", "why", why, "method", r.Method, "path", r.URL.Path,
		"host", r.Host, "origin", r.Header.Get("Origin"), "fetch-site", r.Header.Get("Sec-Fetch-Site"))
	writeError(w, http.StatusForbidden, "forbidden", why)
}

func (s *server) state(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.cam.State())
}

func (s *server) tracking(w http.ResponseWriter, _ *http.Request) {
	t := s.cam.Tracking()
	writeJSON(w, trackingStatus(t), t)
}

func (s *server) trackingCommand(w http.ResponseWriter, r *http.Request) {
	cmd := r.PathValue("cmd")
	switch cmd {
	case "toggle", "on", "off":
	default:
		writeError(w, http.StatusNotFound, "not-found", "unknown tracking command "+strconv.Quote(cmd)+" (want toggle, on or off)")
		return
	}
	t, err := s.cam.Command(cmd)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad-request", err.Error())
		return
	}
	writeJSON(w, trackingStatus(t), t)
}

func (s *server) setMode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Mode string `json:"mode"`
	}
	if !decode(w, r, &body) {
		return
	}
	m, err := hid.ParseMode(body.Mode)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad-request", err.Error())
		return
	}
	t := s.cam.SetMode(m)
	writeJSON(w, trackingStatus(t), t)
}

type controlList struct {
	Controls []v4l2.Control `json:"controls"`
}

func (s *server) controls(w http.ResponseWriter, _ *http.Request) {
	list, err := s.cam.Controls()
	s.writeControls(w, list, err)
}

func (s *server) setControl(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Value json.RawMessage `json:"value"`
	}
	if !decode(w, r, &body) {
		return
	}
	if len(body.Value) == 0 || string(body.Value) == "null" {
		writeError(w, http.StatusBadRequest, "bad-request", `missing "value"`)
		return
	}
	v, err := strconv.ParseInt(string(body.Value), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad-request", `"value" must be an integer`)
		return
	}
	list, err := s.cam.SetControl(r.PathValue("name"), v)
	s.writeControls(w, list, err)
}

func (s *server) resetControls(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Names []string `json:"names"`
	}
	if !decode(w, r, &body) {
		return
	}
	if len(body.Names) == 0 {
		writeError(w, http.StatusBadRequest, "bad-request", `"names" must list at least one control`)
		return
	}
	list, err := s.cam.ResetControls(body.Names)
	s.writeControls(w, list, err)
}

func (s *server) writeControls(w http.ResponseWriter, list []v4l2.Control, err error) {
	if err != nil {
		kind := camera.Classify(err)
		writeError(w, controlStatus(kind), kind, err.Error())
		return
	}
	if list == nil {
		list = []v4l2.Control{}
	}
	writeJSON(w, http.StatusOK, controlList{list})
}

// events streams "state" and "controls" snapshots as server-sent events:
// the latest of each on connect, then every change.
func (s *server) events(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)

	sub := s.cam.Subscribe()
	defer sub.Close()
	heartbeat := time.NewTicker(s.heartbeat)
	defer heartbeat.Stop()

	if _, err := io.WriteString(w, "retry: 2000\n\n"); err != nil {
		return
	}
	for {
		if err := rc.Flush(); err != nil {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
		case <-sub.Ready():
			for _, e := range sub.Take() {
				if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Name, e.Data); err != nil {
					return
				}
			}
		}
	}
}

func trackingStatus(t camera.Tracking) int {
	switch t.Error {
	case "":
		return http.StatusOK
	case camera.ErrNotSwitched:
		return http.StatusConflict
	case camera.ErrNoAnswer:
		return http.StatusGatewayTimeout
	}
	return http.StatusServiceUnavailable
}

func controlStatus(kind string) int {
	switch kind {
	case camera.ErrNotFound, camera.ErrPermission:
		return http.StatusServiceUnavailable
	case camera.ErrKindUnknownControl:
		return http.StatusNotFound
	case camera.ErrKindInvalidValue:
		return http.StatusBadRequest
	case camera.ErrKindRefused, camera.ErrKindBusy:
		return http.StatusConflict
	}
	return http.StatusBadGateway
}

// decode reads a JSON object into v, answering 400 itself on failure.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	dec.UseNumber()
	err := dec.Decode(v)
	if err == nil && dec.More() {
		err = errors.New("trailing data after the JSON object")
	}
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "bad-request", "request body too large")
		} else {
			writeError(w, http.StatusBadRequest, "bad-request", "invalid JSON: "+err.Error())
		}
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(append(data, '\n'))
}

func writeError(w http.ResponseWriter, status int, kind, message string) {
	writeJSON(w, status, struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}{kind, message})
}
