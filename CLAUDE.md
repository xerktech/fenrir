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
  - Exception: `nri-nvenc-fix` and `wolf-agent` ship C shims, so each has `cmd/<name>/Dockerfile` (repo-root
    context); the workflow matrix passes it as `file:`.
- Non-Go images live in `images/<name>/` (own Dockerfile + context), e.g. `images/library`
  (linuxserver/steam + Heroic), `images/retroarch` (GoW RetroArch + pinned cores).
  CI builds them via matrix `include:` entries carrying `context:`.
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
    - A pod create rejected as Invalid ends the Session: CRD CEL cannot check every pod field
      (keys nested in arrays exceed the cost budget, annotation size). `TestInvalidPodEndsSession`.
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
      - A stream's Wolf session ID is its paired client's cert hash, and a dead client's ENet peer
        pauses that ID seconds after a /resume. So the init container seeds Wolf's config (on
        the PVC) with a few paired clients and consecutive attaches use different ones
        (`wolf_clients.go`, XERK-1380). IDs still repeat: tell streams apart by events, not ID.
      - Tests: `TestFirstAttachCreatesLobbyBeforeStream`, `agent_lobby_test.go`.
  - Every pod listener (Wolf HTTP/HTTPS too, wolf-agent) must come from the session's port block
    (`pkg/controllers/ports.go`, `--session-port-range`), or pods on the node collide.
    - Blocks are keyed by pod and freed when its Session is deleted; `status.ports` is the
      record, replayed via `Claim` on operator start. Tests: `ports_test.go`.
    - Ports stay declared as containerPorts: under hostNetwork they become hostPorts, so the
      scheduler holds a pod whose block a terminating predecessor still binds.
    - A pod records its block in the `port-block` annotation; one whose block differs from
      `status.ports` ends the Session, since the advertised ports would go unserved.
    - Exception: Wolf's mDNS (UDP 5353, SO_REUSEPORT) is hardcoded and outside the block.
  - Wolf's HTTP/HTTPS bind to 127.0.0.1 via a `bind()` preload shim (`cmd/wolf-agent/shim`,
    `wolfCommand`): Wolf hardcodes 0.0.0.0 and its HTTPS accepts any cert naming an unknown issuer
    as paired (XERK-1682). Flannel enforces no NetworkPolicy, and none applies to hostNetwork.
    - The shim ships in the wolf-agent image (own Dockerfile); an init container copies it in.
      Wolf refuses to start without it. Tests: `TestLoopbackShim`, `TestSessionPodPreloadsLoopbackShim`.
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
  - Watches session pods (label `direwolf/session=true` only) to re-reconcile their Session.
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
  - wolf-agent re-reads its token and its cert on every request/handshake: the operator may
    regenerate the Secret under a running pod and uses the new token and cert at once.
  - `Agent.Run` resubscribes to Wolf's `/api/v1/events` itself; `SubscribeToEvents` is one
    connection whose channel closes when it ends. r3labs/sse never retries an EOF'd stream.
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
