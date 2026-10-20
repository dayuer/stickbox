<div align="center">

# StickBox

**Turn any TV into a Claude usage dashboard — with an ESP32-S3 stick plugged into its USB port.**

[![License: MIT](https://img.shields.io/github/license/dayuer/stickbox)](LICENSE)
[![Latest release](https://img.shields.io/github/v/release/dayuer/stickbox)](https://github.com/dayuer/stickbox/releases/latest)
![ESP-IDF 5.5](https://img.shields.io/badge/ESP--IDF-5.5-red)
![Go 1.26](https://img.shields.io/badge/Go-1.26-00ADD8)

English · [简体中文](README.zh-CN.md)

<img src="docs/design/screenshots/02-%E5%AE%89%E5%85%A8.png" alt="StickBox dashboard rendered on a 1080p TV" width="720">

<sub>Design render of the 1080p dashboard.</sub>

</div>

StickBox shows how much of your **Claude subscription** you have used — the 5-hour and 7-day
windows, how fast you are burning them, and whether you will run out before the reset — as a big,
glanceable board on the TV in the room. The stick only draws power from the TV's USB port; the
picture goes over your home Wi-Fi.

## Features

- **Usage dashboard on a TV** — 5-hour / 7-day quota, time progress, burn rate and a suggested
  limit, rendered in 1080p with a flip clock.
- **No app, no phone number, no account on someone else's cloud** — pair with a 6-digit code shown
  on the TV, then run one small agent on your computer.
- **Your Claude credentials never leave your machine** — the agent runs `claude -p /usage`
  locally (it does not consume quota) and uploads only the numbers.
- **Seamless 1080p over DLNA** — the board finds the TV with SSDP and pushes an endless MJPEG
  stream; content changes by swapping frames, so there is no toolbar and no flash back to the
  home screen.
- **Works without PSRAM** — the 1080p frame is split into independently decodable JPEG strips
  (restart markers) and only the strips that changed are re-encoded.
- **Photo slideshow (optional)** — with an SD card, photos uploaded from the agent are
  interleaved with the dashboard.
- **Signed over-the-air updates** — RSA-3072 signed firmware, A/B slots, automatic rollback.
- **Self-hostable** — the server is a single Go binary plus one SQLite file; it embeds its own
  MQTT broker.

## How it works

```
your computer              server (Go)               ESP32-S3 stick            TV
┌────────────────┐  HTTPS  ┌────────────────┐  MQTT   ┌─────────────────┐  DLNA  ┌──────┐
│ stickbox-agent │ ──────▶ │   stickboxd    │ ──────▶ │ render + stream │ ─────▶ │ 1080p│
│ claude /usage  │ token   │ stores latest  │  TLS    │ MJPEG over HTTP │  SSDP  │ MJPEG│
└────────────────┘         │ usage, renders │         └─────────────────┘        └──────┘
                           │ dashboard base │
                           └────────────────┘
```

Anthropic offers no third-party authorization for subscription usage, so the agent uses *your own*
local Claude login. The server issues the agent a scoped token and never sees your Claude
credentials. Details: [`docs/AGENT.md`](docs/AGENT.md), [`docs/SECURITY.md`](docs/SECURITY.md).

## Hardware and TV compatibility

| | Tested |
|---|---|
| Board | Espressif **ESP32-S3-USB-OTG** (USB power, 240×240 status LCD, optional SD slot) |
| TV | Samsung UA60DU7000 (2024) — DLNA MJPEG 1080p, 3-minute soak with no disconnects |

DLNA behaviour differs between TV brands and even firmware versions. Before buying hardware, run
the compatibility checker against your TV and consider sending us the report:

```bash
cd server/go && go run ./cmd/stickbox-tvcheck --help
```

The compatibility matrix lives in [`docs/SUPPORTED-TVS.md`](docs/SUPPORTED-TVS.md).

## Quick start

You need a supported board, a DLNA-capable TV on the same network, and the
[Claude Code](https://claude.com/claude-code) CLI logged in on your computer.

### 1. Flash the firmware

Download `stickbox-firmware.zip` from the [latest release](https://github.com/dayuer/stickbox/releases/latest)
and flash the files listed in its `flasher_args.json` with `esptool`. A static in-browser flasher
(ESP Web Tools, Chrome or Edge) is provided in [`web/flash/`](web/flash/); generate its manifest
with [`rom/tools/gen_web_manifest.py`](rom/tools/gen_web_manifest.py) and host the folder yourself.

On first boot the stick opens a `StickBox-Setup-XXXX` Wi-Fi hotspot; connect with a phone and the
setup page pops up. Hold the MENU key (GPIO14) at boot to clear Wi-Fi settings.

> The official firmware talks to the author's hosted server. To use your own server, build the
> firmware yourself with your own address — see [Self-hosting](#self-hosting).

### 2. Install the agent

Download the binary for your platform (macOS, Linux or Windows; amd64 or arm64) from the release
page, check it against `SHA256SUMS`, then:

```bash
stickbox-agent pair 123456   # the 6-digit code shown on the TV
stickbox-agent pair          # ...or no code: opens a local web page to enter it (or auto-find the screen)
stickbox-agent run --once    # send one report to verify everything works
stickbox-agent install       # run in the background at login (no sudo)
stickbox-agent status        # server, pairing, last success, claude login state
stickbox-agent doctor        # diagnose problems
```

Optional commands: `tz set Asia/Jakarta` for your time zone and `photo add / ls / rm / init` for
the SD-card slideshow. Full reference: [`docs/AGENT.md`](docs/AGENT.md).

## Self-hosting

The server is one Go binary (`stickboxd`) and one SQLite file.

```bash
cd server/go
go test ./... && go build ./cmd/stickboxd
```

Deployment, TLS, backups and upgrades are covered in [`server/DEPLOY.md`](server/DEPLOY.md) and
[`docs/OPS.md`](docs/OPS.md). Point the firmware at your server:

```bash
source ~/esp/esp-idf-v5.5/export.sh
cd rom
cp main/cloud_creds.h.example main/cloud_creds.h   # fill in your server address
idf.py build
idf.py -p /dev/cu.usbmodemXXXX flash
```

Releases are signed with a private key that stays with the maintainer; if you build your own
firmware, generate your own signing key — see [`docs/RELEASE.md`](docs/RELEASE.md).

## Repository layout

| Path | What it is |
|---|---|
| [`rom/`](rom/) | ESP32-S3 firmware (ESP-IDF 5.5): Wi-Fi provisioning, MQTT, DLNA streaming, rendering, LCD status screen, OTA |
| [`server/go/`](server/go/) | `stickboxd` server, `stickbox-agent`, `stickbox-tvcheck` and other tools |
| [`web/flash/`](web/flash/) | Browser-based flasher |
| [`docs/`](docs/) | Design, protocol and operations documentation |
| [`probe/`](probe/) | Early DLNA/TV experiments (reference only) |

## Documentation

Most documents are written in Chinese.

| Topic | Document |
|---|---|
| HTTP / MQTT API | [`docs/API.md`](docs/API.md) |
| Dashboard frame protocol | [`docs/FRAME.md`](docs/FRAME.md) |
| Decisions and constraints | [`docs/DECISIONS.md`](docs/DECISIONS.md) |
| Photo slideshow | [`docs/PHOTO-USER.md`](docs/PHOTO-USER.md), [`docs/PHOTO.md`](docs/PHOTO.md) |
| Board acceptance tests | [`docs/BOARD-ACCEPTANCE.md`](docs/BOARD-ACCEPTANCE.md) |
| Releasing | [`docs/RELEASE.md`](docs/RELEASE.md) |
| Security model | [`docs/SECURITY.md`](docs/SECURITY.md) |
| Design mockups | [`docs/design/`](docs/design/) |

## Status

Current release: **v1.1.x**. The scope is the Claude usage dashboard on an ESP32-S3 stick, and the
project is in maintenance mode: bug fixes are welcome, large new features are out of scope.

## Contributing

Issues and pull requests are welcome.

- Open an issue before large changes. Please include your board, TV model and firmware version for
  hardware problems; `stickbox-agent status` output helps for agent problems.
- Server and agent changes must pass `cd server/go && go vet ./... && go test ./... -race`.
- Firmware changes must pass `rom/tools/bandtest/run.sh` and be verified on a real board; please
  state which board and TV you tested with.
- Build firmware from a clean export (`git archive`), not from a working tree that contains your
  local `cloud_creds.h`.
- Never commit secrets (`cloud_creds.h`, signing keys, tokens).

## Security

Please do not report vulnerabilities in a public issue; contact the maintainer privately through
GitHub instead. The token model is described in [`docs/SECURITY.md`](docs/SECURITY.md).

## License

[MIT](LICENSE) © 2026 dayuer.

Third-party components keep their own licenses — for example ESP-IDF (Apache-2.0), LVGL (MIT) and
the Go modules listed in [`server/go/go.mod`](server/go/go.mod). The embedded CJK bitmap font
(`rom/main/font_cn_16.c`) is generated from a third-party typeface; if you redistribute commercially,
regenerate it from a font whose license you have verified (for example an OFL-licensed Noto Sans SC).
