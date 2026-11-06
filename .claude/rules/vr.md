---
paths:
  - images/vr/**
  - images/gow-nvidia32/vr/**
  - pkg/controllers/*vr*.go
  - pkg/controllers/ports.go
  - pkg/controllers/ports_test.go
  - examples/vr*.yaml
---

# VR streaming (epic XERK-2656; spike report on XERK-2662)

## Stack

- WiVRn (OpenXR runtime + server) with xrizer or OpenComposite as the OpenVR layer.
  Both layers ran Assetto Corsa; OpenComposite "looks better", xrizer is maintained.
- Pin the server to the Quest client's release (Horizon store): a protocol mismatch is refused
  (`wivrn_connection.cpp`), so bump server and store client together.
- ALVR + SteamVR is rejected, not a fallback to reach for: its TCP control port 9943 is a
  hard-coded const (no port block), its dashboard on 127.0.0.1:8082 lets any node process set a
  script ALVR runs, and its stream is plaintext.

## Pod shape

- WiVRn runs in the split App pod (`requireAppPod`, `.claude/rules/app-pod.md`), off the host
  network, with its port as a hostPort.
  - Steam's bwrap needs `hostUsers: false`, which the apiserver refuses with hostNetwork.
  - CUDA opens an abstract unix socket (`@cuda-uvmfd-…`); abstract sockets live in the netns, so
    under hostNetwork any node process could reach it (XERK-2692).
- It needs no capability, device, hostPath, hostPID or privilege. A change that seems to need one
  is a finding to raise, not to add.
  - Ship no `setcap` (CAP_SYS_NICE): with file caps gdbus ignores `DBUS_SESSION_BUS_ADDRESS`.
  - `/dev/uinput` is only for `hid-forwarding`; keep it false.
- Mount the NVIDIA ICD manifests (ArgoCD `nvidia-icd` ConfigMap, as the other Apps do): the DRA
  CDI spec injects libraries only, so without them WiVRn composites on lavapipe and NVENC fails
  `cuImportExternalMemory` with CUDA error 999 ("No suitable hardware accelerated codec").
- Memory: 16Gi was OOM-killed at Steam + AC start; 24Gi held (steady 2-7 GiB).
- One VR session per GPU: two 1280x1344 eyes at 90 Hz used ~50% of the RTX 3090's NVENC.

## Image (`images/gow-nvidia32` target `vr`, files in `vr/`; XERK-2663)

- The steam image plus WiVRn (built from source at a verified commit), xrizer (zip, sha256) and
  OpenComposite (commit); `DIREWOLF_VR_OPENVR` = `xrizer` (default) or `opencomposite`.
- `/opt/gow/startup-app.sh` links to `vr/startup-app.sh`; GoW's Steam startup moves beside it as
  `steam-startup-app.sh`. Siblings are found by the script's own path (tests copy them).
- It refuses to start (the session ends) without `DIREWOLF_VR_PORT` (XERK-2664 sets it on App
  containers when `spec.vrPort`), or unless `/srv/direwolf/state/{wivrn,openxr,openvr}` are each
  a mount (the App's `spec.stateMounts`). It never falls back to the shared home or port 9757.
- startup-app.sh rewrites WiVRn's `config.json` whole at every start, keeping no user key: WiVRn
  parses keys in one try block, and one of the wrong type would leave the port on 9757.
- PipeWire, WirePlumber and pipewire-pulse run with `XDG_RUNTIME_DIR=$XDG_RUNTIME_DIR/vr-audio`:
  Wolf's pulseaudio shares the runtime dir. The game gets `PULSE_SERVER` there, so its audio
  goes to the headset, not the Moonlight stream. wivrn-server keeps the real runtime dir: games
  reach its IPC socket there.
- The session bus listens on `unix:dir=/tmp` (`dbus-session.conf`), never an abstract socket.
- Either Steam or the VR services exiting ends the session. Tests: `images/gow-nvidia32/vr/vr_test.go`.
- wivrn-server's stderr runs through `vr-notice`, which shows "connect ... to <host> port <port>"
  and the pairing PIN in a swaynag overlay bar on the session's sway: the Moonlight mirror is the
  authenticated place to see them (XERK-2665). `DIREWOLF_VR_HOST` is the App's `status.hostIP`.
  - Only an address and 6 digits reach `swaymsg exec` (a shell, single-quoted): no `'`, `;` or
    `,` in its text, as sway splits commands on the last two.
  - It starts before sway (launch-comp starts sway inside GoW's Steam startup), so it waits on
    `$XDG_RUNTIME_DIR/sway.socket`; a PIN not yet shown dies with its wivrn-server.
- GoW's Steam startup sets `WINEDLLOVERRIDES=dxgi=n` outright; the image patches it to append
  `DIREWOLF_WINEDLLOVERRIDES` (grep-guarded, so a GoW bump moving the line fails the build).

## Ports and auth

- Exactly one port, TCP (control) and UDP (stream) on the same number: config `"port"`.
- `"publish-service": "none"`: Avahi needs the node's system bus; the headset dials IP:port.
- The TCP listener binds every address and closes while a headset is connected: one headset per
  server.
- Pairing: 6-digit PIN (SMP) while the known-keys list is empty, then only stored keys connect.
  The PIN goes to stderr and D-Bus only, so the user needs an authenticated place to see it.
- The config dir holds the keys: writable and per-user persistent, never a read-only ConfigMap
  ("Failed to save keys") and never the shared Steam home.
- The stream is AES-128-CTR with no MAC (XERK-2691). Never set `--no-encrypt`.

## Operator port (XERK-2664)

- `App.spec.vrPort` (CEL: needs `requireAppPod`) gets one port per Session from `--vr-port-range`
  (`vrPorts`, `vr_port.go`), recorded as `status.ports.vr` and replayed via `ClaimBase` on start.
  - Never from the session block: growing `sessionPortBlockSize` moves every block base, and
    running Sessions' recorded `status.ports` would no longer `Claim`.
  - `Claim` ignores `ports.VR`; keep it so, or every VR Session's block fails to re-claim.
- Every App container gets `DIREWOLF_VR_PORT` (first in env); the App's first container declares
  it TCP and UDP with `hostPort` = `containerPort`. Never the Wolf pod.
- The App pod's `vr-port` annotation records it; one differing from `status.ports.vr` ends the
  Session (`vrPortMoved`). No annotation means it serves none, and is left alone.
- No port is allocated once `PodCreated` is True: an App turning vrPort on would hold ports for
  pods that never serve them. A pod then built without one is refused (InvalidSpec).
- Without `--vr-port-range` a vrPort App's Sessions end InvalidSpec; the range must not overlap
  `--session-port-range` (operator exits). Tests: `vr_port_test.go`.

## Runtime pitfalls

- wivrn-server needs a D-Bus session bus (`dbus-run-session`); no system bus or systemd needed.
- After a failed headset session the server stayed up but never listened again: supervise it on
  "port listening", not on "process alive".
- Audio is PipeWire only. 26.9 segfaulted on every connect with no PipeWire config in the image
  (`can't load config client.conf`); building `-DWIVRN_USE_PIPEWIRE=OFF` gives a silent session.
- Pressure-vessel hides `/usr/local` and `/opt`: `XR_RUNTIME_JSON` there is not seen. Use
  `~/.config/openxr/1/active_runtime.json`, `PRESSURE_VESSEL_IMPORT_OPENXR_1_RUNTIMES=1` and
  `PRESSURE_VESSEL_FILESYSTEMS_RO=<OpenVR layer dir>`.
- WiVRn rewrites `~/.config/openvr/openvrpaths.vrpath` while it runs: in a shared home every
  Proton game then finds a VR runtime. Keep VR config on a per-user volume.
- Seated height: WiVRn uses Monado's legacy space overseer, so OpenXR LOCAL (OpenVR Seated) is a
  fixed 1.6 m up and the Quest recenter keeps y. The seated view sits ~0.4 m low until the game
  calls `ResetSeatedZeroPose` (XERK-2689); the App needs a recenter reachable in the headset.
- Headless sway on NVIDIA needs `--unsupported-gpu` and has no keyboard: xdotool keys are dropped
  and wtype's keymap is read wrongly by DirectInput. Inject keys through a real virtual device.

## Assetto Corsa (244210)

- Content Manager needs .NET 4.x, which does not install under Proton 9 or Experimental 11
  (XERK-2690). Launch `acs.exe` with `cfg/video.ini` `[CAMERA] MODE=OPENVR`, `FULLSCREEN=0`.
- CSP 0.2.11 crashes in VR; 0.3.x works once the prefix has winetricks `corefonts`, a
  `segoeui*.ttf` substitute (Segoe isn't redistributable) and `d3dcompiler_47` (Wine's compiler
  fails CSP's weather shaders: black scene), with `WINEDLLOVERRIDES=dwrite=n,b`.
- Vanilla AC takes no Quest controller input; CSP 0.3.x gives controller and hand control.
- The App is `examples/vr.yaml` (XERK-2665; deployed by digest from ArgoCD, XERK-2666).
  - Per-User state is `spec.stateMounts` on its `volumeClaimTemplate` PVC, at
    `/srv/direwolf/state/<name>` (XERK-2713): the three VR config dirs, and AC's
    `Documents/Assetto Corsa` (VR `video.ini`, CSP config).
  - Never mount them in a volume a game writes (the shared home, /games): runc follows a link at a
    mount point, and QA had `~/.config/openvr -> /etc/cont-init.d` run a game's script as root.
    The shared paths are links to the fixed root instead, which each container resolves to its
    own mount. A game replacing one redirects only file access as uid 1000, which the shared
    home already allows.
  - `startup-app.sh` links `~/.config/{wivrn,openxr,openvr}` there; in a flat session they
    dangle, so no Proton game finds a VR runtime. Pressure-vessel shares `/srv` like the home
    (steam-runtime-tools `wrap-home.c`), so games resolve the links.
  - The command makes the prefix's `Documents/Assetto Corsa` a link to `/srv/direwolf/ac-documents`
    and moves the flat copy to `Assetto Corsa.flat`. The vr image links that path to the
    `ac-documents` stateMount, the steam image to `.flat`, so flat sessions keep the flat config.
    Flat AC outside the steam image (the Library) finds nothing.
  - No image may ship anything under `/srv/direwolf/state`: runc follows an image link at a mount
    point too, then a game's link in /games (QA reproduced it with runc; both stages test it).
  - The command still checks, before the entrypoint, that each is a mount exactly where named
    (`realpath -e`, `mountpoint`), and chowns with `-h`.
  - Its command seeds that cfg from `Assetto Corsa.flat` (a second, read-only `steam-home` mount)
    once, skipping links, then forces `[VIDEO] FULLSCREEN=0`, `[CAMERA] MODE=OPENVR`.
  - Assumes AC in Steam's default library in the shared home
    (`~/.steam/debian-installation/steamapps`), launched flat once, or the command ends the
    session. The steam image's flat link and the example name that path: change them together.
  - The shared install keeps AssettoCorsa.exe = acs.exe (CM and the Kunos launcher can't run),
    and the Proton 9 pin; `steam -applaunch 244210` then starts acs.exe in either mode.
  - Recenter: Ctrl+Space from the Moonlight keyboard, on track. A pad/wheel binding is untested.
  - Its memory limit (24Gi) needs a User ceiling to match. Tests: `state_mounts_test.go`.
