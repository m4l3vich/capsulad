# Third-party notices for the embedded Web UI

This directory is copied verbatim into the served SPA tree (`web/` builds into
`internal/webui/dist`, which the daemon embeds with `go:embed`). It carries the
notices for assets that are redistributed to browsers rather than shipped as
separate libraries. The same `web/public` notices and licences are copied into
`internal/webui/dist`, so a released daemon ships them at `/THIRD-PARTY.md`,
`/LICENSE-tailwindcss.txt` and `/LICENSE-daisyui.txt`.

## Tailwind CSS — settings-document kit

- **Component:** the stylesheet generated into `internal/appui/assets/kit.css`
  and embedded in the daemon, built from the `tailwindcss` npm package
  **3.4.17**.
- **Licence:** MIT. The full copyright and permission notice is
  [LICENSE-tailwindcss.txt](LICENSE-tailwindcss.txt).
- **Copyright:** Tailwind Labs, Inc.
- **Use:** utility CSS and the framework's preflight normalization, compiled
  locally into the settings-document kit; no runtime dependency and no network
  request.

## daisyUI — settings-document kit

- **Component:** the daisyUI component CSS in the same generated
  `internal/appui/assets/kit.css`, built from the `daisyui` npm package
  **4.12.23**.
- **Licence:** MIT. The full copyright and permission notice is
  [LICENSE-daisyui.txt](LICENSE-daisyui.txt).
- **Copyright:** 2020 Pouya Saadeghi.
- **Use:** component classes (`btn`, `card`, `input`, `select`, `textarea`,
  `checkbox`, `alert`, `badge`, …) and the two appliance themes; compiled
  locally, no CDN and no runtime dependency.

## Material Symbols Rounded — icon font subset

- **Asset:** `assets/material-symbols-rounded-subset.<hash>.woff2`, generated
  from the `material-symbols` npm package 0.48.3, Google's Material Symbols
  Rounded variable font (<https://fonts.google.com/icons>).
- **Licence:** Apache License 2.0. The full text is
  [LICENSE-material-symbols.txt](LICENSE-material-symbols.txt).
- **Copyright:** Google LLC. The subset retains the original `name` table
  entries, including the copyright notice.
- **Modification:** subset to the icon ligatures this console renders, their
  `.fill` alternates and the ASCII letter/digit components used by the
  ligature markup. All four variable axes (FILL, GRAD, opsz, wght) are
  retained; no outline was redrawn.
- **Provenance:** the source package version, exact glyph list and the source
  and output SHA-256 digests are recorded in `web/src/assets/icon-font.json`
  in the source tree, and `web/scripts/build-icon-font.py` regenerates both.