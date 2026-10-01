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
- Only a plain GET starts the Library: a tab left open after an idle shutdown keeps
  reconnecting its WebSocket and must not restart it.
- Selkies has no login. The page is trusted only from `--library-trusted-proxies` (the Ingress
  behind Authentik) and the chart's NetworkPolicy lets only the operator reach the pod.
- Tests: `TestLibraryIdleRule`, `TestLibraryEnsurePod*`, `TestLibraryServer` in
  `pkg/controllers/library_test.go`; `TestLaunchBacksOutWhenBusyAfterCreate` in
  `pkg/moonlight/library_test.go`.
