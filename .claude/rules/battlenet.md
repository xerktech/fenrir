---
paths:
  - images/library/**
  - images/gow-nvidia32/**
  - pkg/controllers/library*.go
  - pkg/controllers/catalogue*.go
---

# Battle.net (XERK-2359 epic; findings from the XERK-2360 hand-run spike)

Hand-run in the Library on talos04, 2026-10-27. Spike still in progress: items marked open are
unanswered.

## Running it in the Library

- umu-run fails here: pressure-vessel's bwrap can't create a user namespace in the pod
  ("No permissions to create a new namespace"). Steam gets by via `~/.local/bin/proot-bwrap`.
- Heroic's `Proton-CachyOS-latest` `proton run` (no runtime) works: set `STEAM_COMPAT_DATA_PATH`
  (prefix used: `~/Games/Heroic/Prefixes/default/Battle.net`) and
  `STEAM_COMPAT_CLIENT_INSTALL_PATH=/config/.steam/steam`.
  - Without umu's `GAMEID`, protonfixes skips the game's fixes, so apply them by hand (below).
- `Battle.net.exe` and `Battle.net-Setup.exe` are 32-bit (PE32), and this Proton runs them on its
  i386 unix side, so they need `libfreetype6:i386` and `libfontconfig1:i386`. The Library image
  lacks both: every label is blank and Wine logs "cannot find the FreeType font library".
- The login page (CEF) renders black until `Client.HardwareAcceleration` is `"false"` in
  `%APPDATA%\Battle.net\Battle.net.config`. The Library has no GPU (no `/dev/dri`), so the
  Library pod can't answer how WoW renders on the GPU; a session must.
- Keyboard: Selkies keystrokes don't reach the login window, but XTEST (`xdotool type`) does,
  so the field takes input and labwc doesn't give the Wine window keyboard focus. A Wine
  virtual desktop didn't fix it; `UseTakeFocus=N` (HKCU `Software\Wine\X11 Driver`) is
  being tested.
- Screenshots: `xwd -root` fails under rootless Xwayland (BadMatch); `xwd -id <window>` works.

## Loopback listeners (node loopback under hostNetwork; XERK-2094/2128 class)

- They show as `wineserver` in `ss -p`, and only to uid 1000.
- `127.0.0.1:22885`: the client's `[WebServer]` (fixed across restarts, logged in
  `%LOCALAPPDATA%\Battle.net\Logs\battle.net-*.log`). Answers no HTTP on `/`.
- `127.0.0.1:<port in ProgramData\Battle.net\Agent\Agent.dat>` (37155 here, not 1120): the
  Agent; HTTP 401 without its auth token.
- No CEF remote-debugging port was open (no `--remote-debugging-port` on its processes).
- Open: ports after sign-in and while a game runs.

## Installed products

- `ProgramData\Battle.net\Agent\product.db` exists after a bare client install; its contents
  with WoW installed are open.

## Open

- WoW rendering (DXVK vs vkd3d-proton) in a session, `--exec="launch WoW"`, which process to wait
  on, patching at launch, idle-rule download counting, controller through XInput, where the
  login token lands.
