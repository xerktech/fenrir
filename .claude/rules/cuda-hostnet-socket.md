---
paths:
  - pkg/controllers/session.go
  - pkg/controllers/app_pod.go
  - pkg/controllers/ports.go
  - pkg/controllers/gpu_claim.go
---

# CUDA abstract unix socket on the host netns (XERK-2692)

Background for anyone touching the session pod's network namespace or GPU wiring. Do not spend a
GPU node and hours re-deriving this; it was probed live on talos04 (driver 595.91.07, CUDA 13.2).

## What the driver does (proven)

- `cuInit()` makes an abstract unix socket `@cuda-uvmfd-<pidns-inode>-<pid>@` (SOCK_SEQPACKET,
  listening) in the calling process's **network** namespace — not a filesystem path, not pidns- or
  uid-scoped. One per CUDA process. It appears the moment CUDA initialises, before any IPC call.
- Session pods are `hostNetwork`, so Wolf's socket lives in the **node** netns. Every other process
  in that netns sees it in `/proc/net/unix` and can `connect()` it: other `hostNetwork` pods
  (moonlight-proxy, node-exporter, CNI daemons, **other users' session pods**) and node daemons.
  Abstract sockets have no file permissions, and iptables / NetworkPolicy do not apply to them.
- Any uid may connect; the server answers `OUTBHELL`. libcuda's protocol tags are
  `OUTBHELL` / `OUTBCUFD` / `OUTBCRED` (handshake / fd-passing / peer-credentials): the endpoint
  passes file descriptors (the UVM fd) over `SCM_RIGHTS`.
- Same-netns CUDA IPC is **not uid-gated**: a uid-1001 process imported a uid-1000 process's
  `cuIpcGetMemHandle` handle (`cuIpcOpenMemHandle`) and read its GPU memory back. The uvmfd socket
  exists precisely to pass the UVM fd across namespaces, so treat a cross-pod reader in the shared
  host netns as real too (not run here — baseline PSA and the cluster-mutation guard block standing
  up two `hostNetwork` GPU pods from a session). The normal-API attack needs a leaked IPC handle;
  reaching GPU memory from the socket alone needs the raw `OUTB*` protocol.

## Why there is no clean code fix

- **No CUDA knob disables the socket.** Grepping libcuda turns up only MPS and IPC-peer env vars
  (`CUDA_MPS_*`, `CU_IPC_MEM_LAZY_ENABLE_PEER_ACCESS`); nothing suppresses the uvmfd listener. It is
  created unconditionally.
- **Wolf cannot leave hostNetwork.** The conntrack stream-liveness check reads the node's conntrack
  and needs the per-netns `net.netfilter.nf_conntrack_acct` sysctl kubelet refuses in a pod netns;
  Moonlight only accepts a port-only redirect on the node IP; `/dev/uinput`; and the apiserver
  refuses `hostUsers: false` with hostNetwork (see `app-pod.md`). So the CUDA process stays in the
  node netns by construction.

## Mitigation posture (the XERK-2692 decision)

- **Untrusted game → Wolf** (the game shares the session pod's netns even without hostNetwork, since
  pod containers share one netns): contained by **`--app-pod-host-dir`** — the split App pod gives
  the game its own netns, so it cannot see Wolf's socket. This is the containment for untrusted game
  code; recommend it wherever that matters.
- **Host-netns co-tenant → Wolf** (another pod on the node): inherent to hostNetwork. Accepted as
  defense-in-depth — talos04's co-tenants are first-party/admin workloads, and the untrusted surface
  (the game) is handled above. Full isolation would need Wolf off hostNetwork (replace the conntrack
  liveness check, move port handling to CNI port-mapping): a multi-day architectural change, deferred.
- If you ever add a second CUDA/NVENC user to a host-networked pod, it inherits this exposure —
  revisit rather than assuming it is covered.
