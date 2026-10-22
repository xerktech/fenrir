---
paths:
  - pkg/controllers/app_pod.go
  - pkg/controllers/app_pod_test.go
  - pkg/controllers/session.go
  - pkg/inputbridge/**
  - cmd/wolf-agent/**
  - cmd/nri-input/**
  - pkg/api/v1alpha1/containers.go
---

# User-namespaced App pod (`--app-pod-host-dir`, XERK-1920)

- With the flag set, each Session's App runs in a second pod (`<session>-app`) with
  `hostUsers: false`, off the host network; Wolf, wolf-agent and pulseaudio stay in the Session pod.
  - Why: an App with SYS_ADMIN in the host user namespace mounted a devtmpfs (host keyboards) and,
    measured on talos04, remounted `/proc/sys` rw and opened `core_pattern` for write (node root).
    In a user namespace both fail (measured), and Steam's bwrap works without SYS_ADMIN given
    seccomp Unconfined plus `procMount: Unmasked` (Unmasked is valid only with `hostUsers: false`).
  - Not the whole pod: the apiserver refuses `hostUsers: false` with hostNetwork (Moonlight's
    port-only redirect), a user-namespaced Wolf can't open `/dev/uinput` (0660 root:996 on talos04),
    and kubelet refuses the per-netns `nf_conntrack_acct` sysctl the liveness check would need.
- Mode is per Session: the Session pod's `direwolf/app-pod` annotation, not the flag, says it is
  split, so a flag flip leaves running Sessions alone. The App pod is never recreated either:
  `AppPodCreated` True and no pod ends the Session. Tests: `TestFinishedAppPodEndsSession`.
- The pods share per-Session host dirs `<dir>/<session UID>/{runtime,udev,input}` (UID, not name:
  a new Session of the same name must not see old nodes). The App mounts udev and input read-only.
  - Read-only is not the boundary: the App's idmapped hostPath mounts aren't locked, so its root
    remounts them rw and wrote into both (QA on talos04). The bridge chowns `input/`, `udev/` and
    `udev/data/` to `LockedOwner`, an ID outside the App's idmap, then clears them; the App's root
    has no capability over an unmapped owner. Never let a privileged process trust a path there
    without that lock.
  - Files there (udev entries, `control`) are staged in `<dir>/<UID>` (no App mount) and renamed
    in already locked: created in place they are briefly the App root's, and an fd it opens then
    outlives the chown (QA won 48/50). `WriteLockedFile`; tests `TestLock*` need root (CI skips).
- A user-namespaced pod's volumes are all mounted `nodev` (PVC, hostPath, emptyDir; measured), and
  mknod is refused there, so a node in a shared volume never opens in the App pod.
  - The `input-bridge` sidecar (privileged, `<dir>` mounted Bidirectional) mknods each node into
    `input/` and bind-mounts it onto itself; the App's `input` mount is HostToContainer, so the
    node arrives as a mount of its own without nodev (measured: opens from the userns pod).
  - wolf-agent asks it over a unix socket in an emptyDir only the two mount
    (`--input-bridge-socket`); it publishes only major 13 directly in `input/`.
  - It writes the udev entries too: wolf-agent (drop ALL + MKNOD, no DAC_OVERRIDE) can't write the
    locked `udev/data/`, so it mounts neither hotplug directory in split mode.
  - It lstat-checks the new node is a char device before bind-mounting it.
  - The bridge unmounts before removing (a mount point unlinks EBUSY) and on exit clears and
    deletes `<dir>/<UID>`. A SIGKILLed bridge leaves that directory (and its mounts) on the node.
  - `<dir>` must sit on a mount with shared propagation on the node, or Bidirectional fails.
- The App pod's own network namespace doesn't hear the node's udevd, so Steam's Big Picture missed
  every pad plugged after it started (games, which enumerate at start, still saw them).
  - wolf-agent passes each udev event to the bridge (`uevent` op) after its node and entry exist;
    the bridge streams them on `<dir>/<UID>/events/events.sock` to the App pod's `udev-relay`,
    which broadcasts libudev monitor messages there (`fakeudev.MonitorMessage`, Wolf's format).
  - The relay alone mounts that socket; its NET_ADMIN (needed to send to the udev group, EPERM
    without it, measured) is the pod user namespace's, so it reaches only the pod's own network.
  - A relay that (re)connects gets an add for every device still announced; Clear announces
    removals, as Wolf destroys a stopped stream's devices without unplug events.
- nri-input grants by container name, so the App pod (it keeps the session label) still gets
  `13:*`/`241:*` and the seccomp filter; `input-bridge` is an operator container and gets nothing.
- spec.gpu's claim is a named ResourceClaim both pods reference; a template-generated claim is
  per pod (Wolf would get another card) and is refused when both pods use it.
- The stream waits for the App pod to be Ready as it waited for the game in one pod
  (`appPodReady`). The App pod has podAffinity to its Session pod's node.
- Tests: `app_pod_test.go`, `pkg/inputbridge`, `TestAgentPublishesNodesThroughBridge`.
