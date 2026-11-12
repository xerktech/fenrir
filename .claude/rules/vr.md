---
paths:
  - images/vr/**
  - images/gow-nvidia32/vr/**
  - pkg/controllers/*vr*.go
  - pkg/controllers/headset*.go
  - pkg/api/v1alpha1/headset.go
  - pkg/api/v1alpha1/headset_keys.go
  - pkg/moonlight/vrdoor*.go
  - cmd/moonlight-proxy/flags*.go
  - cmd/wolf-agent/vr_agent*.go
  - pkg/controllers/ports.go
  - pkg/controllers/ports_test.go
  - examples/vr*.yaml
  - pkg/api/v1alpha1/session.go
  - docs/vr-native-session.md
---

# VR streaming (epic XERK-2656; spike report on XERK-2662)

## Stack

- WiVRn (OpenXR runtime + server) with xrizer or OpenComposite as the OpenVR layer.
  Both layers ran Assetto Corsa; OpenComposite "looks better", xrizer is maintained.
- AC runs on xrizer (XERK-2842): for a legacy-input game OpenComposite (cff07db) recreates the
  OpenXR session after the first frames (`MaybeRestartForInputs`). With a frame in flight WiVRn
  logged `Fence waiting failed`, a later `xrAcquireSwapchainImage` failed, and OC aborted into an
  invisible MessageBox: AC alive, render dead, black for good, Session never ended (2 of 3).
  xrizer restarts only before the real session's first frame. OC logs to
  `~/.local/state/OpenComposite/logs/opencomposite.log`.
- The server is built from the xerktech/WiVRn fork (`WIVRN_REPO`/`WIVRN_COMMIT`): AES-GCM stream
  (XERK-2691) and a long-term `server_key.pem` the headset pins (XERK-2699).
  - The protocol hash covers the AEAD revision, so the Horizon store client is refused: the Quest
    runs the fork's APK from the same commit. Bump server and APK together.
  - Only the headset packets (`common/wivrn_packets.h` + serialization) enter the hash: a server
    commit leaving them alone still pairs with the deployed APK (check `git diff` on them).
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
  - When `/run/direwolf-vr-bus` is mounted (native mode), on `unix:path=` there instead (`dbus-session-agent.conf`), for the `vr-agent` sidecar. Keep it
    equal to `VRBusAddress` (`TestWiVRnStatusOverDBus` reads the image's file).
- ArgoCD's `app-assetto-vr.yaml` pins this image and copies `examples/vr.yaml`'s command by hand:
  bump both after a VR change here, or production keeps the old behaviour (XERK-2671 found it
  on a pre-native, upstream-WiVRn image whose command waited for Wolf's display).
- Either Steam or the VR services exiting ends the session. Tests: `images/gow-nvidia32/vr/vr_test.go`.
- wivrn-server's stderr runs through `vr-notice`, which shows "connect ... to <host> port <port>"
  in a swaynag overlay bar on the session's sway (XERK-2665). `DIREWOLF_VR_HOST` is the App's
  `status.hostIP`. It still shows and redacts a PIN, as a guard: WiVRn opens none (XERK-2811).
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
- Pairing: WiVRn opens 6-digit PIN pairing (SMP) while the known-keys list is empty, else only
  stored keys connect. The list is never empty here (XERK-2811): the game owns the bus and can
  read the PIN (`Pin`) or reach WiVRn on loopback, so it could pair itself.
- Never let the PIN reach the container log: Loki serves every pod's log to any pod, and the VR
  port is reachable from other session pods, so another user's game could pair (XERK-2671).
  `vr-notice` redacts it from every line it passes on. Tests: `TestVRNoticeShowsAddressAndPairingPIN`.
- WiVRn reads an unparseable `known_keys.json` as no keys, which opens PIN pairing: only ever
  write it whole, from a list that parsed.
- The config dir holds the keys (and `server_key.pem`): writable and per-user persistent, never a
  read-only ConfigMap ("Failed to save keys") and never the shared Steam home.
- Never set `--no-encrypt`; the fork's headset refuses an unencrypted server.

## Headsets (XERK-2669)

- A `Headset` CR (user, public key) is the only record of who may connect, not the volume.
  - Its key is WiVRn's stored form, the PEM's bare base64 body: WiVRn strips the headset's PEM
    (`clean_key`) and compares with `==`, so a seeded PEM block never matches and, the list being
    non-empty, pairing is off too: the headset is locked out.
  - Every vrPort App container gets `DIREWOLF_VR_HEADSETS` (WiVRn's known_keys format, the
    User's Headsets sorted by name; an App's own value is replaced). `startup-app.sh` rewrites
    `known_keys.json` from it at every start, so a key left on the volume is dropped.
  - Unset or `[]`, the image refuses to start: an operator that cannot revoke would leave old
    keys in force, and an empty list opens PIN pairing (XERK-2811).
  - A User with no Headset gets no pod: `buildPod` ends the Session InvalidSpec, after a live List
    (the Headset watch can lag a pairing the front door just made). The VR page refuses the start
    with "Pair your headset first". Tests: `TestVRPodWithoutHeadsetsIsInvalidSpec`,
    `TestVRPodFindsHeadsetMissingFromCache`, `TestVRPageRefusesUserWithoutHeadset`.
  - A pod whose `vr-headsets` annotation is present and empty (seeded with none by an older
    operator: its WiVRn pairs by PIN) ends its Session on reconcile (`headsetRevokedReason`).
    Tests: `TestPodSeededWithNoHeadsetsEnds`.
  - Headsets are made only by the VR front door (XERK-2808). Never create one from what the App's
    bus reports (XERK-2742): every App process is uid 1000, WiVRn's uid and the bus owner, so a
    game can plant a key in `known_keys.json` or PIN-pair itself. Nothing on the bus proves a
    human paired a headset; vr-agent reports no keys or PIN.
  - The App pod's `vr-headsets` annotation lists the Headsets it was seeded with as `name/uid`.
    One gone, being deleted, or re-created under its name (UID differs; cache miss confirmed live)
    ends the Session on reconcile, and at once on any Headset event (`reconcileHeadset`, found by
    annotation: a deleted object no longer names its User).
  - The spec is immutable (CEL): a changed key would admit a headset without ending Sessions.
  - vr-agent has its own token Secret `<session>-vr-agent-token` in both modes, never
    wolf-agent's (that one drives Wolf). A native pod from before XERK-2747 mounts wolf-agent's
    and is polled with it.
  - Moonlight mode: a vrPort App's split App pod runs vr-agent too (`withVRAgent`), with the bus
    dir and that Secret, only for the server key (`recordAppPodServerKey`, polled from the
    Moonlight reconcile until recorded). No readiness probe there: the stream waits for the App
    pod to be Ready, and the App waits for the stream's display before it starts WiVRn.
  - Tests: `TestMoonlightVRRecordsServerKey`, `TestSplitVRAppPodRunsVRAgent`.
  - Tests: `headset_record_test.go`, `headset_test.go`,
    `TestHeadsetCRDValidatesKeyAndImmutability`,
    `TestVRStartSeedsKnownKeysFromHeadsets`, `TestVRStartRefusesBadHeadsetSeed`.

## VR page (XERK-2670; `library_vr.go`, `/.direwolf/vr`)

- Served only on the headsets host (its proxy secret), the only page there, never the Library's:
  desktop code serves same-origin pages there that could start sessions (XERK-2750). Needs
  `--headsets-proxy-secret-file` and `--vr-port-range`. POSTs need `userSubmittedForm`.
- The Library lock is checked before and after the Session create (the Library's `EnsurePod`
  does the mirror image); drop either check and two racing starts can both survive.
- It never deletes a Moonlight Session to start VR, and its stop never touches one.
- Start also waits out every pod labelled `direwolf/user=<user>` (terminating too): Sessions have
  no finalizer, so a stopped one is gone while its pod still runs Steam (QA, XERK-2670).
- Page views read the Session and session-pod informers and reuse a vr-agent read for 3s: tabs
  reload every few seconds, and live reads spent the operator's shared client-go rate limit
  (QPS 5) until the session controller's calls timed out (QA). Only POSTs read live.
  - The vr-agent read is single-flight per Session: concurrent views wait for one read (each is a
    Secret GET). Without it a scripted flood of one's own page slowed others' starts to ~10s.
- Headset state is read live from vr-agent (`ReadVRStatus`, which never ends a Session). The
  address is the pod's `status.hostIP` and `status.ports.vr`, shown only once the pod the Session
  controls is Ready. It shows no PIN (XERK-2811).
- Its session cap is the operator's `--max-concurrent-sessions`; keep it equal to moonlight-proxy's.
  The two processes don't share a lock, so a VR start and a Moonlight launch at the same instant
  can both pass the cap.
- Tests: `library_vr_test.go`.

## VR front door (XERK-2808; `pkg/moonlight/vrdoor.go`, protocol in `docs/vr-native-session.md`)

- moonlight-proxy, `--vr-port` on `--vr-address` only: never a wildcard, loopback or link-local
  address, and IPv4 as itself (`checkVRFrontDoorFlags`): the pairing page shows it to add. Codes are entered on the pairing page, so it needs `--pin-port`.
- A headset is SHA-256 of its TLS client key's SPKI, held in `Headset.spec.clientKeySHA256`; the
  Headset's User is the caller. Two Headsets on one key admit neither. Read from the informer.
- The pairing code needs the commitment step: a code over the certificates alone can be ground by
  a LAN relay. Keep the derivation equal to the Quest app's (XERK-2809);
  `TestVRPairingCodeBindsEverything` pins a vector.
- The code proves nothing to the headset: a fake front door computes it too (XERK-2813). The
  pairing page shows the front door's key fingerprint (`vrKeyFingerprint`), which the Quest app
  must show for the key it pins, and pin only once the user confirms a match. Keep the format
  equal to the app's. Tests: `TestVRPairingPageShowsServerKey`, `TestVRKeyFingerprint`.
- The pairing page's QR code (`vrPairingLink`, `vrQRCode`, XERK-2812) carries the full key hash; the
  app pins it before `/pair`. Keep the link format equal to the app's `parse_pairing_link`; its
  `qr_ut` decodes a PGM of `vrQRCode` (regenerate it there if the rendering changes). Inline SVG,
  as the page's CSP allows no images. Tests: `TestVRPairingLink`, `TestVRPairingPageShowsQR`.
- Never lift `/pair`'s server-wide rate limit (`vrFrontDoor.draws`): each answer is a server nonce a
  relay can test against the headset's code before confirming; unlimited, QA paired its own key
  in under a second. Tests: `TestVRFrontDoorLimitsDraws`.
- Launch, stop and the app list are `VRLaunch`'s, with moonlight-proxy's clients: one set of
  checks for both doors. moonlight-proxy's `/launch` and the front door share no lock, as the
  operator's VR page doesn't.
- A Moonlight Session of a VR App is the headset's to join (XERK-2811): the Quest app trusts no
  server it neither pinned nor PIN-paired (fork `client/wivrn_client.cpp`), and PIN is gone.
  - `SessionState` reports it `starting`, then `ready` with the split App pod's `hostIP`,
    `status.ports.vr` and key; a launch of that same App returns it, another App is refused.
  - The headset's stop never ends it: quit it in Moonlight. Tests:
    `TestVRFrontDoorJoinsMoonlightVRSession`, `TestVRFrontDoorLeavesFlatMoonlightSession`.
- `ready` needs `status.vrServerKey`, which the operator sets once from vr-agent's first report
  and never replaces (`TestVRSessionRecordsServerKeyOnce`). The vr image makes the key with openssl
  and writes its public half to `VRServerKeyFile`; vr-agent reads it non-blocking, regular files
  only (the App can plant a FIFO there).
- RBAC: moonlight-proxy gets Headset `create` only.
- Tests: `vrdoor_test.go`, `TestVRSessionStateForFrontDoor`, `TestReadServerKey`,
  `TestVRStartPublishesServerKey`.

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

## Native VR sessions (`spec.mode: VR`, XERK-2667)

- Design and lifecycle: `docs/vr-native-session.md`. Mode is a Session field, fixed at create:
  the same App can be launched from Moonlight (mirror) or natively.
- A VR Session has no `config`, `pairingReference` or `gateway`; Moonlight mode still needs all
  three (CEL). Its only port is `status.ports.vr`. Tests: `TestSessionCRDValidatesMode`.
- The pod (`buildVRPod`, XERK-2668) is `buildPod`'s minus Wolf's containers and volumes, so every
  App check still runs; one pod, `hostUsers: false`, off the host network, never split, no port
  block (`checkPod` skips the block check). It needs no `--app-pod-host-dir`.
  - XDG_RUNTIME_DIR is the pod's own 64Mi memory emptyDir; `DIREWOLF_VR_MODE=native` makes the
    image run sway headless (`WLR_BACKENDS=headless`, `WAYLAND_DISPLAY` unset). The App's command
    must skip its wait for Wolf's display then (`examples/vr.yaml`).
  - No GAMESCOPE_* env: with no client mode GoW would set sway's output to 0x0.
- Liveness: the `vr-agent` sidecar (wolf-agent image) reads WiVRn's `HeadsetConnected` /
  `SessionRunning` over the App's bus and serves them behind the Session's token and pinned cert
  (its own Secret, `<session>-vr-agent-token`), polled every `streamPollInterval` on the pod IP,
  port `VRAgentPort`.
  - A sidecar, not an agent in the App: the token stays out of the game's reach, and the agent
    ships with the chart, not the hand-pinned App digest.
  - It runs as the App's PUID/PGID (1000 if unset or empty, as GoW reads them; the last of a
    repeated name): the bus's EXTERNAL auth admits only its owner. An id the spec can't tell
    (valueFrom, not a number, 0, two containers disagreeing) is InvalidSpec (`vrAgentIDs`,
    XERK-2709); a fixed 1000 left such pods never ready for 15 min.
    - Not read: a PUID from `envFrom` (ConfigMap/Secret) stays 1000 unseen. Set PUID as a value.
  - Ready only once WiVRn answers. First ready: `HeadsetConnected` False `WaitingForHeadset` and
    `disconnectedAt` set, so the grace period covers putting the headset on. A connect clears it;
    a disconnect sets it. A zero `--disconnect-grace-period` ends a VR Session once it is ready.
  - An unreachable agent counts as a disconnect (`AgentUnreachable`), and a started pod is polled
    even NotReady: else a hung bus or WiVRn (the game can move the bus) held the GPU forever (QA).
  - The Moonlight reaper skips VR Sessions; one whose pod is never ready goes after `vrUnstartedTTL`,
    with a `VRPodNeverReady` Warning Event on the App (the VR page cannot show a deleted Session).
  - Frame watchdog (XERK-2844): a game whose count of delivered frames (the fork's D-Bus
    `AppFramesDelivered`) stops changing for `vrFrameStallTimeout` while `HeadsetFocused` ends
    the Session. A crashed OpenVR layer otherwise hangs the game behind a dialog nobody sees
    (OpenComposite's MessageBox, xrizer's X11 panic window) or an infinite fence wait, and WiVRn
    keeps streaming black. Armed only once a frame was delivered (loading). Only time between two
    focused reads adds up, each gap capped at `vrFrameMaxInterval`: an unread stretch (failed poll,
    reconcile error) mostly never counts, and a flapping agent can't reset it.
    Any change is progress (the count restarts with WiVRn's server). Off when the image's WiVRn
    lacks the properties. Tests: `TestVRFrameStallEndsSession`.
  - Tests: `vr_session_test.go`, `cmd/wolf-agent/vr_agent_test.go`.
- moonlight-proxy's `/resume` never picks a VR Session. Tests: `TestResumeSkipsVRSession`.
- Moonlight never sees one: `/cancel` skips VR Sessions and serverinfo and the cancel wait skip
  pods labelled `direwolf/session-mode=VR`. A Moonlight `/launch` still replaces it (one Steam home).
  Tests: `TestCancelLeavesVRSession`.

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
    (`~/.steam/debian-installation/steamapps`), or the command ends the session. The steam image's flat link and the example name that path: change them together
    (`TestVRExampleACPathsAgreeWithSteamImage`).
  - The shared install keeps AssettoCorsa.exe = acs.exe (CM and the Kunos launcher can't run),
    and the Proton 9 pin; `steam -applaunch 244210` then starts acs.exe in either mode.
- `vr/ac-setup` (XERK-2726) does the one-time setup before the entrypoint, as PUID, on every VR
  launch: prefix (Proton 9 + winetricks + Selawik as segoeui*.ttf), flat cfg `FULLSCREEN=1` and
  `INPUT_METHOD=X360` (once), acs.exe swap (every launch: Steam updates restore the launcher),
  CSP at `DIREWOLF_AC_CSP` (App env, not an operator flag).
  - Not a Library feature: it wouldn't repair a Steam update before the next Library visit, and
    the Library image has no Proton 9 for winetricks. Flat sessions benefit after one VR launch.
  - Markers: `compatdata/244210/direwolf-ac-setup.json` (prefix, flat cfg) and the game folder's
    (exe, CSP), so a CSP bump reruns only CSP. Bump `PREFIX_REV`/`FLAT_CFG_REV` to rerun a step.
  - CSP versions are pinned by sha256 in `ac-csp-versions.json`; a version not listed ends the
    session. Downloaded on first use (redistribution unclear), with curl: acstuff.club returns
    403 to Python's urllib.
  - Every file it reads or writes in the shared home is reached by fd, one folder at a time with
    `O_NOFOLLOW`; a link at a folder is refused, a link at a file is replaced by rename.
    Proton, Wine and winetricks do follow links in the prefix: as the game does, same uid and
    container, so nothing beyond what the game can reach. Their cache stays out of the home.
  - The prefix is made with `proton getcompatpath /`: every verb but `runinprefix` builds the
    prefix, and this one then only runs winepath (no steam.exe, which `run` starts).
  - It refuses root (Wine refuses a prefix it doesn't own; root files break the Library).
  - A prefix counts as made only with compatdata's `version` file, which Proton writes once the
    prefix is copied and registered (Steam's next launch finishes the rest).
  - The exe marker records acs.exe's hash even when AssettoCorsa.exe already matched (a hand
    swap): else an acs.exe update took the old copy for the launcher and dropped it (QA).
  - Two runs serialize on `compatdata/244210/direwolf-ac-setup.lock`, the second logging that it
    waits.
  - A game's plant must not end every User's VR (XERK-2819): a non-file at a marker, the lock or
    a temp name is replaced (`clear`: a folder is renamed aside, which its mode can't stop, then
    rmtree'd as far as it lets us); an unreadable marker (only) reads as absent; an unreadable lock is
    replaced (it is opened read-only, enough for flock); a flat cfg error logs and skips it,
    unrecorded, so the next launch retries. Tests: `TestACSetupRecoversFromPlantsOfEveryUsersPaths`.
    - Its mode plants skip as root (DAC_OVERRIDE): run the test binary as a user (`setpriv`).
  - The CSP marker lists the archive's files (`csp-files`); a bump removes those the new archive
    lacks, never other files in `extension/` (CM installs apps and weather there). Installs from
    before the list (XERK-2770) keep one bump's dropped files. The list is game-writable: each
    entry must pass `csp_member`.
  - Do the first launch from Moonlight: a native VR Session's pod is reaped after
    `vrUnstartedTTL` (15m) never ready, and the first setup's downloads count against it.
  - Tests: `images/gow-nvidia32/vr/ac_setup_test.go` (stub Proton, Wine, winetricks, curl),
    `TestVRExampleRunsACSetup`.
  - Recenter: Ctrl+Space from the Moonlight keyboard (fixed in CSP), and the pad's right-stick
    click: ac-setup adds `[__CM_RESET_CAMERA_VR] XBOXBUTTON=RTHUMB_PRESS` to the flat and the
    User's VR `controls.ini` each launch (XERK-2770). CSP 0.3.1 lists the hotkey in
    `data_hotkeys_supported.txt`; the value names are Content Manager's (`ControllerButtonEntry`).
    - A section already naming `XBOXBUTTON` (even empty) is the User's; a button another section
      holds is not bound. The VR cfg is edited only once seeded: making it would stop the seed.
    - Any recenter failure (a folder, socket, link or mode a game plants at either cfg, its
      controls.ini or the temp name) is logged, never ends the launch: it runs every launch, so
      one game could block every User's VR. A failed flat cfg step is logged and retried next
      launch. Tests: `TestACSetupBindsVRRecenterToPad`.
    - Unverified in a headset: that the press recenters.
  - Its memory limit (24Gi) needs a User ceiling to match. Tests: `state_mounts_test.go`.
