# Native VR sessions (XERK-2667)

Design note for Phase 3 of the Fenrir VR epic (XERK-2656): a VR session the user starts and plays
without Moonlight. Phase 2 (XERK-2665) already streams VR from an App in the split App pod, but it
is launched from Moonlight, and Moonlight's flat mirror shows the pairing PIN and carries the
recenter key. Phase 3 removes Moonlight from that path.

Status: the API (XERK-2667), the operator's pod and liveness (XERK-2668), headsets (XERK-2669),
the VR page that starts sessions (XERK-2670) and the VR front door (XERK-2808, below) are in
place. The front door's Quest side is XERK-2809.

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

The VR page (XERK-2670, `/.direwolf/vr`, `library_vr.go`) creates the Session.

- It is on the headsets page's host, not the Library's: the Library host proxies the desktop,
  whose code can serve same-origin pages there (XERK-2750) that would read the PIN or start and
  stop sessions. Its POSTs are the page's own clicked form, as the headsets page requires.
- It sits behind Authentik and maps the username header to a User (`LookupUser`), as the Library
  already does. It's the authenticated place that replaces Moonlight's pairing.
- The Session it creates has mode `VR`, the User, and an App with `vrPort`. Moonlight's `/launch`
  never creates VR Sessions.
- The page shows the address to dial (node IP and `status.ports.vr`) and, while pairing, WiVRn's
  PIN (see Pairing).
- It refuses while the User's Library pod exists (running or stopping), checking again after the
  create and deleting its Session if the Library started meanwhile, as moonlight-proxy's `/launch`
  does. The Library counts the VR Session like any other: VR Apps run in the shared Steam home.
- It never replaces a running session (a Moonlight `/launch` does): ending a Moonlight game the
  user didn't quit loses unsaved play. It refuses instead, and caps players at the operator's
  `--max-concurrent-sessions`, which must equal moonlight-proxy's.
- The PIN is read live from vr-agent per page view (`ReadVRStatus`), never stored in the Session:
  more can read a Session than the page. vr-agent reports it only as six digits.
- Stop deletes only the User's VR Sessions; Moonlight ones are Moonlight's to quit.
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
  serving cert, the same scheme as wolf-agent's (`agent_token.go`), in vr-agent's own Secret
  (`<session>-vr-agent-token`, XERK-2747): wolf-agent's token also drives Wolf.
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
- A User's paired headsets are `Headset` objects, in both modes. The operator seeds WiVRn's known
  keys from them at every start, so the keys file on the User's volume decides nothing.
- Revoking a headset is deleting its Headset. That ends the User's Sessions seeded with it, the way
  deleting a Pairing ends a Moonlight client's. `RevokeKey` isn't used: it only works while a
  session runs, and the next start would bring the key back.
- A new PIN pairing does not become a Headset by itself (XERK-2742, XERK-2747). The game runs as
  WiVRn's uid and owns its bus, so it can plant or pair a key of its own; nothing the agent reads
  proves a human paired. The operator keeps keys no Headset holds as pending in the Session's
  `status.pendingHeadsets` (never seeded; gone with the Session), and the page lists them
  (fingerprint, headset name, time seen) for the user to confirm, warning when more than one
  waits. The fork's Quest app shows the same fingerprint for its own key (XERK-2743), and the page
  tells the user to confirm only an exact match. Confirming creates the Headset; the pod then
  records it, so revoking it ends that Session too. An unconfirmed key works for that Session only.
  The page is `/.direwolf/headsets`
  (XERK-2740), in both modes, on a host of its own behind Authentik
  (`--headsets-proxy-secret-file`, XERK-2750): never the Library's host, whose desktop runs user
  code that can serve pages on that origin.

## VR front door (XERK-2808)

Moonlight-style VR: pair the headset once by typing the code it shows into the pairing page, then
list and launch VR Apps from the headset. It replaces the VR page plus in-session PIN for a headset
running a Quest app that speaks it (XERK-2809); the VR page and PIN path stay as the fallback.

### Where it runs

- moonlight-proxy, on `--vr-port`, TLS 1.3, bound to `--vr-address` only (the chart sets it to the
  node IP from `status.hostIP`): never a wildcard, which under hostNetwork includes the node's
  loopback. Off without `--vr-port`; needs `--pin-port`, where codes are entered.
- It serves moonlight-proxy's certificate, which the headset pins at pairing.
- A client is its TLS key: SHA-256 of the client certificate's SubjectPublicKeyInfo. The Headset
  holding it in `spec.clientKeySHA256` names the User. Deleting the Headset refuses the headset
  and, as for any Headset, ends the Sessions seeded with it. Two Headsets holding one key admit
  neither.
- moonlight-proxy gains Headset create (nothing else): it is internet-facing, and the spec is
  immutable, so it can't change one.

### Pairing

1. `POST /vr/v1/pair` `{publicKey, name, commitment}`: `publicKey` is WiVRn's key in Headset form
   (the PEM's base64 body), `commitment` is hex SHA-256 of a random 32-byte headset nonce. The
   reply is `{pairingID, serverNonce, pairingURL?}`. A key already paired gets 409.
2. `POST /vr/v1/pair/confirm` `{pairingID, nonce}`: the nonce must match the commitment (403
   otherwise, and the pairing is dropped). The request is held until the code is entered (200
   `{paired: true}`) or 3 minutes pass (408). A confirm dropped mid-wait may still have paired:
   try `GET /vr/v1/apps`.
3. Both sides compute the code: SHA-256 over `direwolf-vr-pair-v1`, a NUL byte, the server's TLS
   key hash, the client's TLS key hash, SHA-256 of the `publicKey` text, the server nonce and the
   headset nonce (raw bytes, in that order). The first 8 bytes, big-endian, mod 10^8, as 8 digits
   with leading zeros. `TestVRPairingCodeBindsEverything` holds a fixed vector.
4. The user enters it on the pairing page (Authentik, trusted proxy and secret, JSON POST from
   the page). A match creates the Headset, owned by the User.

Why a commitment: a code derived from the two certificates alone lets a LAN relay grind
certificate serials offline until its two connections' codes collide, in seconds for 8 digits.
With the commitment the relay must fix the server nonce it shows the headset before the headset
reveals its nonce, so it can't steer the headset's code.

Why a rate limit: the relay can still draw server-side codes, since each `/pair` hands it a new
server nonce and it knows every other input before it confirms (QA matched a chosen code in under a
second). So `/pair` answers at most one per 5 seconds server-wide, burst 3, else 429: in the
minutes the user takes to type the code a relay gets a few dozen draws at 1 in 10^8 each.

A wrong code counts against the entering User: 5 per 10 minutes, in memory (a restart resets it).
At most 16 pairings wait at once; both limits let anyone delay pairing, never complete one.

### Authenticated API (paired client certificate)

- `GET /vr/v1/apps`: `{apps: [{id, title, cover?}]}`, the Apps with `vrPort` the User may see
  (the VR page's list). `cover` is a path serving the box art as PNG.
- `POST /vr/v1/launch` `{app}`: the VR page's launch (`VRLaunch.Launch`): the same Library lock
  before and after the create, the user's pods waited out, `--max-concurrent-sessions`, never a
  Moonlight Session replaced. 202 with the session, or 409 `{error}` saying why not.
- `GET /vr/v1/session`: `{state, app, title, host, port, serverKey, headsetConnected, message}`.
  `state` is `none`, `starting`, `ready`, `stopping` or `other` (a Moonlight Session runs).
  `host`, `port` and `serverKey` are set only when `ready`.
- `POST /vr/v1/stop`: ends the User's VR Sessions (204).
- Errors are JSON `{error}` with a real HTTP status (this is not Moonlight's protocol).

### Server key

- `serverKey` is the session's WiVRn identity key (raw X448, hex): the headset pins it for the
  stream instead of trusting the server on first use.
- The vr image makes `server_key.pem` with openssl before WiVRn starts, if missing, and writes its
  public half to `/run/direwolf-vr-bus/server_key.pub`; vr-agent reports it, and the operator
  records the first report in `status.vrServerKey`. `ready` waits for it, so a pod on an image
  from before this never reads ready.
- The App's processes can write that file, as they can run their own WiVRn: the pin protects the
  headset from the network, not from the game.

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
