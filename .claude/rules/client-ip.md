---
paths:
  - pkg/moonlight/rest.go
  - pkg/moonlight/rest_test.go
  - pkg/controllers/session.go
  - pkg/controllers/wolfsession_test.go
  - pkg/controllers/session_ports_test.go
  - pkg/controllers/session_lifecycle_test.go
---

# Moonlight client IP → Wolf

- The client IP comes only from `r.RemoteAddr` via `remoteIP` (proxy is hostNetwork, no SNAT).
  - Never take it from headers: the Moonlight ports are internet-facing, so headers are spoofable.
- Wolf's stream sockets are IPv4-only (`udp::v4`/`tcp::v4` upstream) and it matches peers by exact
  IP string, so `wolfSessionFor` refuses non-IPv4 rather than handing Wolf a peer it can't reach.
- A Session whose IP fails `streamClientIP` ends while it has no pod (no port block, image pull or
  GPU claim for a stream that can't start). Once a pod exists it only fails the attach: a later
  /resume from an IPv4 address may still save the game.
  - Look the pod up, never trust `PodCreated`: it is False or missing while a pod runs after a
    failed reconcile or a lost status write. Tests: `TestUnstreamableClientIP*`.
- `spec.config.clientIP` has a CRD maxLength (45): the refusal quotes it twice in the
  StreamStarted message, and one over 32768 characters fails every status write.
- There is no default client IP: a placeholder makes Wolf stream to the wrong peer silently.
- Wolf does not read `WOLF_STREAM_CLIENT_IP`; don't reintroduce it as a way to pass the IP.
- `gh` defaults to the `upstream` remote (games-on-whales/fenrir); pass `--repo xerktech/fenrir`.
