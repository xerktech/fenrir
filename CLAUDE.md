# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

- Kubernetes orchestration for multiple [Wolf](https://github.com/games-on-whales/wolf) game-streaming
  instances ("direwolf"). README says it is not yet usable; expect POC-grade code.
- Go module path is `games-on-whales.github.io/direwolf` (not the GitHub repo name `fenrir`).
- CRD API group: `direwolf.games-on-whales.github.io/v1alpha1` — kinds `App`, `User`, `Session`, `Pairing`.

## Commands

- Test (matches CI): `make test` → `go test -race -shuffle=on -timeout 5m ./...`
- Single test: `go test -race -run TestName ./pkg/controllers/`
- Lint / format: `make lint`, `make fmt` (golangci-lint v2, config in `.golangci.yml`); `make vet`.
- Build one binary: `go build ./cmd/<operator|moonlight-proxy|wolf-agent>`
- Image: `docker build --build-arg APP_NAME=<cmd name> .` — one Dockerfile, `APP_NAME` picks the cmd.
  - The entrypoint is fixed at build time, never read from env: Users set wolf-agent's env.
  - Exception: `nri-nvenc-fix` ships a C shim, so it has `cmd/nri-nvenc-fix/Dockerfile` (repo-root
    context); the workflow matrix passes it as `file:`.
- Non-Go images live in `images/<name>/` (own Dockerfile + context), e.g. `images/library`
  (linuxserver/steam + Heroic), `images/retroarch` (GoW RetroArch + pinned cores).
  CI builds them via matrix `include:` entries carrying `context:`.
  - `images/gow-nvidia32` (targets `steam`, `heroic`) adds NVIDIA's i386 GL/EGL/Vulkan libs to
    the GoW images: Talos' toolkit extension has no lib32, so a GPU claim injects 64-bit only and
    32-bit games render on lavapipe (XERK-1930). Bump `NVIDIA_VERSION` with the Talos driver; on
    a mismatch its cont-init hook deletes them (CPU fallback, not a crash).
  - It also ships NVIDIA's NGX Wine DLLs in `/usr/lib/nvidia/wine` (XERK-2073): Proton enables
    DLSS only if `nvngx.dll` sits in `<dir of libGLX_nvidia.so.0>/nvidia/wine`; CDI injects none.
  - Nexus games run from a temporary launch view, so `launch-nexus` sets a per-game
    `VKD3D_SHADER_CACHE_PATH` in `~/.cache/vkd3d-proton/<id>`; without it vkd3d-proton caches in
    that view and every launch recompiles shaders (XERK-2349). Tests: `launch_nexus_test.go`.
  - Its game-session stage (base of `nexus`, `battlenet`) sets
    `ENV __GL_SHADER_DISK_CACHE_SKIP_CLEANUP=1` session-wide: NVIDIA trims its cache past its cap
    in the session's first OpenGL process (sway), before the game starts, and Gears' alone reached
    about 1.2 GB. Setting it only for the game does not stop that.
  - Its steam and heroic stages set `ENV UMASK=022` (game-session inherits heroic's): GoW's bases
    set 000, so sessions left world-writable files in the shared home and /games (XERK-2351).
    Tests: `TestSessionImagesUseUmask022`, `TestImageScriptsLeaveUmaskAlone`.
  - Its game-session targets drop to the desktop user via `/opt/direwolf/run-as-user`, not gosu,
    keeping CAP_SYS_PTRACE ambient (XERK-2349): talos04 has `kernel.yama.ptrace_scope=2`, and Easy
    Anti-Cheat's Linux module ptrace-attaches in its own tree, so without it EAC exits 210
    `Unexpected error. (#1)`. Strace hides this: EAC sees the tracer and bails earlier.
  - Session Apps (`steam`, `heroic`, `nexus`, `battlenet` targets) are pinned by digest in
    xerktech/ArgoCD `streaming/direwolf/app-*.yaml` and bumped there by hand: the chart release
    moves only the operator, Library and moonlight-proxy. A merged change here reaches sessions only after that
    PR (`crane digest ghcr.io/xerktech/fenrir/<target>:main`; check its revision label).
  - Its steam target keeps GoW's NetworkManager off the node's links (XERK-1867): session pods
    are hostNetwork and Steam added NET_ADMIN, so stock NM ran DHCP on every new pod veth.
    It manages only loopback and bridges: with nothing managed it reports "disconnected".
  - Its `steam` cont-init hook deletes `.cef-enable-remote-debugging` from the shared Steam install
    (Heroic writes it): Steam would serve its CEF debugger on 127.0.0.1:8080, the node's loopback
    under hostNetwork, so any node process could drive it. Tests: `TestSteamInputOffHookRunsScript`.
  - Its steam target cuts GoW's Decky Loader block from `system-services.sh` (XERK-2128): Decky
    runs as root on 127.0.0.1:1337, the node's loopback, and hands its auth token to any caller.
    The cut block is pinned by sha256, so a GoW digest bump that changes it fails the build.
- The chart's `WOLF_IMAGE` is `ghcr.io/xerktech/wolf` (fork xerktech/wolf, branch `xerktech`;
  XERK-1944), pinned by digest in `charts/direwolf-operator/values.yaml`; CI does not rebuild it.
  - Bump it by hand to a digest the fork's Docker build printed for the `xerktech` branch.
  - CI `wolf-image.yml` runs `wolfCommand` in the pinned image: the unit tests fake `wolfBinary`.
  - Fix Wolf behaviour in the fork (its `.claude/rules/xerktech-fork.md`), not with an overlay
    image here. The fork carries the XERK-1740 waylanddisplaysrc CUDA-context fix.
- Codegen after editing `pkg/api/v1alpha1/*.go`: `hack/update-codegen.sh` (bash + python3).
  - Regenerates `zz_generated.*`, `pkg/generated/` (clientset/listers/informers/applyconfig),
    `crds/`, and `schemas/`. All are committed — never hand-edit them.
  - CI (`codegen.yml`) reruns it and fails on any diff or untracked file; commit the regen.
  - Keep `crd:generateEmbeddedObjectMeta=true`: without it an embedded template's `metadata` is a
    bare `{type: object}` and the API server prunes its labels/annotations on admission (XERK-1347).
    Unit tests seed fake clients that never prune; `TestCRDsKeepEmbeddedMetadata` guards it.
  - App template metadata keys are checked by CEL markers in `app.go`; label values by a pattern
    `update-codegen.sh` patches into every CRD `labels` map (XERK-1375). CEL over a map's values
    exceeds the CRD cost budget. `TestAppCRDValidatesEmbeddedMetadata` runs the API server's checks.
  - CEL rules must compile on 1.28, the chart floor: no `quantity()` (1.29+, XERK-1521). The
    vendored libraries back-date it to 1.28, so only an envtest 1.28 kube-apiserver catches it.
    `hack/check-crds-on-floor.sh` (CI `crd-floor.yml`) creates every CRD on one; bump its
    version with the chart's `kubeVersion` (XERK-1678).
- Chart: `charts/direwolf-operator` has no `crds/` dir in git; CI copies `crds/*.yaml` in before
  `helm lint --strict` / `helm template --kube-version 1.36.0` (kubeVersion floor is >=1.28).

## Architecture

Three binaries in `cmd/`, all sharing `pkg/`:

- **moonlight-proxy** (`pkg/moonlight`): the Moonlight HTTP/HTTPS server users connect to.
  - Pairing creates a `Pairing` CR mapping client-cert fingerprint → `User`; HTTPS requests are
    authorized by fingerprint lookup.
  - The PIN is entered on the pairing page (`pkg/moonlight/pinpage.go`, own `--pin-port`) behind
    Authentik; the username header is trusted only from `--pin-trusted-proxies` peers that also
    send the `--pin-proxy-secret-file` secret.
    - Never serve it on the Moonlight ports: those are on the LoadBalancer, where SNAT can make
      an internet client look like a trusted in-cluster peer.
  - App list is rendered from `App` CRs.
  - `/launch` creates a `Session` CR, then blocks until the operator writes an RTSP URL into
    `Session.status` (bounded by `--launch-timeout` and the client connection).
  - `/resume` must not replace the Session (that deletes the pod the game runs in): it writes the
    client's new keys into its `spec.config` and waits for `status.attachedGeneration` to catch up.
  - `/cancel` deletes the user's Sessions at once, bypassing the disconnect grace period.
- **operator** (`pkg/controllers/session.go`): leader-elected (lease); only `SessionController` runs.
  - Per `Session` it creates one bare pod (game container + wolf + wolf-agent + pulseaudio
    sidecars, sharing `XDG_RUNTIME_DIR`) and PVCs. No Service: pods use `hostNetwork` on the node
    Moonlight clients stream from (`--session-node-selector`), since Moonlight only accepts a port
    redirect.
  - The pod is Job-like: named after, and controlled by, its Session; `restartPolicy: Never`.
    - Any exited container, or the pod gone (`PodCreated` condition True, no pod), ends the
      Session; deleting it GCs the pod and the pod's generated ResourceClaims. Never recreate it.
    - Disconnects are only seen by polling wolf-agent (`streamPollInterval`): wolf-agent stops
      Wolf's session on pause and has no Kubernetes access.
    - A disconnect sets `status.disconnectedAt`; the pod is kept for `--disconnect-grace-period`
      (default 10m) for `/resume`, then the Session is deleted. Tests: `session_lifecycle_test.go`.
    - The unstarted-session reaper must skip disconnected sessions (`expiredReason`).
    - An attach first stops every stream Wolf lists (none carry the current keys, so all are
      superseded); one still listed after its stop means Wolf is wedged, and the Session ends
      rather than stacking streams (`stopStaleStreams`, XERK-1688). Wolf lists a stream's session
      ID as `client_id`. Tests: `TestUnstoppableStreamEndsSession`.
    - A stream Wolf lists under the Session's keys with no `status.wolfSessionID` is adopted by its
      `client_id`, not ended: a slow `sessions/add` times out while Wolf completes it (XERK-2116).
      The add gets its own longer timeout so a retry rarely adds a second stream.
      Two streams under its keys end it, ID recorded or not (one shared ID, XERK-2116).
      Tests: `TestUnrecordedWolfStream*`, `TestDuplicateWolfStreamsEndSession`.
    - Never put an attach's wolf-agent calls under one shared deadline: a slow node then never
      attaches and a wedged Wolf goes unnoticed (XERK-2119). Slow pods are absorbed by
      `sessionWorkers` instead.
    - A stream Wolf still lists whose video flows (wolf-agent reads the node's conntrack for its
      `--video-port`) rose during the attach but not for `streamStallTimeout` froze; the Session
      ends (XERK-1741). Judge per flow against the attach's first sample: a previous attach's
      flow lingers ~120s at its final count, so a sum or "count > 0" ends unstarted resumes.
      The baseline is sampled just before AddSession, so a stream's first packets count as a rise.
      Needs `net.netfilter.nf_conntrack_acct=1` on the session node (Talos `nvidia-gpu` role);
      without it, or on an old pod, the check is off. Tests: `stream_liveness_test.go`.
    - A pod create rejected as Invalid ends the Session: CRD CEL cannot check every pod field
      (keys nested in arrays exceed the cost budget, annotation size). `TestInvalidPodEndsSession`.
    - So does a buildPod error from the App/User spec (wrapped in `errInvalidSpec`); a failed
      informer lookup stays retryable. `TestInvalidSpecEndsSession` (XERK-1782).
    - So does a Session with no pod whose App is missing from the cache and a live GET
      (`appGoneReason`, XERK-2191); the GET covers an App watch lagging the Session's.
      One with a pod is kept: its game runs on, and `replace --force` deletes an App briefly.
      Tests: `TestMissingApp*`.
    - All three record a Warning Event on the App (the client only sees a failed launch); the
      operator Role needs `events` create/patch.
    - `validateBuiltPod` runs on the finished pod for collisions an App/User spec makes with the
      operator's additions (volume names, mount paths, undeclared claims, label values), so the
      reason is logged up front. `TestBuildPodRejectsCollisionsWithOperatorAdditions` (XERK-1675).
    - A Session whose `spec.pairingReference` Pairing is gone (client revoked) is ended; Pairing
      deletes end their Sessions directly. The cache miss is confirmed with a live GET first, as the
      Pairing watch can lag the Session's. Tests: `session_pairing_test.go`.
    - The game runs on the pod's Wolf lobby display, not a stream's: stopping a stream destroys
      its display, and every disconnect stops it (XERK-1363).
      - `ensureLobby` creates it before the first AddSession, so its socket is `wayland-1`, the
        `WAYLAND_DISPLAY` the game waits for. `stop_when_everyone_leaves` must stay false.
      - wolf-agent joins each stream (`lobbyJoiner`) only after both pipelines start (setup events,
        then RTP pings): Wolf switches only running pipelines to the lobby's producers.
      - Wolf runs with `WOLF_USE_ZERO_COPY=FALSE` so a stream's own producer has the lobby's caps
        (`video/x-raw`); mismatched caps break every second producer switch on VA.
      - A stream's Wolf session ID is its paired client's cert hash. The init container seeds
        Wolf's config (on the PVC) with one paired client every attach uses (`wolf_clients.go`).
        IDs repeat across resumes: tell streams apart by events, not ID.
      - One client is safe only on xerktech/wolf >= 1f15679 (XERK-1948, XERK-1964). Older Wolf
        mixes up a stop + add under one ID: a late runner-exit stop, shared interpipe producer
        names, a dead peer's pause. Rotating clients (XERK-1380) is the fallback for older images.
      - A dead peer timing out after a /resume reused its ID fires `EndSupersededStreamEvent`, not
        a pause, and ends only the old stream; wolf-agent ignores it.
      - Tests: `TestFirstAttachCreatesLobbyBeforeStream`, `agent_lobby_test.go`.
  - Every pod listener (Wolf HTTP/HTTPS too, wolf-agent) must come from the session's port block
    (`pkg/controllers/ports.go`, `--session-port-range`), or pods on the node collide.
    - Blocks are keyed by pod and freed when its Session is deleted; `status.ports` is the
      record, replayed via `Claim` on operator start. Tests: `ports_test.go`.
    - Ports stay declared as containerPorts: under hostNetwork they become hostPorts, so the
      scheduler holds a pod whose block a terminating predecessor still binds.
    - A pod records its block in the `port-block` annotation; one whose block differs from
      `status.ports` ends the Session, since the advertised ports would go unserved.
  - Session pods run Wolf with `WOLF_HTTP_SERVERS=FALSE` (`withWolfHTTPServersOff`): no Moonlight
    HTTP/HTTPS, no mDNS. On the host network they'd listen on the node IP, and Wolf's HTTPS is how
    an unpaired client gets in (XERK-1682). Nothing uses them; moonlight-proxy terminates Moonlight.
    - Needs xerktech/wolf >= 91fe38e (XERK-1946): older Wolf ignores the variable and listens on
      0.0.0.0, so `wolfCommand` refuses a `/wolf/wolf` without the string `WOLF_HTTP_SERVERS`.
      Tests: `TestWolfCommandRefusesWolfWithoutHTTPServersSwitch`.
    - Set last, replacing any from a User's wolf policy. Tests: `TestSessionPodTurnsOffWolfHTTPServers`,
      `TestWithWolfHTTPServersOffOverridesPolicy`.
    - HTTP/HTTPS keep their slots in the port block so recorded blocks keep their layout.
  - Chart: moonlight-proxy is host-networked with a nodeSelector and tolerations that must match
    the operator's `--session-node-selector` / `--session-tolerations`.
    - Talos labels `kubernetes.io/hostname` with the FQDN (`talos04.xerktech.com`), and talos04
      is tainted `nvidia.com/gpu=present:NoSchedule`; miss either and every pod stays Pending.
  - Gateway API code in `session.go` is commented-out experimentation.
  - `LibraryController` (`pkg/controllers/library*.go`, off unless `--library-port`) starts the
    Steam + Heroic Library pod on a page visit, proxies it, and stops it when idle.
    It and game sessions never run together (shared Steam home); see `.claude/rules/library.md`.
  - The catalogue (`pkg/controllers/catalogue.go`, off unless `--catalogue-*-app`) scans the
    Library pod for Steam/Heroic installs and keeps one App per game; see `.claude/rules/catalogue.md`.
  - RomM sync (`pkg/controllers/romm.go`, off unless `--romm-url`) keeps one RetroArch App per
    playable RomM ROM, via the catalogue's App sync; see `.claude/rules/romm.md`.
  - The PVC is per user and App and outlives Sessions. Once it exists, `reconcilePVC` re-applies
    its live spec (only the storage request grows), never the template's: the apiserver rejects
    nearly every spec change, and a field left out of the apply is removed (XERK-1519).
    Template drift shows as `VolumeCreated` reason `TemplateDrift`. Tests: `session_pvc_test.go`.
    - Exception: `volumeAttributesClassName` follows the template once the claim is Bound (only
      then may it change); once set it can't be unset, so a dropped one stays (XERK-1554).
  - `App.spec.gpu` becomes a per-Session DRA ResourceClaim (`pkg/controllers/gpu_claim.go`).
    - `User.spec.resources` caps only requests/limits, never DRA claims: Apps are admin-authored
      and their author can already run any pod spec, so a per-User GPU policy adds no boundary
      (XERK-1329). An App template's own resourceClaims may still ask for more than one device.
  - Watches session pods (label `direwolf/session=true` only) to re-reconcile their Session.
  - `--app-pod-host-dir` (off by default) runs the App in a second, user-namespaced pod beside the
    session pod, which keeps Wolf and the sidecars; see `.claude/rules/app-pod.md` (XERK-1920).
- **wolf-agent** (`pkg/controllers/agent.go`, `pkg/wolfapi`, `pkg/fakeudev`): sidecar talking to
  Wolf's HTTP API over a mounted unix socket.
  - Syncs desired sessions into Wolf and emulates udev (mknods `/dev/input/*` nodes and writes
    `/run/udev/data`, emptyDirs shared with the game container) so SDL/Steam see hotplugged
    controllers. Rationale and Talos test: `.claude/rules/input-hotplug.md`.
  - Its `/api/v1/` proxy drives Wolf (can run arbitrary containers) and, under hostNetwork, is
    reachable on the node IP. It requires a per-Session bearer token (Secret
    `<session>-wolf-agent-token`, `pkg/controllers/agent_token.go`); the operator dials the pod IP.
    Wolf's API stays on the socket.
  - The same Secret holds wolf-agent's per-session serving cert, which the operator pins
    (RootCAs + ServerName `wolf-agent`). Never dial it with InsecureSkipVerify: other
    host-networked processes can bind the agent port and would collect the token.
  - App template containers (init too) may not mount any operator volume by name (`operatorVolumes`,
    XERK-1366): they'd get the token and key, or Wolf's socket. Add each new pod volume there.
    Tests: `TestBuildPodRejectsAppMountOfOperatorVolume`, `TestOperatorVolumesListsEveryOperatorVolume`.
  - Nor set `hostPID`/`shareProcessNamespace` (`validateAppConfinement`, XERK-1781): a root App
    reads the sidecars' mounts through `/proc/<pid>/root`. Tests: `TestBuildPodRejectsAppHostDevices`.
  - Nor declare any hostPath volume (XERK-1921): `/proc`, `/var/lib/kubelet` and more expose the
    token; a denylist always misses one. Live Apps use none; a game library goes in a local PV.
  - Nor mount a User's hostPath volume by name (`validateNoUserHostPathMounts`, XERK-1950): User
    volumes join the pod after the App checks. Other User volumes stay mountable by the game.
    Tests: `TestBuildPodRejectsAppMountOfUserHostPath`.
  - Apps and examples add no `NET_ADMIN` (XERK-2100): under hostNetwork it reconfigures the
    node's links, routes and firewall. Steam and Heroic run without it; only the unused
    `gst-ptp-helper` (file caps incl. `cap_net_admin`) then fails to exec. Not enforced (XERK-1329).
  - wolf-agent re-reads its token and its cert on every request/handshake: the operator may
    regenerate the Secret under a running pod and uses the new token and cert at once.
  - `Agent.Run` resubscribes to Wolf's `/api/v1/events` itself; `SubscribeToEvents` is one
    connection whose channel closes when it ends. r3labs/sse never retries an EOF'd stream.
    - Wolf replays no events, so after each subscribe `catchUp` rebuilds what was missed from the
      `state` Wolf lists per stream (`setup`/`streaming`/`paused`; xerktech/wolf, XERK-1947).
      Needs a Wolf image with that field (the operator defaults to `ghcr.io/xerktech/wolf:xerktech`):
      upstream images list no state, so missed pauses are lost. Tests: `TestListSessionsDecodesState`.
    - Listed `paused` is stopped as a pause event would (XERK-1378). A joined stream no longer
      listed under its AES key was stopped. Session IDs repeat across resumes; keys don't.
      Tests: `TestAgentCatch*`.
    - With the lobby empty, the one live stream the joiner doesn't know is seeded as if its setup
      events were seen, and joins on its next pings (XERK-1797). Several live unknown streams
      are left alone. Tests: `TestAgentJoinsStreamStarted*`, `TestAgentCatchUpStopsPaused*`.
  - `fakeudev` is Linux-only for real work (`fakeudev_linux.go` vs `fakeudev_other.go` stub);
    tests touching it behave differently on Windows/macOS.
- **`pkg/generic`**: typed generic wrappers over client-go informers/listers plus a reusable
  `Controller[T]` reconcile loop. New controllers should build on it, not raw `cache.SharedIndexInformer`.
- Game containers must wait for the `WAYLAND_DISPLAY` socket before starting, or the pod never goes
  Ready and the stream is never started.

## CI (`.github/workflows`)

- Open PRs on the fork: `gh pr create -R xerktech/fenrir`. With the `upstream` remote, gh may
  otherwise target games-on-whales/fenrir.
- PRs: `go test -race`, govulncheck, golangci-lint, Docker build of all three images, chart
  lint/template/package, and codegen drift (path-filtered).
- Push to main / release (`builder.yml`): pushes images to `ghcr.io/<owner>/fenrir/*` (operator image
  is named `direwolf-operator`), then rewrites chart `values.yaml` image refs to digests and pushes
  the chart as OCI.
  - Pass `github.ref*` into scripts via `env:`, never `${{ }}` splices — tag names are untrusted.
  - Chart version = next `v*` tag (`hack/release/next-chart-version.sh`): PATCH + 1 per merge,
    minor/major by `workflow_dispatch`. The tag is the only record (nothing committed); it is
    pushed before the chart, so a failed push skips a version rather than republishing one.
  - ArgoCD auto-deploys every published chart (tracks `0.*`), so builder.yml is a deploy path:
    keep `permissions: {}` with per-job grants, the `tests` gate, `plan`'s main-tip check, and the
    script's refusal of a commit older than the latest tag (a re-run would roll production back).
    Never add a deploy credential here (public repo); the cluster pulls.
