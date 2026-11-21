# Shared assets

The framework provides one shared, read-only **asset store** for user media and other files that
must be available to several apps. It is a plain directory, not a package: there is no asset
manifest, no license metadata and no per-app visibility rule. Every regular file below the store
root is an asset.

| Item | Value |
|---|---|
| Host directory | `<data-dir>/assets` (on the stock device `/data/capsdemo/assets`) |
| App environment | `CAPSULAD_ASSETS` — `/assets` in namespace sandboxes, the host path in `uid`/`none` modes |
| Canonical app path | `/assets/<asset-id>` |
| Access | Read-only for every app |
| Writer | The daemon's session-authenticated Web UI routes only |

How it relates to the package:

- Files inside your package are immutable app data at `/app` (`CAPSULAD_APP_DIR`). Use them for
  defaults, templates, icons and fallback audio. A QuickJS app reads one with
  `capsulad.readPackageFile("<slash-relative path>")`; native apps open them relative to their
  executable path.
- The asset store is user-managed media shared by all apps. Use it for recordings, imported
  audio, images and other files a user adds at runtime.
- An app cannot write the store. To change it, an administrator uploads through the Web UI.

## Catalog

`AssetService.ListAssets{prefix}` returns the catalog; it never returns file contents.

| Field | Meaning |
|---|---|
| `prefix` (request) | Matches the **start** of `asset_id`. Empty lists everything. A `/assets/...` prefix is accepted as an alias. |
| `asset_id` | Slash-relative path below the store root, for example `audio/replies/hello.wav`. |
| `path` | Canonical in-sandbox path: `/assets/audio/replies/hello.wav`. |
| `media_type` | Derived from the extension, never sniffed. |
| `sha256_hex` | Lowercase SHA-256 of the file. |
| `size_bytes` | File size. |

Entries are sorted by `asset_id`. The store is enumerated on every call, so a file added by an
administrator or copied onto the device is visible without restarting the daemon. Symlinks,
directories, special files and in-progress upload staging files (`.capsulad-upload-…`) are never
published. A file that cannot be hashed is skipped rather than reported with a partial hash.

```json
→ {"prefix":"audio/"}
← {"assets":[
     {"assetId":"audio/replies/hello.wav","path":"/assets/audio/replies/hello.wav",
      "mediaType":"audio/wav","sha256Hex":"b4c0…","sizeBytes":"184320"},
     {"assetId":"audio/ding.mp3","path":"/assets/audio/ding.mp3",
      "mediaType":"audio/mpeg","sha256Hex":"9f21…","sizeBytes":"20480"}
   ]}
```

Read the bytes from the mount. The catalog is not a download API:

```go
var list struct {
    Assets []struct {
        AssetID   string `json:"assetId"`
        Path      string `json:"path"`
        MediaType string `json:"mediaType"`
        SHA256    string `json:"sha256Hex"`
        Size      uint64 `json:"sizeBytes"`
    } `json:"assets"`
}
err := client.CallJSON(ctx, "", "capsulad.framework.v1.AssetService", "ListAssets",
    map[string]any{"prefix": "audio/"}, &list, 0)
if err != nil {
    log.Fatalf("list assets: %v", err)
}
root := os.Getenv("CAPSULAD_ASSETS") // "/assets" inside the sandbox
for _, a := range list.Assets {
    pcm, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(a.AssetID)))
    if err != nil {
        log.Printf("%s: %v", a.AssetID, err)
        continue
    }
    play(pcm)
}
```

A shell or Python app uses the same path:

```sh
ls "$CAPSULAD_ASSETS/audio"
```

## Media types

The extension table is short and explicit. Anything else is `application/octet-stream`. Content
is never sniffed, so a `.bin` WAV stays `application/octet-stream`; name files with the correct
extension.

| Extension | Media type |
|---|---|
| `wav` | `audio/wav` |
| `mp3` | `audio/mpeg` |
| `ogg` | `audio/ogg` |
| `flac` | `audio/flac` |
| `png` | `image/png` |
| `jpg`, `jpeg` | `image/jpeg` |
| `gif` | `image/gif` |
| `svg` | `image/svg+xml` |
| `webp` | `image/webp` |
| `json` | `application/json` |
| `txt` | `text/plain` |
| `html` | `text/html` |
| `css` | `text/css` |
| `js` | `text/javascript` |
| `bin` | `application/octet-stream` |

## Administrator uploads

These routes exist only on the HTTPS Web UI. They require an authenticated administrator
**browser session** plus a CSRF token and a same-origin request. A static API key is not accepted
for any asset route — a `status` key is limited to `GET /api/status` and the public certificate.

| Method | Route | Behavior |
|---|---|---|
| `GET` | `/api/assets` | `{assets:[…], total_bytes}` for the whole store. |
| `POST` | `/api/assets?path=<asset-id>` | Raw request body written to that asset ID. Creates parent directories, replaces an existing regular file atomically through a staging file and rename. |
| `DELETE` | `/api/assets?path=<asset-id>` | Removes one regular file. Directories and symlinks are never removed. |

`POST` returns `{"asset": {…}}` with the published catalog entry. Errors are explicit:
`400` for an invalid path, `413` when the file exceeds 32 MiB or the store is full, `404` for a
missing asset, `403` for a permission problem, `507` when the device filesystem is full.

## Path and size limits

| Limit | Value |
|---|---|
| One uploaded asset | 32 MiB |
| Whole store | 48 MiB |
| Asset ID | 4096 bytes |
| One path component | 255 bytes |
| Separator | `/` only; backslashes are rejected |
| Reserved | Components beginning `.capsulad-upload-` |
| Rejected components | absolute paths, empty components, `.`, `..`, NUL bytes |

Parent directories are created as needed. An existing parent that is a symlink or not a
directory is rejected. Upload staging happens inside the destination directory and is renamed
into place, so readers never observe a partial file; a failed upload leaves the previous file
intact.

The aggregate budget is below the 78.5 MiB device data partition because the same partition holds
apps, settings and logs.

## Choosing between package files and the store

| Need | Use |
|---|---|
| Default sound, icon, template or fallback that ships with the app | Package file under `/app`. |
| User recording, imported music, downloaded image | Asset store. |
| File that several apps must see | Asset store. |
| File that must be present at first launch with no administrator action | Package file. |
| File the app writes | The app's own package directory is read-only; the store is read-only too. There is no writable app data directory in the current sandbox contract. Keep generated state in settings, or re-upload media through the administrator. |

If an app references a user asset for an optional feature, treat a missing asset as a normal
state: fall back to a packaged default, or show a clear message. Do not fail startup because a
user file is absent.

The assistant app demonstrates the pattern: it accepts a `reply_asset` setting, validates it
against the live catalog, accepts only `audio/wav`, and uses its packaged fallback when the
setting is empty or the file has disappeared.

## Limits

| Limit | Current behavior |
|---|---|
| App writes to `/assets` | Not possible; the mount is read-only. |
| Asset download API | None. Read the file from `CAPSULAD_ASSETS`. |
| Content sniffing | None; media type follows the extension. |
| Per-app asset visibility | None; every app sees the whole store. |
| Directory removal | Not exposed. Empty directories may remain after deletes. |
| Symlinks | Never followed and never published. |
| Atomic replacement | Yes, for a single file upload. |
| Restart needed after an upload | No; the catalog is enumerated live. |
| Nested app-specific namespaces | Convention only (`<app-id>/…` or a feature prefix); nothing enforces isolation. |