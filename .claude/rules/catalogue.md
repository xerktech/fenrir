---
paths:
  - pkg/controllers/catalogue*.go
  - pkg/controllers/gpu_claim.go
  - pkg/controllers/library.go
  - pkg/controllers/testdata/catalogue/**
  - pkg/api/v1alpha1/app.go
  - pkg/moonlight/rest.go
  - pkg/moonlight/apps_test.go
  - cmd/operator/**
---

# Catalogue (App per installed game) and App.spec.gpu

- The scan execs `tar` in the Library pod; the operator never mounts the library.
  - The game library is talos04 local-path and the operator runs on any node.
  - Games only change while the Library runs, so it scans every `catalogueScanInterval` while
    the pod is ready, and once more in `stop()` after `steam -shutdown` (manifests flushed).
  - A scan error never stops the Library or its shutdown (it would hold the Steam lock).
  - Art fetching shares `catalogueArtBudget` per periodic scan; the `stop()` scan fetches none.
- Fail closed on deletes: any exec, tar or parse error (store read mid-write) syncs nothing.
  - A Heroic store that is present but empty is an error; only an absent one means no games.
  - `tar -h` follows symlinks: without it a symlinked store archives as empty, and its games'
    Apps (with their per-game settings) get deleted. A dangling symlink fails the scan.
  - The scan runs as the desktop user (`s6-setuidgid abc`): exec is root, and `-h` would
    follow a planted symlink to a root-only file. Parse errors never quote file content.
  - As abc, an unreadable directory would make a glob come up empty (no games, Apps deleted);
    the script's `reachable` fails the scan instead, for the stores, the default libraries and
    each library in `libraryfolders.vdf` (both the `config/` and `steamapps/` copies, current
    and pre-2021 formats, absolute paths only; each vdf is itself `reachable` and size-capped).
    Never for every folder on the games volume:
    ext4's root-only `lost+found` would fail every scan.
  - Library paths come from a user-writable file; a relative one once hung `reachable`
    forever. The script runs under `timeout`: a cancelled exec leaves its process running. `TestCatalogueScanUnreadableDirFails` skips
    as root, so run it as a non-root user (CI is).
  - `$home/.local/share/Steam` is a link to the sessions' install (see `library.md`); the
    scan reads through it, so keep `tar -h` and never resolve or reject that link.
  - A launcher whose base App is missing is skipped: its Apps stay, never "uninstalled".
  - Only Apps labelled `direwolf/catalogue=<store>` are ever updated or deleted.
- Generated Apps are a copy of `--catalogue-steam-app` / `--catalogue-heroic-app`.
  - The base App's container must run `$DIREWOLF_LAUNCH_COMMAND` (a shell command line).
  - The base should be `hidden: true`; copies reset `hidden` to false on create.
  - `hidden` and `gpu` are per-game settings: set from the base on create, then the user's.
    Everything else (template, wolfConfig, volumes) follows the base on every scan.
  - A base App change also reaches existing copies without a scan (XERK-1963): `RunRefresh`
    (leader, on `BaseAppChanges` and every `catalogueRefreshInterval`) re-copies the base.
    It reads each game back from its App; the ID annotation goes into a shell line, so it is
    checked as a scan would and must still give the App's name. It creates/deletes nothing.
    Tests: `catalogue_refresh_test.go`.
  - Refresh lists in pages (`catalogueRefreshPageSize`): every App carries its cover, and one
    LIST of 300 with 200 KB covers held ~140 MiB at once, beside the App informer's copy
    (256Mi operator limit).
  - RomM's Catalogue runs `RunRefresh` too: its base change reaches the ROMs' Apps at once,
    even while RomM is down (a failed RomM scan syncs nothing).
- The Library's files are user-writable, so treat them as untrusted:
  - Heroic appNames must match `heroicAppName` (they go into a shell line and a URL).
  - Box art is fetched only over https from `artHosts`, every redirect re-checked, at most
    1 MiB, PNG/JPEG/WebP only. Anything else is SSRF whose reply lands in a readable App.
  - More than `catalogueMaxGames` games fails the scan.
  - Size caps: per manifest/store in the script and `readTar`, and `maxExecOutput` on every exec.
    `cappedBuffer` must not embed `bytes.Buffer`: its `ReadFrom` lets `io.Copy` skip the cap.
  - client-go only logs a failed stream write and returns no error, so `streamCapped` must turn
    an overflowed `cappedBuffer` into one; else a cut-off tar reads as fewer games, and deletes.
  - `vdfMaxDepth` bounds the parser's recursion: deep nesting is a Go stack overflow,
    which kills the whole operator, not just the scan.
  - The art client dials public addresses only (`publicAddressOnly`), and covers are capped at
    4096px a side (moonlight-proxy decodes them on every request).
- Xodus Nexus (`--catalogue-nexus-app`, store `nexus`): the scan reads
  `~/.config/Xodus Nexus/launcher-state.json` (Electron userData for productName "Xodus Nexus").
  - Only `installState: "ready"` games get an App; `incomplete` is a download in progress.
  - A missing file is no games; a file with no `games` list fails the scan. An empty list is
    no games: Nexus writes the file atomically (temp + rename), so it is never torn.
  - IDs must match `nexusGameID` (16 lowercase hex, Nexus's own `validId`): they go into the
    launch line `xodus-nexus --launch-game <id>`, which runs the game with no launcher window
    and quits when it exits, ending the session.
  - Covers come from `coverUrl`/`heroUrl` (Microsoft Store CDN, often scheme-relative `//`).
    `coverDataUrl` (an inline data: URL) is ignored: art is only ever fetched from `artHosts`.
- The CRD rejects an empty `appAssetWebP` (the fake clientset doesn't): a catalogue App shows
  the base App's cover until its own is fetched.
- A name held by a hand-made App is never overwritten; that game is skipped, with an error.
- Moonlight IDs are an FNV hash in 2^30..2^31-1, clear of hand-written Apps' small IDs.
- moonlight-proxy serves `appAssetWebP` as WebP, PNG or JPEG (Steam's CDN is JPEG).
- It serves every cover as PNG scaled to fit 600x800 (`fitBoxArt`): Epic's 1200x1600 took ~8s
  per request to encode at full size on talos04, and Moonlight showed no cover (XERK-1343).
- A JPEG cover is turned upright per its EXIF Orientation (`jpegOrientation`, stdlib-only),
  scaled before it is rotated so the copy stays box-art sized. `TestAppAssetHonoursJPEGOrientation`.
- A hidden App is neither listed nor launchable by ID.
- `App.spec.gpu` is a per-Session ResourceClaim `<session>-gpu` owned by the Session:
  `capacity.requests.memory` for a share, none for the whole card (NVIDIA DRA consumable shares).
  - Pods reference it by name, so a template's generated claims' GC doesn't cover it.
  - A claim still owned by an earlier same-named Session is waited out, never adopted.
  - Don't combine it with a template GPU claim: the pod would get two cards.
  - The CRD checks only the memory string's shape: `addAppGPUClaim` rejects a value over
    int64 ("1e1000" has Value() 0), ending the Session.
  - Wolf gets it directly, not only via the App's containers: testball has none (XERK-1676).
    Tests: `TestContainerlessAppGPUReachesWolf`.
- Tests: `pkg/controllers/catalogue_test.go` (fixtures in `testdata/catalogue`, run through
  the real scan script); `TestHiddenAppsAreNotListed`, `TestAppAssetServesJPEGAsPNG` in
  `pkg/moonlight/apps_test.go`.
