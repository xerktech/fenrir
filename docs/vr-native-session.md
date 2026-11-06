# Native VR sessions (XERK-2667)

Design note for Phase 3 of the Fenrir VR epic (XERK-2656): a VR session the user starts and plays
without Moonlight. Phase 2 (XERK-2665) already streams VR from an App in the split App pod, but it
is launched from Moonlight, and Moonlight's flat mirror shows the pairing PIN and carries the
recenter key. Phase 3 removes Moonlight from that path.

Status: the API (XERK-2667) and the operator's pod and liveness (XERK-2668) are in place. Nothing
creates VR Sessions yet: the library page is XERK-2670.

## API

- `Session.spec.mode`: `Moonlight` (default) or `VR`. It can't change after creation, because the
  pod is built for one mode or the other.
  - Moonlight mode keeps `config`, `pairingReference` and `gateway` required, as before.
  - VR mode may set none of them: there's no Moonlight client, Pairing or Wolf. The CRD's CEL
    rejects any mix of the two (`TestSessionCRDValidatesMode`).
  - The App must set `spec.vrPort` (which implies `requireAppPod`). CEL can't see the App from a
    Session, so the operator checks this and ends the Session as InvalidSpec otherwise.
- Why a Session field and not an App field: the same App (Assetto Corsa VR) can be started from
  Moonlight, with a mirror, or natively. Whoever starts the session picks the mode.
- `status.ports.vr`: the only port a VR Session gets, from `--vr-port-range`. With no Wolf there's
  no port block, so all the other `status.ports` fields stay zero.
- Liveness uses status conditions, which need no new schema:
  - `HeadsetConnected`: True while a headset is connected to the VR server.
  - `StreamStarted`: True while the server is streaming to the headset.
- `status.disconnectedAt`: same meaning as in Moonlight mode (see Lifecycle).

## Lifecycle

### Launch trigger

The library page (XERK-2670) creates the Session.

- It sits behind Authentik and maps the username header to a User (`LookupUser`), as the Library
  already does. It's the authenticated place that replaces Moonlight's pairing.
- The Session it creates has mode `VR`, the User, and an App with `vrPort`. Moonlight's `/launch`
  never creates VR Sessions.
- The page shows the address to dial (node IP and `status.ports.vr`) and, while pairing, WiVRn's
  PIN (see Pairing).
- It counts against the Library's Steam lock like any Session: VR Apps run in the shared Steam home.
- Plan for one VR Session per GPU: one used about 50% of the RTX 3090's NVENC (XERK-2662).

### Pod (XERK-2668)

One pod, shaped like today's App pod:

- `hostUsers: false`, its own network namespace, and the VR port as a TCP + UDP hostPort.
- No Wolf, wolf-agent, pulseaudio or input-bridge.
- It has to supply what Wolf supplies today:
  - Display: the image starts its own headless sway (`WLR_BACKENDS=headless`, `--unsupported-gpu`)
    instead of waiting for Wolf's `WAYLAND_DISPLAY`. The operator tells the image the mode with an
    env var (for example `DIREWOLF_VR_MODE=native`). It must not change Moonlight-mode behaviour.
  - `XDG_RUNTIME_DIR`: an emptyDir in the pod itself. The split pod mounts the Wolf pod's runtime
    dir through a hostPath, and that only works while a Wolf pod exists.
  - Audio: already PipeWire to the headset in the vr image. Nothing new is needed.
  - Input: Quest controllers reach the game through OpenXR. There are no Moonlight pads or keyboard,
    and an App can't use `/dev/uinput`. So AC's Ctrl+Space recenter needs another route: XERK-2689's
    `ResetSeatedZeroPose` fix, or a CSP controller binding. A wheel on the headset isn't supported.

### Liveness signal

WiVRn's D-Bus interface `io.github.wivrn.Server` has two read-only properties, and emits
`PropertiesChanged` when they change (checked in xerktech/WiVRn `dbus/io.github.wivrn.Server.xml`):

- `HeadsetConnected` drives the `HeadsetConnected` condition.
- `SessionRunning` drives `StreamStarted`.

Why not probe the VR port instead: WiVRn's TCP listener closes while a headset is connected, but its
backlog is 1. A probe is itself a connection attempt, so it could take the slot a headset needs.

How the operator reads the properties:

- A `vr-agent` sidecar (the wolf-agent image) reads them. In native mode the image's session bus
  listens on a file in an emptyDir only the App and vr-agent mount (`/run/direwolf-vr-bus/bus`).
  It serves HTTPS on the pod IP.
- The operator polls it every `streamPollInterval`, with a per-Session bearer token and a pinned
  serving cert, the same scheme as wolf-agent's (`agent_token.go`).
- The game never sees the token, but it shares the bus and could report false values. That only
  keeps its own Session alive or ends it, which the App can already do.
- An agent the operator can't reach counts as a disconnect: the grace period runs, and an answer
  showing the headset connected stops it. Otherwise a hung bus or WiVRn would keep the Session (and
  its GPU) forever. Container exits still end the Session, as they do today.

### Disconnect and grace period

This works as it does for Moonlight sessions.

- Pod ready, no headset yet: set `disconnectedAt` when the pod first goes Ready. The grace period
  then covers putting the headset on. The unstarted-session reaper (`expiredReason`, keyed on
  `wolfSessionID`) must skip VR Sessions, or it reaps them 3 minutes after launch.
- Headset connects: `HeadsetConnected` True, `disconnectedAt` cleared.
- Headset disconnects: `HeadsetConnected` False, `disconnectedAt` set to now.
- `disconnectedAt` older than `--disconnect-grace-period`: the Session is deleted, and its pod goes
  with it. A headset reconnecting before then clears `disconnectedAt`.
- Reconnecting needs no `/resume` step: WiVRn reopens its listener after a session ends. The vr
  image supervises it on "port listening", so a server that never listens again ends the Session.

### Pairing (XERK-2669)

- There's no mirror to show the PIN. While pairing is enabled, the library page reads WiVRn's `Pin`
  property through the same agent and shows it behind Authentik.
- Revoking a headset is a call to `RevokeKey` on that agent. There's no Pairing CR in VR mode.

## Moonlight interplay

- `/resume` skips VR Sessions (this ticket). Otherwise it would write a Moonlight client's keys into
  a Session whose CRD forbids `config`. Tests: `TestResumeSkipsVRSession`.
- `/cancel` (Moonlight's "quit") stops only Moonlight Sessions, and serverinfo leaves VR pods out,
  so Moonlight never shows a VR session it can't resume or quit. A Moonlight `/launch` still
  replaces the user's VR Session: both would run Steam in one home.
- `--max-concurrent-sessions` counts users, VR included. That's correct: they share the node.

## Rollback

- An operator older than XERK-2668 ends VR Sessions at once. A VR Session created during a rollback
  is lost, but nothing is left running.
- The CRD change only adds a field and relaxes `required`. Removing the field later is the case the
  ArgoCD managedFields warning in `CLAUDE.md` covers (XERK-2713), but Sessions aren't
  ArgoCD-applied.
