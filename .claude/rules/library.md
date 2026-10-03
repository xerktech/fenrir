---
paths:
  - pkg/controllers/library*.go
  - pkg/moonlight/library*.go
  - pkg/moonlight/rest.go
  - cmd/operator/**
---

# Library (on-demand Steam + Heroic desktop)

- Steam lock: the Library and game sessions share one Steam home, so they never run together.
  - Both sides check, create, then check again (`EnsurePod`; `createSession`'s post-create
    `BusyCheck`), so two racing starts can't both survive. Drop either recheck and they can.
  - Both checks read the API server (`List`), never an informer: the other side's object may
    be seconds old.
  - The operator side also counts session pods, not just Sessions: a terminating pod still
    runs Steam.
- Idle rule: no browser traffic for `--library-idle-timeout` AND nothing downloading.
  - Activity lives on the pod (`direwolf/library-last-activity`), not in memory: every
    operator replica serves the page, only the leader runs the idle check.
  - Any doubt (exec failure, unparseable Heroic store) keeps the Library up.
  - Shutdown deletes the pod even if `steam -shutdown` fails: a stuck pod would hold the lock.
- Only a user-activated page load (`Sec-Fetch-User: ?1`) or the stopped page's Start button
  (POST `libraryStartPath`) starts the Library.
  - Selkies calls `location.reload()` when its stream drops, so a tab left open across an idle
    shutdown would restart it at once. A plain GET or a WebSocket must never start it.
- The pod's nginx demands basic auth (`PASSWORD` from Secret `direwolf-library-auth`); only the
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
  `TestLibraryEnsurePod*`, `TestLibraryServer` in
  `pkg/controllers/library_test.go`; `TestLaunchBacksOutWhenBusyAfterCreate` in
  `pkg/moonlight/library_test.go`.
