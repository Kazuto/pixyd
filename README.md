# pixyd

pixyd controls an [EMEET PIXY](https://emeet.com) webcam on Linux, where EMEET's
own software doesn't run. It is a small background daemon with a web UI: switch
between standard, tracking and privacy mode, adjust the image, and pan, tilt and
zoom while watching a live preview.

It talks to the camera on two paths: the vendor HID interface for the mode, and
standard UVC controls through V4L2 for everything else. One static Go binary,
no cgo, no root.

## Features

- **Modes:** standard, tracking (the camera follows people) and privacy (the lens
  physically closes), with the camera's state polled every 1.5 s and pushed to
  the page.
- **Image and PTZ controls:** generated from what the camera reports, with
  ranges, defaults and reset buttons. Pan and tilt are shown in degrees, zoom as
  1.00×–1.50×. Controls an automatic mode overrides (exposure time, white
  balance, focus) are disabled, with a link to switch to manual.
- **Preview:** the page shows the PIXY itself (picked by name, not the laptop's
  own camera) and lets go of it whenever the tab is hidden, so video calls can
  have it. If another app holds the camera, the page says which one.
- **Unplugging:** pixyd reports the camera as disconnected and picks it up again
  when it comes back, without a restart.
- **JSON API** for scripts and status bars, in the same shape as
  `pixy-track --json`.

## Requirements

- Linux with `uvcvideo` and hidraw, and a systemd user session.
- Access to the camera's hidraw node. It is root-only by default; this udev rule
  gives it to the logged-in user:

  ```bash
  echo 'SUBSYSTEM=="hidraw", ATTRS{idVendor}=="328f", ATTRS{idProduct}=="00c0", TAG+="uaccess"' \
    | sudo tee /etc/udev/rules.d/70-emeet-pixy.rules
  sudo udevadm control --reload && sudo udevadm trigger --subsystem-match=hidraw
  ```

  The video nodes usually get the same access already. Without the rule, image
  and PTZ controls work and the mode buttons say why they don't.
- Go 1.26 to build.
- A browser with camera support for the UI. `localhost` counts as a secure
  context, so the camera preview needs no certificate.

## Install

```bash
make install
```

This builds `bin/pixyd`, installs it as `~/.local/bin/pixyd`, installs the user
unit as `~/.config/systemd/user/pixyd.service`, and enables and starts it. It
starts with every login from then on. Open <http://127.0.0.1:7499/>.

`make install` again after pulling changes rebuilds and restarts it. `PREFIX`
(default `~/.local`) and `UNITDIR` change the destinations.

Logs: `journalctl --user -u pixyd -f`.

### Uninstall

```bash
make uninstall
```

stops and disables the service and removes the binary and the unit. The udev
rule stays; remove `/etc/udev/rules.d/70-emeet-pixy.rules` yourself if you added
it only for pixyd.

## Using it

The first visit asks for camera access. The browser may open its default camera
for a moment to learn the camera names, then switches to the PIXY.

**Standard and tracking only switch while an app streams video.** The camera
reports the same value for both while nothing streams from it, and it ignores
switching between them. The page's own preview counts as a stream, so with the
page visible everything works. A switch requested while the camera is idle
(from the API, or with the preview paused) is kept as *pending* and applied once
the next time the camera streams, e.g. when a call starts. Privacy works either
way.

Pausing the preview, or hiding the tab, frees the camera for other apps.

## Configuration

| Flag      | Environment   | Default  | Description                                       |
| --------- | ------------- | -------- | ------------------------------------------------- |
| `-port`   | `PIXYD_PORT`  | `7499`   | Port on 127.0.0.1 (pixyd never listens elsewhere) |
| `-poll`   |               | `1.5s`   | Interval of the mode poll                         |
| `-hidraw` | `PIXY_HIDRAW` | found    | Use this hidraw node instead of finding it        |
| `-video`  | `PIXY_VIDEO`  | found    | Use this video node instead of finding it         |
| `-debug`  |               | off      | Debug logging                                     |

Nodes are found through sysfs on every poll, by USB ID (`328f:00c0`), so the
numbers may change between boots. An override that names a missing node means
"no camera", not a fallback.

For the service, `systemctl --user edit pixyd` and add for example:

```ini
[Service]
Environment=PIXYD_PORT=7500
```

## API

Everything is JSON on `http://127.0.0.1:7499`. Request bodies must be
`application/json`.

| Method | Path                        | Body                       | Returns              |
| ------ | --------------------------- | -------------------------- | -------------------- |
| GET    | `/api/state`                |                            | State                |
| GET    | `/api/events`               |                            | Server-sent events   |
| GET    | `/api/tracking`             |                            | Tracking             |
| POST   | `/api/tracking/toggle`      |                            | Tracking             |
| POST   | `/api/tracking/on`, `/off`  |                            | Tracking             |
| PUT    | `/api/mode`                 | `{"mode":"privacy"}`       | Tracking             |
| GET    | `/api/controls`             |                            | `{"controls":[…]}`   |
| PUT    | `/api/controls/{name}`      | `{"value":12}`             | `{"controls":[…]}`   |
| POST   | `/api/controls/reset`       | `{"names":["brightness"]}` | `{"controls":[…]}`   |

**Tracking** is what `pixy-track --json` prints, so a script can switch over
without changes:

```bash
curl -s -X POST http://127.0.0.1:7499/api/tracking/toggle
```

```json
{"mode":"tracking","confirmed":true,"idle":false}
{"mode":"standard","confirmed":false,"idle":true,"pending":"standard"}
{"mode":"privacy","confirmed":true,"idle":false,"error":"not-switched","message":"camera did not switch to tracking"}
{"error":"permission","message":"no permission to open /dev/hidraw3; install the udev rule from the README"}
```

`mode` is `standard`, `tracking`, `privacy`, or `null` if unknown. While the
camera is `idle`, `mode` is the pending or last confirmed mode and `confirmed`
is false. Errors are `not-found`, `permission`, `no-answer` and `not-switched`;
the HTTP status is 503, 503, 504 and 409.

**State:**

```json
{
  "connected": true,
  "camera": {"name": "EMEET PIXY", "card": "EMEET PIXY: EMEET PIXY", "usb": "7-1.2.4",
             "speed": "480M", "video": "/dev/video4", "hidraw": "/dev/hidraw3"},
  "tracking": {"mode": "tracking", "confirmed": true, "idle": false},
  "videoUsers": [{"pid": 4242, "name": "chrome"}]
}
```

`videoUsers` are the processes (of your user) that have the video node open.

**Control:**

```json
{"id": 10094856, "name": "pan_absolute", "label": "Pan, Absolute", "class": "camera",
 "type": "int", "min": -540000, "max": 540000, "step": 3600, "default": 0, "value": 0,
 "flags": []}
```

`name` follows `v4l2-ctl`. `type` is `int`, `bool`, `menu`, `intmenu` or
`button`; menus carry `"menu":[{"value":0,"label":"Disabled"},…]`. `flags` may
hold `inactive`, `read-only`, `write-only`, `volatile` and `grabbed`. Values are
clamped and snapped to the step before they are written; the response is what
the camera reads back. Writing an inactive control answers 409.

Pan and tilt are in arc-seconds (3600 per degree), zoom in hundredths.

**Events:** `/api/events` sends `state` and `controls` events, each a complete
snapshot as above: the current ones on connect, then every change.

### Security

pixyd only listens on 127.0.0.1. Since every website can make the browser send
requests to localhost, it also rejects:

- a `Host` other than `127.0.0.1:PORT` or `localhost:PORT` (DNS rebinding),
- an `Origin` other than `http://127.0.0.1:PORT` or `http://localhost:PORT`, and
  `Sec-Fetch-Site` other than `same-origin` or `none` (cross-site requests),
- request bodies that aren't JSON (form posts, which skip the CORS preflight).

It sends no CORS headers. Requests without `Origin`, like curl's, are allowed:
anything that runs as your user can use the camera anyway.

## How it works

- **Discovery:** `/sys/class/hidraw/*` and `/sys/class/video4linux/*` are traced
  to their USB device; the hidraw node whose report descriptor declares usage
  page 0x83 is the vendor interface, and of the two video nodes the one whose
  `device_caps` has video capture is the capture node.
- **HID:** reports are 32 bytes, `09 GG AA BB 00 LL 00 LL <payload>`. pixyd keeps
  one descriptor open (the first write on a fresh one is dropped), drains stale
  input before each query, matches replies on group (masked with 0x1f) and
  command, retries queries 4× with 0.6 s timeouts, commits a mode set with a
  mode query, reads it back and repeats a set that didn't land once (the first
  one after a quiet period can be dropped). Privacy → tracking goes through
  standard.
- **V4L2:** raw ioctls (`VIDIOC_QUERYCTRL` with `V4L2_CTRL_FLAG_NEXT_CTRL`,
  `VIDIOC_QUERYMENU`, `VIDIOC_G_CTRL`/`S_CTRL`) through `golang.org/x/sys/unix`.
  The node is opened when needed and closed after 10 s; controls never take the
  stream, so they work during calls. HID PTZ commands are never used: they
  desync the UVC pan/tilt readback.
- **Who uses the camera:** inotify on the video node, then a scan of
  `/proc/*/fd`.

Measured on a PIXY behind a USB 2.0 dock: a mode switch takes about 0.3 s,
privacy → tracking 0.7 s.

## Development

```bash
make test     # unit tests against a simulated camera, no hardware needed
make lint
make run      # foreground, debug logging
make hwtest   # read-only checks against the connected camera
```

`PIXYD_HW=mode go test -v -run Hardware ./internal/hid` also cycles through
every mode, privacy included, and restores the starting one. It needs an app
streaming from the camera.

Layout:

```
cmd/pixyd         flags, wiring, shutdown
internal/sysfs    finding the camera and pairing its nodes
internal/hid      vendor HID reports, retries and mode logic (hidtest: simulated camera)
internal/v4l2     controls through raw ioctls
internal/camera   supervisor: polling, hotplug, pending mode, video users, events
internal/server   API, SSE, Host/Origin guard
web               embedded UI: index.html, style.css, app.js
deploy            systemd user unit
```

## Troubleshooting

- **"No HID access":** the udev rule is missing or the camera was plugged in
  before it was added; replug it.
- **"The browser doesn't list the EMEET PIXY":** check the site's camera
  permission, and that the camera shows up in `ls /sys/class/video4linux`.
- **"The camera is in use by …":** another app is streaming. Controls still work;
  the preview starts once it lets go.
- **Standard/tracking don't change:** see [Using it](#using-it); the camera has to
  be streaming.

## Next steps

Not in this first version:

- Presets (the camera's three slots; loading them over UVC, since the vendor load
  command does nothing on some firmware)
- Gesture control, microphone DSP mode, flip and auto-rotate, the auto-privacy
  timeout (commands are documented but not verified on this camera yet)
- Recording and snapshots
- A v4l2loopback output
- The vendor extension unit (unit 2, 10 selectors of unknown meaning)
- Quickshell integration: moving the tracking tile from `pixy-track` to the API
- Freeing the camera automatically when another app asks for it while the
  preview is visible

## Credits

The protocol knowledge comes from the community:
[nille/omarchy-emeet-pixy](https://github.com/nille/omarchy-emeet-pixy)
(docs/PROTOCOL.md, MIT) and
[Romonaga/PixyPilot](https://github.com/Romonaga/PixyPilot) (read as reference;
no code taken).

Bundled: [Inter](https://rsms.me/inter/) and
[JetBrains Mono](https://www.jetbrains.com/lp/mono/) (SIL Open Font License 1.1,
`web/fonts/LICENSE-*.txt`), icons from [Lucide](https://lucide.dev) (ISC,
`web/LICENSE-lucide.txt`). The UI theme follows wallgrade.

## License

GPL-3.0, see [LICENSE](LICENSE).
