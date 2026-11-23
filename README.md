# `capsulad`
![Core build status badge](https://img.shields.io/github/actions/workflow/status/m4l3vich/capsulad/release-core.yml?label=core%20build) ![Apps build status badge](https://img.shields.io/github/actions/workflow/status/m4l3vich/capsulad/release-apps.yml?label=apps%20build)

![VK Capsula Homebrew badge](https://img.shields.io/badge/VK_Capsula_Homebrew-blue?logo=vk) ![Golang badge](https://img.shields.io/badge/Golang-00ADD8?logo=go&logoColor=FFF) ![Heavily Vibecoded badge](https://img.shields.io/badge/Heavily_vibecoded-FF5555?logo=claude&logoColor=FFF)

[На русском](README_RU.md)

Universal framework and application runtime for jailbroken VK Capsula smart speakers.
It replaces stock `voice-agent` and reimplements its core logic:

- Bluetooth speaker mode
- Wakeword training and detection (supports multiple simultaneous wakewords for different assistants!)
- Marusia assistant (if you want her back for some reason…)
- Wi-Fi networks management
- Volume control and microphone toggle

It's also a platform for applications — you can make your own applications packaged as:
1. Native binaries (Go and Rust bindings available)
2. Python programs (via included MicroPython runtime)
3. JavaScript programs (via included QuickJS runtime)

For ease of development, all of the hardware and inter-application communication is abstracted and unified. Developer docs are available [here](docs/app_development/README.md).

Can't program? It's ok, everyone is vibecoding these days. The `capsulad` project is like 85% vibecoded too. There is an MCP server built into the `capsulad` so the AI agent you're using can do everything on your speaker on their own. Just give them a link to this repo and ask them to install the `capsulad` MCP server, they'll figure it out.
# Installation

>[!IMPORTANT]
>Currently only Rockchip and Amlogic Capsula Minis are supported. Fat Capsula support is coming very soon. Like, I hope that it's here less than a week after initial release.

You need:
1. VK Capsula Mini with either jailbroken stock software or Alpine Linux (Amlogic only)
	- To jailbreak your Capsula Mini, use this: https://github.com/librecapsula/capsula-jb
2. An computer
3. `capsulad` installer for your platform available on [releases page](https://github.com/m4l3vich/capsulad/releases)

Download and launch the capsulad installer binary and follow the instructions.
# Huge thanks
- **Canonical** — the `ubuntu-sounds` 0.3 set used for the framework's UI cues (`internal/sounds`), licensed CC BY-SA. The licence text is [internal/sounds/COPYING](internal/sounds/COPYING) and the files are unmodified.
- **Hardware sources** — the Amlogic A113X facts come from cmp18's published survey (debianlive.ru, 2026-08-23), `ryysel/capsula-mini-airplay2`, `ryysel/marusya-hw` and `byrbyrka/capsula-mini-tweaker`; [docs/HARDWARE.md](docs/HARDWARE.md) names them and marks what is third-party evidence rather than a measurement in this repository.
- **station-wakeword** ([https://github.com/tangenx/station-wakeword](https://github.com/tangenx/station-wakeword)) — reference material for the station wake-word work that the personal wake-word pipeline was developed and validated against. No source from that project is vendored here; the detection and training evidence is in [docs/WAKEWORD.md](docs/WAKEWORD.md). The author is also my dear friend so that's nice.
- **Cadence** — the voice-assistant platform Cadence Client (`apps/cadence`) speaks to (device link protocol `cadence/v2`: WebSocket `GET /v2/device`, protobuf `Envelope` frames, bearer device token, Opus 16 kHz mono uplink, Opus reply and announcement streams; enrollment at `POST /v2/enroll`). Source: [https://git.shino.boo/devilreef/cadence/](https://git.shino.boo/devilreef/cadence/) (self-hosted, GitLab). The author is also my dear friend too! I have at least two friend so that's very nice.
# First-party apps
The `capsulad` comes with two built-in apps:
- **Clock** — reimplementation of stock segment display clock.
	- Set as a default segment display controller app.
	- It has a stopwatch, a timer and an ability to set alarms, all available through either the Web UI or via your voice assistant of choice (except Marusia…)
- **Ring** — an app to display effects on the LED ring
	- Set as a default LED ring controller app.
	- Has various effects available, a customisable palette and a on/off schedule (so you can use it as a nightlight, for example)

There is also some optional apps you can choose to install during `capsulad` setup:
## Media players
- **AirPlay 2 receiver** — a client for Apple AirPlay that allows you to play media from your Apple devices via Wi-Fi.
- **Internet radio** — plays MP3 streams over the internet. If you want to feel old for some reason.
## Voice assistants
- **Marusia** — a reimplementation of the default assistant on Capsula speakers.
- **Dumb Assistant** — it pretends to listen to you, but always plays back only a single customisable WAV file in response.
- **Cadence Client** — a client for https://git.shino.boo/devilreef/cadence, a highly customisable AI voice agent. It is very cumbersome to deploy the server, so here's a [simple guide](deploy/capsulad-cadence-server/README.md) on how to do that.
- **OpenCapsula Client** — a client for https://github.com/yawaflua/OpenCapsule, a known AI voice agent implementation for VK Capsulas.
# Smart home
- **Home Assistant integration** — expose the Capsula's hardware to your Home Assistant server via MQTT.
- **PA Speaker** — play WAV files on your speaker through a HTTP API. Useful for announcements.
# Funsies
- **Simon Says** — A game that shines the LED ring's sides yellow/green/red/blue whilst playing a tone and you need to repeat the pattern by touching the corresponding sides of the touch panel.
- **Soundboard** — You can upload some WAV sound effects and configure them in this app that, when launched, will play the selected sound when you touch the middle of the touch panel. Switch sounds by swiping left/right across the touch panel.