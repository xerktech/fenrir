---
paths:
  - images/library/**
  - images/gow-nvidia32/**
  - pkg/controllers/library*.go
  - pkg/controllers/catalogue*.go
  - pkg/controllers/app_pod.go
---

# Battle.net (XERK-2359 epic; from the XERK-2360 hand-run spike, talos04, 2026-10-27)

## In the Library image (XERK-2361)

- `/usr/bin/battlenet` runs it with the image's `/opt/proton-cachyos` (pinned with its .sha512sum,
  root-owned) in the prefix `~/.local/share/battlenet`; its first start runs the image's
  hash-pinned `/opt/battlenet/Battle.net-Setup.exe`. Tests: `images/library/battlenet_test.go`.
  - Proton writes into its own dir only to build a missing `default_pfx`; the release ships one
    (the build checks), so it runs read-only. `user_settings.py` in the prefix is imported as
    code: run Proton only as abc, never root.
  - The installer's versioned URL 404s once Blizzard retires it: bump version + SHA256 together.
  - The operator sets `BATTLENET_GAMES` to `<--library-games-path>/battlenet` in the Library pod.
  - `init-battlenet` (root) makes `$BATTLENET_GAMES` for abc, never following a link, then runs
    `battlenet --prepare` as abc: creates the prefix and sets `UseTakeFocus=N`.
  - Each start with no client running forces `Client.HardwareAcceleration` off and, if unset,
    `Client.Install.DefaultInstallPath` to `Z:$BATTLENET_GAMES/`. That key name is unverified
    against Battle.net; if it ignores it, pick the folder in the install dialog.
  - The Library's labwc maximizes every window (`/defaults/labwc.xml`, rebuilt into rc.xml at each
    start). The fixed-size "Battle.net Login" window then fights the resize and its page never
    renders, so the image adds an UnMaximize rule for that title. Its class `steam_proton` is
    every Proton window's: never unmaximize by class alone.
  - `PROTON_FSR4_UPGRADE=0`: this Proton otherwise downloads a 65 MB DLL, outside the image's
    pin, into the prefix on every run.
  - No autostart beside Heroic: nothing needs the client running, and its CEF costs CPU.

## Managed WoW addons (XERK-2366)

- Chart `wowAddons` → operator env `LIBRARY_WOW_ADDONS` (`--library-wow-addons`, strict JSON,
  `ParseWoWAddons`) → Library env `WOW_ADDONS`, always set (`[]` for none).
- The Library's `svc-wow-addons` runs `wow-addons-update` as abc at start, then hourly; sessions
  never download addons, they read what the Library installed on the games volume.
- Unset or unreadable `WOW_ADDONS` leaves everything alone: read as "none", it would uninstall
  every managed addon. Keep both the operator's and the script's parse strict.
- Each flavor's `Interface/.direwolf-addons.json` records the folders it installed; anything else
  in AddOns is the user's and is never replaced or removed (a name clash refuses the install).
- Only GitHub releases, installed only when the asset matches GitHub's published SHA256. Zips are
  hostile: no absolute, `..`, backslash, link or root-level entries; capped size and entry count.
- Unpacked into `Interface/.direwolf-addons-*` (same filesystem), then swapped in by rename; a
  failed swap or manifest write puts the old folders back. Flavor dirs are never created.
- ConsolePort: `seblindfors/ConsolePort`, asset `ConsolePort-<version>.zip`, one zip for every
  flavor (multi-interface TOCs).
- Tests: `images/library/wow_addons_update_test.go` (real script, stubbed curl);
  `TestParseWoWAddons*` in `pkg/controllers/library_wow_addons_test.go`.

## Running it under Proton

- umu-run fails in the Library: pressure-vessel's bwrap can't create a user namespace there
  ("No permissions to create a new namespace"). Steam gets by via `~/.local/bin/proot-bwrap`.
- Heroic's `Proton-CachyOS-latest` (cachyos-11.0-20261005-slr) `proton run` works without a
  runtime, in the Library and in a heroic-image session. Set `STEAM_COMPAT_DATA_PATH` (spike
  prefix: `~/Games/Heroic/Prefixes/default/Battle.net`) and `STEAM_COMPAT_CLIENT_INSTALL_PATH`.
  - Without umu's `GAMEID`, protonfixes skips its fixes ("probably running a unit test"),
    so each fix below is applied by hand.
- `Battle.net.exe` and its setup are 32-bit (PE32) and run on this Proton's i386 unix side.
  - Without `libfreetype6:i386`/`libfontconfig1:i386`, every label is blank ("Wine cannot
    find the FreeType font library").
  - Without `libldap2:i386` (Lutris also lists krb5, sqlite3, gpg-error, pulse, dbus, Xrandr,
    Xcomposite, Xcursor, cups i386), the web sign-in succeeds, then the client's connection to
    `us.actual.battle.net:1119` fails: `ERROR_RPC_DISCONNECT (3018)`, BLZBNTBGS80000011.
  - The Library image (Debian trixie) has none of these; the GoW heroic session image has them.
- On a GPU-less Library pod the login page (CEF) renders black until `Client.HardwareAcceleration`
  is `"false"` in `%APPDATA%\Battle.net\Battle.net.config`.
- Keyboard input from Selkies never reached the login window under labwc (XTEST did), even in
  a Wine virtual desktop. `UseTakeFocus=N` (HKCU `Software\Wine\X11 Driver`) fixed it.
- Proton's Xalia (`xalia.exe`) took ~27% CPU walking CEF's accessibility tree and made pages
  crawl on the CPU-rendered Library; `PROTON_USE_XALIA=0` turns it off.
- Screenshots: `xwd -root` fails under rootless Xwayland (BadMatch); `xwd -id <window>` works.

## Sign-in

- The login sticks only with "Keep me logged in". Without it the client deletes its token
  ("DeleteToken() because !enabled") and asks again on the next start.
- With it, the token is a registry value under `HKCU\Software\Blizzard Entertainment\Battle.net\
  UnifiedAuth` (hash-named, in `pfx/user.reg`); a restart then logs in with no page.
  - Also under that key: `EncryptionKey\CacheDatabase` (Wine-DPAPI-wrapped, i.e. readable, key of
    `%LOCALAPPDATA%\Battle.net\CachedData.db`), `Identity`, `Launch Options\URI_TOKEN`.
  - All of it is in the shared home, like Steam's and Heroic's tokens. Never print or copy
    `user.reg` values or `CachedData.db`.

## Installing and the installed-games record

- Battle.net's default install dir is the prefix's `C:\Program Files (x86)`, in the home.
  - `steam-backup-saves` (hourly) copies the home except `steamapps/common`, so a game there
    goes to NFS every hour. Install to `Z:\games\battlenet\` instead (a moved folder is found again
    via "Locate the game").
- `ProgramData\Battle.net\Agent\product.db` (protobuf) lists each product: uid (`agent`,
  `battle.net`, `wow`), install path (`Z:/games/battlenet/World of Warcraft`), region, locale,
  version (`12.1.5.70575`), plus a `_retail_` subdir for WoW.
- Each game folder also has `.build.info` (pipe-separated table: Branch, Active, Build Key, CDN
  Key, …, Version, Product) and its own `.product.db`.
- A WoW download counts as Library activity under the idle rule: it is received bytes on eth0
  (~100 MB/s here), and `direwolf/library-last-activity` kept moving through it.

## Launching a game

- `Battle.net.exe --exec="launch WoW"` on a cold start only logs the argument; it launches
  nothing. A second `--exec="launch WoW"` sent to the running client launches the game.
- The game runs as `Z:\games\battlenet\World of Warcraft\_retail_\Wow.exe -launcherlogin -uid wow`,
  started by the client (not a child of either `proton run`): wait on that process.
- After the game exits, Battle.net and its Agent keep running, and so do both `proton run`
  wrappers. `wineserver -k` on the prefix ends them all at once.
- WoW retail runs DX12 through vkd3d-proton on the GPU (RTX 3090, NVIDIA 595.91.07):
  `Logs/gx.log` shows "D3d12 Device Create Successful"; the prefix's d3d12/d3d12core/dxgi load.
- Patching at launch is unverified: WoW was current. Battle.net does the update itself.

## Controller

- A Wolf "X-Box One (virtual) pad" reaches WoW: Wine's `winedevice.exe` opens its evdev node.
- WoW ignores it until `/console GamePadEnable 1`; then movement works. The CVar persists in
  `_retail_/WTF/Config.wtf` (`SET GamePadEnable "1"`) on the games volume.

## Loopback listeners (feeds XERK-2362)

- The client's `[WebServer]` on `127.0.0.1:22885`, the same on every run.
- The Agent on `127.0.0.1:<ProgramData\Battle.net\Agent\Agent.dat>` (37155 here, not 1120);
  HTTP 401 without its auth token.
- No CEF remote-debugging port, and no new listener after sign-in or while WoW runs.
- They show as `wineserver` in `ss -p`, and only to uid 1000.
- Neither the Library pod nor a session's app pod (`--app-pod-host-dir`) is host-networked, so
  there they stay in the pod's own loopback, and two pods never collide on 22885 (XERK-2362).
- A Battle.net App must set `spec.requireAppPod: true`: the operator then refuses its Sessions
  (InvalidSpec) without `--app-pod-host-dir`, rather than run it in the hostNetwork session pod,
  where these ports would be the node's loopback.
  - Catalogue copies follow the base's value on every scan; set on one copy it is reset.
  - It applies when a pod is built: setting it never moves a running Session's pod, and an App
    pod not yet created when the operator loses the flag is refused, not built host-networked.
  Tests: `TestRequireAppPodKeepsAppOffHostNetwork`, `TestLibraryEnsurePodStartsLibrary`,
  `TestCatalogueRefreshFollowsBaseApp`.
