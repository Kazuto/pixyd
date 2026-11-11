// Command pixyd controls an EMEET PIXY webcam on Linux: tracking and
// privacy modes over the camera's vendor HID interface, image and PTZ
// controls over V4L2, through a web UI and a small JSON API on localhost.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/Kazuto/pixyd/internal/camera"
	"github.com/Kazuto/pixyd/internal/server"
	"github.com/Kazuto/pixyd/web"
)

// version is set at build time with -ldflags "-X main.version=…".
var version = "dev"

const defaultPort = 7499 // "PIXY" on a phone keypad

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	flags := flag.NewFlagSet("pixyd", flag.ContinueOnError)
	flags.Usage = func() {
		fmt.Fprintf(flags.Output(), "Usage: pixyd [flags]\n\n"+
			"Serves a UI and JSON API for an EMEET PIXY webcam on http://127.0.0.1:PORT.\n\nFlags:\n")
		flags.PrintDefaults()
	}
	port := flags.Int("port", envInt("PIXYD_PORT", defaultPort), "port on 127.0.0.1 (env PIXYD_PORT)")
	poll := flags.Duration("poll", 1500*time.Millisecond, "interval of the camera mode poll")
	hidraw := flags.String("hidraw", os.Getenv("PIXY_HIDRAW"), "use this hidraw node instead of finding it (env PIXY_HIDRAW)")
	video := flags.String("video", os.Getenv("PIXY_VIDEO"), "use this video node instead of finding it (env PIXY_VIDEO)")
	debug := flags.Bool("debug", false, "log debug messages")
	showVersion := flags.Bool("version", false, "print the version and exit")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *showVersion {
		fmt.Println("pixyd", version)
		return 0
	}
	if *port < 1 || *port > 65535 {
		fmt.Fprintf(os.Stderr, "pixyd: invalid port %d\n", *port)
		return 2
	}

	log := newLogger(*debug)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Listen before anything else, so a taken port fails right away.
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(*port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Error("can't listen", "addr", addr, "err", err)
		return 1
	}

	cam := camera.New(camera.Options{
		HIDRaw: *hidraw,
		Video:  *video,
		Poll:   *poll,
		Logger: log,
	})
	srv := &http.Server{
		Handler:           server.New(cam, server.Options{Port: *port, Logger: log, UI: web.FS}),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       time.Minute,
		// Request contexts end with ctx, which ends the event streams so
		// Shutdown doesn't wait for them.
		BaseContext: func(net.Listener) context.Context { return ctx },
		ErrorLog:    slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	var wg sync.WaitGroup
	wg.Go(func() { cam.Run(ctx) })
	wg.Go(func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdown); err != nil {
			log.Warn("shutdown", "err", err)
		}
	})

	log.Info("pixyd listening", "url", "http://"+addr+"/", "version", version)
	err = srv.Serve(ln)
	if !errors.Is(err, http.ErrServerClosed) {
		log.Error("serve", "err", err)
		stop()
		wg.Wait()
		return 1
	}
	wg.Wait()
	log.Info("pixyd stopped")
	return 0
}

// newLogger logs to stderr; under systemd, without timestamps, since the
// journal adds its own.
func newLogger(debug bool) *slog.Logger {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	opts := &slog.HandlerOptions{Level: level}
	if os.Getenv("JOURNAL_STREAM") != "" {
		opts.ReplaceAttr = func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		}
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}

func envInt(name string, fallback int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil {
		return v
	}
	return fallback
}
