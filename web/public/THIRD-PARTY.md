# Third-party notices for the embedded Web UI

This directory is copied verbatim into the served SPA tree (`web/` builds into
`internal/webui/dist`, which the daemon embeds with `go:embed`). It carries the
notices for assets that are redistributed to browsers rather than shipped as
separate libraries.

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