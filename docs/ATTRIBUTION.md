# Attribution and third-party notices

This page records the upstream projects, reference material and third-party assets that capsulad
uses, reimplements or ships. Per-package notices live beside the code that carries them, for
example [apps/airplay/NOTICE](../apps/airplay/NOTICE) and
[internal/sounds/COPYING](../internal/sounds/COPYING).

## AirPlay 2 receiver

- **capsula-mini-airplay2** (https://github.com/ryysel/capsula-mini-airplay2) — the AirPlay
  receiver behaviour this app reimplements: the compact-speaker model string, the stock-profile
  latency intent, the cover-art ring rendering and the single-volume transport interaction.
  Upstream packages Shairport Sync 5.6.2 and NQPTP 1.2.9 plus its own installers; none of that
  is included or redistributed here, and [apps/airplay](../apps/airplay) is an independent Go
  implementation.
- **Shairport Sync** (https://github.com/mikebrady/shairport-sync) and **NQPTP**
  (https://github.com/mikebrady/nqptp) — the reference for AirPlay wire behaviour (RTSP method
  handling, HAP pairing key derivation, RTP/RTCP and timing packet layouts, PTP message layout).
  The AirPlay 2 receiver reproduces the fixed FairPlay handshake reply constants from
  `rtsp.c` (`handle_fp_setup`), which is MIT-licensed: "Copyright (c) James Laird 2013;
  modifications copyright (c) Mike Brady 2014–2026. Permission is hereby granted, free of
  charge, to any person obtaining a copy of this software and associated documentation files
  (the 'Software'), to deal in the Software without restriction … The above copyright notice
  and this permission notice shall be included in all copies or substantial portions of the
  Software." No other code from either tree is included; NQPTP is GPLv2 and nothing GPL is
  linked or copied.
- **FFmpeg** (https://ffmpeg.org) — the AirPlay 2 receiver links `libavcodec`/`libavutil` for AAC
  decoding, the same decoder the upstream receiver links. FFmpeg is LGPL-2.1-or-later; the app
  uses only the AAC decoder and links the libraries, and the deployment ships them as separate
  shared objects where the platform does not already provide them.

## Hardware sources

The Amlogic A113X facts come from cmp18's published survey (debianlive.ru, 2026-08-23),
`ryysel/capsula-mini-airplay2`, `ryysel/marusya-hw` and `byrbyrka/capsula-mini-tweaker`;
[HARDWARE.md](HARDWARE.md) names them and marks what is third-party evidence rather than a
measurement in this repository.

## Framework assets

- **Canonical** — the `ubuntu-sounds` 0.3 set used for the framework's UI cues
  ([internal/sounds](../internal/sounds)), licensed CC BY-SA. The licence text is
  [internal/sounds/COPYING](../internal/sounds/COPYING) and the files are unmodified.
- **Material Symbols Rounded** (https://fonts.google.com/icons) — the Web UI serves a
  generated subset of the Material Symbols Rounded variable font (90 glyphs; axes intact),
  Apache-2.0, copyright Google LLC. The font is unmodified apart from subsetting; the source
  package, exact glyph list and hashes are recorded in
  [web/src/assets/icon-font.json](../web/src/assets/icon-font.json). The licence text is
  [web/src/assets/LICENSE-material-symbols.txt](../web/src/assets/LICENSE-material-symbols.txt)
  and is also shipped with the embedded Web UI assets, which serve it at
  `/LICENSE-material-symbols.txt` with the notices in `/THIRD-PARTY.md`.
- **Tailwind CSS** and **daisyUI** — the generated settings-document kit embeds the full MIT
  licence texts for the installed versions, repeats the notices in its `kit.css` banner, and
  serves the texts at `/LICENSE-tailwindcss.txt` and `/LICENSE-daisyui.txt` with both listed in
  `/THIRD-PARTY.md`. See [DEVELOPMENT.md](DEVELOPMENT.md#web-ui).

## Wakeword reference material

**station-wakeword** (https://github.com/tangenx/station-wakeword) — reference material for the
station wake-word work that the personal wake-word pipeline was developed and validated against.
No source from that project is vendored here; the detection and training evidence is in
[WAKEWORD.md](WAKEWORD.md).

## Cadence

**Cadence** — the voice-assistant platform Cadence Client (`apps/cadence`) speaks to (device link
protocol `cadence/v2`: WebSocket `GET /v2/device`, protobuf `Envelope` frames, bearer device
token, Opus 16 kHz mono uplink, Opus reply and announcement streams; enrollment at
`POST /v2/enroll`). Source: https://git.shino.boo/devilreef/cadence/ (self-hosted, GitLab).
The client implements the wire protocol against that upstream definition; no upstream source code
is vendored and no vendor binary is redistributed. Its protobuf files are an independently written
subset of cadence.v2 (same field numbers and enums), and its `.binpb` test vectors are
upstream-generated data from upstream `main` commit
`f9571ad0d89acd7326414adf04a85ea1c9dec131`. The announcement flow, enrollment `/v2/enroll`
contract and board ids were checked against upstream `main` commit
`9a6b257fd6e21b8f0e725461af332489fb11e383`; that revision adds the `vk-capsula-mini` speaker
model and optional trace fields only.
