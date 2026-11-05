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
