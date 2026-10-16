---
paths:
  - cmd/wolf-agent/shim/**
  - cmd/wolf-agent/shim_test.go
---

# Loopback shim tests

- `TestLoopbackShim*` SKIP (exit 0) without `cc` and `python3` on PATH: a green run proves nothing
  until `-v` shows them PASS.
  - Hosts without gcc: `zig cc` behind a `cc` wrapper works; `go test -race` also needs
    `CGO_ENABLED=1 CC=cc`, and `TMPDIR` off a noexec `/tmp`.
- Ports come from `withFreePorts`, which retries on EADDRINUSE: freePort's port is released and
  ephemeral, so any outgoing connection can take it before the test binds (XERK-1978).
  - The shim reads `WOLF_HTTP(S)_PORT` at load, so the ports can't come from binding port 0.
  - Each retry is logged with Python's output: a listener the shim leaks surfaces there, or in
    the final failure if it happens every time.
