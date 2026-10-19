---
paths:
  - pkg/controllers/library*.go
  - pkg/moonlight/library*.go
  - pkg/moonlight/rest.go
  - cmd/operator/**
  - images/library/**
---

# Library (on-demand Steam + Heroic desktop)

- Steam lock: the Library and game sessions share one Steam home, so they never run together.
  - Both sides check, create, then check again (`EnsurePod`; `createSession`'s post-create
    `BusyCheck`), so two racing starts can't both survive. Drop either recheck and they can.
  - Both checks read the API server (`List`), never an informer: the other side's object may
    be seconds old.
  - The operator side also counts session pods, not just Sessions: a terminating pod still
    runs Steam.
- One Steam install: `~/.local/share/Steam` (Valve's launcher, here) is a relative link to
  `~/.steam/debian-installation` (sessions' Debian launcher), made by the image's
  `init-steam-home` before the desktop starts (XERK-1559).
  - Each launcher points `~/.steam/steam` at its own dir by an absolute path. `/home/retro`
    (sessions' mount) links to `/config` here so the sessions' path resolves; Valve's
    launcher otherwise falls back to `~/.local/share/Steam`, and the Debian one relinks a
    dangling `/config/...` link to its own dir.
  - Never make `debian-installation` the link: the GoW image isn't ours.
  - A real `~/.local/share/Steam` next to it is moved aside (`Steam.library-<time>`), never
    deleted; games installed only there (StateFlags bit 4, as the catalogue reads it) are moved
    into the shared install first, or the catalogue would see them uninstalled and delete their
    Apps.
  - `init-steam-home` never exits non-zero: a failed oneshot stops s6 starting the desktop, and
    the pod stays Running, never Ready, holding the Steam lock.
  - Tests: `images/library/init_steam_home_test.go` (runs the real script).
  - GoW's cont-init dies on a dangling `~/.steam/steam` when `~/homebrew/services/PluginLoader`
    is missing (`mkdir -p ~/.steam/steam/`), i.e. a fresh home where the Library ran first.
    So `stop()` repoints every absolute `/config/...` link in `~/.steam` to `/home/retro/...`
    after `steam -shutdown` (`steamRelinkCommand`, XERK-1916); that path resolves in both pods.
    Tests: `TestSteamRelinkCommand*`, `TestLibraryStopRelinksSteamForSessions`.
  - A Library pod that ends without `stop()` (Failed, not Ready, node loss, a manual delete)
    skips that, so a session pod whose App mounts its own home at `/home/retro` runs the same
    script in the `steam-relink` init container (`steamRelinkContainer`, XERK-1951).
    - It copies the App's mount whole (readOnly too) but never the App's env (a `PATH` could
      hide `sh`), so a `subPathExpr` home, which kubelet expands from that env, is skipped.
    - It runs as uid 1000, never root, and only logs a failure: a failed init container ends
      the Session. Tests: `TestSessionPodRelinksSharedSteamHome`, `TestSteamRelinkContainerCommand`.
- Heroic updates at pod start: `init-heroic-update` installs the latest GitHub release when it
  is newer than the image's (`HEROIC_VERSION` is only the baseline); a .deb can't self-update.
  - The .deb is cached in the Steam home (`~/.cache/direwolf-heroic`), which user code can
    write: it is copied out, then installed only if it matches the SHA256 GitHub publishes for
    the asset. Root never touches the home directly (`s6-setuidgid abc`): planted links.
  - The cache read takes only a regular file, capped at the asset's size: a planted FIFO,
    `/dev/zero` or sparse file would stall the start or fill the node's disk.
  - Install with `apt-get install --no-remove <deb>`, never `dpkg -i` + `apt-get install -f`:
    `-f` resolves a dependency it can't fetch by removing Heroic.
  - Never exits non-zero, like `init-steam-home`; offline or any mismatch keeps the image's copy.
  - `heroic-postinstall` runs after every install (build and update): the package resets the
    launchers and chrome-sandbox that `wrapped-heroic` replaces.
  - Tests: `images/library/init_heroic_update_test.go` (real script, stubbed curl/dpkg).
    Stubs never reach a host package tool: TrueNAS's dpkg is a wrapper that refuses to run,
    so the dpkg stub compares versions itself (XERK-2008).
- `wrapped-steam` checks for a running Steam again under a per-pod `flock`, held until it
  execs Steam: two cold launches at once would otherwise both start Steam and Heroic.
  - No child may inherit the lock fd (`9>&-`): Heroic outlives the script, and every later cold
    launch would wait out the timeout. Tests: `images/library/wrapped_steam_test.go`.
- `wrapped-heroic` turns Heroic's "Add games to Steam" off in its config.json: Steam always runs
  beside Heroic here and its CEF debugger stays off, so adding or removing a shortcut fails with
  a "Steam is running" error. Removal on uninstall is unconditional in Heroic, so that error
  still shows on uninstall; it is cosmetic. Tests: `images/library/wrapped_heroic_test.go`.
- Idle rule: (no browser traffic for `--library-idle-timeout` OR no user-opened page load for
  `--library-max-runtime`) AND nothing downloading.
  - An open Selkies tab sends WebSocket traffic every few seconds with nobody there, so traffic
    alone never idles; the max-runtime clock counts only `userNavigation` loads and Start.
  - A listed download counts only while it progresses (files in steamapps/downloading or the
    Heroic store change, or the pod receives `libraryDownloadMinBytes`) within
    `libraryDownloadStallTimeout`. Paused downloads and stale Steam entries stay listed forever.
    - Received bytes are the only progress signal for Heroic: its store is not known to change
      while downloading. Unverified against real paused downloads (no launcher data on talos04).
    - Progress state is in memory on the leader; a new leader starts the stall clock afresh.
    - Exclude lo from the byte counts: it carries the whole nginx↔Selkies stream, so every
      open tab would count as progress.
    - Received bytes count only beyond a tenth of sent bytes (`libraryStreamAckRatio`): an open
      tab's ACKs grow with its bitrate, so no fixed threshold separates them from a download.
  - A replica flushes the visit annotation only when it saw a new visit, so an older visit never
    overwrites another replica's later one.
  - Activity lives on the pod (`direwolf/library-last-activity`), not in memory: every
    operator replica serves the page, only the leader runs the idle check.
  - Any doubt (exec failure, unparseable Heroic store) keeps the Library up.
  - Shutdown deletes the pod even if `steam -shutdown` fails: a stuck pod would hold the lock.
- Moonlight's "Close Library" tile (`CloseLibraryAppID`, listed only while the Library is open)
  lets any paired user stop it, downloads or not: a tab left open elsewhere keeps it up for hours.
  - moonlight-proxy is internet-facing, so it may only delete that one pod (chart RBAC
    `resourceNames`); never grant it exec, or patch (a pod's image is mutable).
  - So the pod's own preStop hook runs the Steam shutdown and relink `stop()` would; keep
    `libraryTerminationGracePeriod` above the shutdown wait. After `stop()` both are no-ops.
  - A delete skips `stop()`'s last catalogue scan; the next Library run picks those installs up.
  - Its API calls are bounded (`libraryCallTimeout`): /applist is otherwise informer-only, and a
    hung API server must cost only the tile. An App holding `CloseLibraryAppID` wins over the tile.
  - Tests: `TestLibraryPodShutsSteamDownOnAnyDelete`; `TestPodLibrary*`, `TestCloseLibraryTile*`,
    `TestAppWithCloseLibraryIDShadowsTile` in `pkg/moonlight/library_test.go`.
- Only a user-activated page load (`Sec-Fetch-User: ?1`) or the stopped page's Start button
  (POST `libraryStartPath`) starts the Library.
  - Selkies calls `location.reload()` when its stream drops, so a tab left open across an idle
    shutdown would restart it at once. A plain GET or a WebSocket must never start it.
- The pod's nginx demands basic auth (`PASSWORD` from Secret `direwolf-library-auth`); only the
  operator's proxy adds it. k8x's flannel does not enforce the chart's NetworkPolicy, so this
  password is the pod's real protection. Selkies itself binds localhost, behind nginx.
- The page is trusted only from `--library-trusted-proxies` (the Ingress behind Authentik).
- Every page request must also carry `X-Direwolf-Proxy-Secret` (`--library-proxy-secret-file`,
  >= 32 bytes, required with `--library-port`). On k8x the proxy is Authentik's outpost, a pod, so
  the trusted range is pod CIDRs, and flannel lets every pod there in. Never drop the secret check.
  - The proxy strips the header before the pod: the desktop runs arbitrary user code.
- The first exec into a just-started Library pod fails its stream upgrade (API server→kubelet
  dial: "use of closed network connection"). `retryUpgradeFailure` retries once on an upgrade
  failure only: the command almost never ran. Never retry other exec errors; the command may
  have run. A new Library exec must stay safe to run twice.
- Tests: `TestRetryUpgradeFailure`, `TestPodExecutorRetriesUpgradeFailure`, `TestLibraryIdleRule`,
  `TestLibraryDownloadStall`, `TestSteamDownloadsCommand`,
  `TestLibraryEnsurePod*`, `TestLibraryServer` in
  `pkg/controllers/library_test.go`; `TestLaunchBacksOutWhenBusyAfterCreate` in
  `pkg/moonlight/library_test.go`.
