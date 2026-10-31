---
paths:
  - pkg/controllers/library*.go
  - pkg/moonlight/library*.go
  - pkg/moonlight/rest.go
  - cmd/operator/**
  - images/library/**
---

# Library (on-demand Steam + Heroic desktop)

- Per-user libraries (`User.spec.library`, XERK-2500): a User with one has their own home and
  games PVCs, Library pod `direwolf-library-<user>` (label `direwolf/library-owner`), auth Secret
  `direwolf-library-auth-<user>` and catalogue. Everyone else shares `--library-home-pvc`/`-games-pvc`.
  - Launchers allow one login per device and Proton prefixes live in the games volume
    (`steamapps/compatdata`), so two Users never share a home or a games volume.
  - The page picks the library from `--library-user-header` (Authentik's username), trusted only
    with the proxy secret. A name with no User, or an empty one, is refused (403): it would
    otherwise get the shared desktop, signed in to its users' accounts. No header at all gets the
    shared library, unless provisioning is on: then it is a misnamed `--library-user-header`
    and 403s (XERK-2544). Tests: `TestLibraryServerProvisionRefusals/no_user_header`.
  - Exception: with provisioning on, an eligible person (group or allowlist) gets a User on
    that visit (`.claude/rules/provisioning.md`).
  - The header's username maps to a User name by `v1alpha1.LookupUser`, as the provisioner
    creates them (`.claude/rules/provisioning.md`).
  - No two libraries may share a claim (`libraryClaimsConflict`, in `EnsurePod` and at pod build):
    each one's lock sees only its own Users, so two would let two Steams write one home.
    Of two Users on one claim the first created keeps it, so a newcomer can't lock out a working one.
  - A session of such a User mounts their claims wherever the App mounts the shared ones
    (`useUserLibrary`); Apps keep naming the shared claims.
  - One password per library: each desktop runs its user's code and can read its own.
  - Tests: `pkg/controllers/library_users_test.go`, `pkg/moonlight/library_users_test.go`.
- Steam lock: a Library and the game sessions of its library's Users share one Steam home, so
  they never run together. Other libraries' sessions don't count; a session whose User is gone
  counts for every library.
  - Operator side, a pod also counts if it mounts the library's home or games claim, whatever
    its User's library is now: `spec.library` can change under a running game (QA, XERK-2500).
    That covers the split App pod, which carries `direwolf/user` but no session label.
  - Session side, a pod is never built on a claim a running Library mounts (`libraryOpenOn`):
    the proxy's check sees only the User's own Library, not one opened before claims moved.
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
  - GoW's cont-init died on a dangling `~/.steam/steam` when `~/homebrew/services/PluginLoader`
    was missing (its Decky install's `mkdir -p ~/.steam/steam/`); the session image cuts that
    block (XERK-2128), but Steam's Debian launcher still needs the link to resolve.
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
- Xodus Nexus (Microsoft Store games) is unpacked from its AppImage (no FUSE in pods); unpacked it
  never self-updates (Nexus updates only a running AppImage). `NEXUS_VERSION` + `NEXUS_SHA512`
  pin the image's baseline; bump both together.
  - `xodus-nexus-update` (identical in `images/gow-nvidia32`, XERK-2288) installs the newest
    launcher release at Library start (`init-xodus-nexus-update`), like `init-heroic-update`:
    cached in the home, trusted only by GitHub's published SHA256, home touched only as abc.
  - The repo also publishes `backend-v*` and `runtime-*` releases (about three per launcher
    release): never use `releases/latest`; a cached version is fetched by its tag, not paged for.
  - The AppImage is unpacked as the desktop user, then chowned to root and made world-readable.
  - Sessions run it `--cached-only` (cont-init `30-xodus-nexus-update.sh`): the Library's verified
    copy, never a download at launch, so a session runs the Library's version.
  - `/opt/xodus-nexus.hold` (in both images) stops `xodus-nexus-update` entirely: the image's
    `NEXUS_VERSION` runs, and neither a newer release nor a newer cached copy is installed. Use it
    to hold back a broken release; its text says why. Remove it with the pin bump to a fixed one.
  - Tests: `images/library/xodus_nexus_update_test.go`.
  - `xodus-nexus` (identical in `images/gow-nvidia32`, `TestXodusNexusWrapperMatchesSessionImage`)
    starts gnome-keyring's secrets component: Nexus keeps the Microsoft account in a Secret
    Service keyring and neither image runs one. Export `GNOME_KEYRING_CONTROL` between the
    `--unlock` and `--start` calls, or the second starts a locked daemon that prompts.
  - The login keyring is in the shared home with an empty password, so it is stored unencrypted,
    like Steam's and Heroic's tokens there. Never print or copy `~/.local/share/keyrings`.
  - Installs go to the games volume: `init-xodus-nexus` (root) makes `/games/xodus-nexus` owned
    by abc, never following an existing link; the wrapper links `~/Games/Xodus Nexus` to it.
  - Nexus keeps its backend and runtime in the home (absolute `/config/...` paths), unpinned.
  - Nexus installs its backend itself on first run, but never its runtime: without one every
    launch fails "Select a Wine/Xodus Proton binary". `init-xodus-nexus-runtime` (as abc) installs
    the newest runtime release when `<runtimeRoot>/current.json` has no working wine, the way
    Nexus's `installNexus` does (XERK-2289). Install-if-missing only; updates stay in Settings.
    - Its source order is Nexus's `checkNexus`: domsvoboda/nexus-runtime (`nexus-runtime-*`) first,
      domsvoboda/xodus-nexus (`xodus-nexus-runtime-*`) only when that repo 404s or has no release.
      Any other release-list error installs nothing (XERK-2499): the fallback runtimes are older.
    Tests: `images/library/xodus_nexus_runtime_test.go`.
  - Nexus updates its backend only on first run or from Settings, and a launcher on a newer launch
    contract (`linux-run-vNN`) refuses to play with an older backend. `init-xodus-nexus-backend`
    (as abc, after `init-xodus-nexus-update`) installs the newest `backend-v*` release the
    launcher accepts, as Nexus's own update does (XERK-2354).
    - Minimum version and contract come from the installed launcher's lock inside `app.asar`. A
      newer release on another contract (published minutes before its launcher) is passed over
      for the newest on the launcher's; a locally built backend (`delivery: local`) is left alone.
    - "Up to date" means Nexus can play with the pointer: both binaries present, matching the
      release's `metadata.json` hashes, and on the launcher's contract. A damaged release
      directory is replaced, not given up on.
    - Keep both images' `NEXUS_VERSION` on the launch contract of the newest backend: a session
      that falls back to the image's launcher refuses to play with a newer-contract backend.
    - Tests: `images/library/xodus_nexus_backend_test.go` (builds a real x86_64 ELF as xodus-cli).
  - The wrapper exports `APPDIR`: AppRun otherwise finds its directory by testing `"$dir/$1"`,
    so any argument sends it to `/xodus-nexus`. The build makes the unpacked tree world-readable
    (`--appimage-extract` leaves 0700 root dirs) and fails if any entry is not.
  - The wrapper runs Nexus on X11 via the desktop's rootless Xwayland: under Wayland its sign-in
    window (created hidden, shown on `ready-to-show`) never appeared and sign-in hung (XERK-2287).
    labwc's children get `DISPLAY=:0`; `kubectl exec` and startwm carry a dead `:1`.
  - `wrapped-steam` (the labwc autostart hook) starts it beside Heroic, unless `xodus-nexus.bin`
    already runs. Tests: `TestWrappedSteamKeepsRunningNexus`.
  - The Library's selkies-desktop menu lists only `.desktop` files; labwc's right-click menu is
    the openbox `menu.xml`, so ship both.
- Battle.net (`/usr/bin/battlenet`, `init-battlenet`) is in `.claude/rules/battlenet.md`.
- linuxserver's `init-selkies-config` copies the default `menu.xml` into the home only when it
  has none, then restores its own `menu.xml.bak` of that first copy at every start: menu items
  added later never reached an existing home (XERK-2363). `init-desktop-menu` (ordered before it
  via its `dependencies.d`) rewrites both from the image's default each start; home edits go.
  Tests: `images/library/init_desktop_menu_test.go`.
- `wrapped-steam` checks for a running Steam again under a per-pod `flock`, held until it
  execs Steam: two cold launches at once would otherwise both start Steam and Heroic.
  - No child may inherit the lock fd (`9>&-`): Heroic outlives the script, and every later cold
    launch would wait out the timeout. Tests: `images/library/wrapped_steam_test.go`.
  - Those tests `pgrep` host-wide: a Steam stub from another test binary on the host (another
    worktree's run) makes launches hand off and fails them. Never run two trees' tests at once.
  - A cold launch deletes `.cef-enable-remote-debugging` (Heroic writes it into the shared
    install), as session pods' cont-init does: Steam would serve its CEF debugger on the pod's
    127.0.0.1:8080 (XERK-2094). Tests: `TestWrappedSteamColdLaunchDeletesCEFDebugFlag`.
  - A cold launch also starts a guard that keeps deleting it while `steam` or `steam.sh` runs:
    Steam's exit-42 restart (`steam.sh` re-execs itself) skips wrapped-steam (XERK-2119).
    `steam.sh` alone runs for seconds between clients, so the pattern needs both.
    Tests: `TestWrappedSteamGuard*` (a relaunch waits out the last guard's lock: `-w`, not `-n`).
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
  lets any paired user of that library stop it, downloads or not: a tab left open elsewhere keeps
  it up for hours.
  - moonlight-proxy is internet-facing, so it holds no pod writes at all (chart RBAC test
    `TestMoonlightProxyHasNoPodWrites`); never grant it delete, exec, or patch (a pod's image is
    mutable). It annotates the requester's Pairing with the pod's UID
    (`LibraryCloseAnnotation`, XERK-2539); the operator then runs `stop()`, so a close works for
    every User, own library or not, with no per-User chart list.
  - The request names the UID, not the pod name: a request left on a Pairing never closes a
    later Library. The operator honours it only if the Pairing's User (read live) plays from
    that pod's library (`closeRequested`).
  - A refused request is cleared (JSON patch `test` + `remove`, so a newer one stays): kept, it
    would close the pod once its User moved to that library.
  - A failed User read fails the reconcile so it is retried: the client was already told
    "Closing". Never swallow it; the next look could be an idle timeout away. The idle check and
    scan still run first: a long API fault must not keep an idle Library up.
  - `WatchCloseRequests` enqueues the pod from a Pairing event; `generic.Controller` makes its
    queue in `NewController` so another informer's handler may enqueue before `Run`, race-free.
  - The pod's own preStop hook still runs the Steam shutdown and relink for deletes that skip
    `stop()` (manual, drain); keep `libraryTerminationGracePeriod` above the shutdown wait.
  - Its API calls are bounded (`libraryCallTimeout`): /applist is otherwise informer-only, and a
    hung API server must cost only the tile. An App holding `CloseLibraryAppID` wins over the tile.
  - Tests: `TestLibraryCloseRequest*`, `TestLibraryWatchCloseRequests` (real informers),
    `TestLibraryPodShutsSteamDownOnAnyDelete`;
    `TestPodLibrary*`, `TestCloseLibraryTile*`, `TestAppWithCloseLibraryIDShadowsTile` in
    `pkg/moonlight/library_test.go`.
- Only a user-activated page load (`Sec-Fetch-User: ?1`) or the stopped page's Start button
  (POST `libraryStartPath`) starts the Library.
  - Selkies calls `location.reload()` when its stream drops, so a tab left open across an idle
    shutdown would restart it at once. A plain GET or a WebSocket must never start it.
- The pod's nginx demands basic auth (`PASSWORD` from Secret `direwolf-library-auth[-<user>]`); only the
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
