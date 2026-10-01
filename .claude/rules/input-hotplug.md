---
paths:
  - pkg/fakeudev/**
  - pkg/controllers/agent.go
  - pkg/controllers/session.go
  - pkg/controllers/session_test.go
  - cmd/nri-input/**
---

# Controller hotplug into the game container

- Path chosen: wolf-agent mknods each `/dev/input/*` node into an emptyDir mounted at `/dev/input`
  in wolf-agent and every App container, and writes `/run/udev/data` into a second shared emptyDir.
  - Don't switch to a hostPath `/dev/input`: it hands the game every host input device.
- Measured on Talos v1.13.8 / containerd 2.2.6 (XERK-1303, a busybox pod on talos03):
  - The emptyDir mounts `rw,relatime`, not `nodev`: a node mknod'ed by one container opens from a
    sibling running non-root with all caps dropped.
  - Opening is gated only by the device cgroup, by number and not path: 13:64 gave EPERM without
    the `cmd/nri-input` grant (13:* rwm). Re-check `nodev` if Talos or kubelet changes.
- wolf-agent needs CAP_MKNOD (runtime default; dropping ALL in its sidecar policy breaks hotplug).
- Only char major 13 under `/dev/input/` is created (`CreateDeviceNode`); a Wolf event can't make
  a disk node. `/dev/hidraw*` is not handled: pads are uinput Xbox pads (no uhid on Talos).
- An App that mounts its own `/dev/input` keeps it (`withHotplugMounts` skips taken paths).
- Tests: `TestSessionPodSharesHotplugVolumes`, `TestSessionPodKeepsAppsOwnInputMount`
  (`pkg/controllers/session_test.go`); `TestCreateAndRemoveDeviceNode` (`pkg/fakeudev`).
