---
paths:
  - pkg/fakeudev/**
  - pkg/controllers/agent.go
  - pkg/controllers/agent_lobby.go
  - pkg/controllers/session.go
  - pkg/controllers/session_test.go
  - cmd/nri-input/**
  - pkg/api/v1alpha1/containers.go
  - images/gow-nvidia32/**
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
- The App's containers (init too) always lose CAP_MKNOD (`dropMknod`, even if the App adds it):
  `13:*` plus containerd's default `c *:* m` would let a root App mknod and open host keyboards or
  another session's pads (XERK-1338). Tests: `TestSessionPodDropsAppMknod`.
  - containerd prefixes `CAP_` itself: a listed `CAP_MKNOD` drop is a no-op (`CAP_CAP_MKNOD`), so
    only a bare `MKNOD` counts as dropped. Tests: `TestDropMknod`.
  - NRI can't narrow the grant per minor: device rules are set only at container create.
  - A privileged App container, or any App hostPath volume, is refused
    (`validateAppConfinement`, XERK-1915). Tests: `TestBuildPodRejectsAppHostDevices`.
  - Not covered: an App adding SYS_ADMIN mounts a devtmpfs and opens every `13:*`/`241:*` node,
    host keyboards included. The Steam/Heroic Apps add SYS_ADMIN with seccomp Unconfined.
  - Steam can't drop SYS_ADMIN: Proton's bwrap mounts a fresh `/proc`, which a masked `/proc`
    forbids (EPERM, measured on talos04); `procMount: Unmasked` needs `hostUsers: false`, which
    the apiserver rejects with hostNetwork. A per-minor eBPF device filter doesn't help either:
    SYS_ADMIN with no seccomp detaches its own `cgroup_device` program via bpf() and then opens
    any device, host disks included (measured on talos04, XERK-1915).
- `cmd/nri-input` gives every seccomp-Unconfined App container a default-allow filter denying bpf,
  perf_event_open, module/kexec loading, open_by_handle_at, keyctl and the like (`deniedSyscalls`,
  XERK-1915). Others keep their profile untouched.
  - An NRI seccomp adjustment replaces the whole policy, and containerd 2.2.6 reports only the
    profile type, never the resolved policy: extending RuntimeDefault would replace it.
  - containerd's RuntimeDefault opens denied syscalls to some caps (bpf to SYS_ADMIN/BPF,
    open_by_handle_at to DAC_READ_SEARCH, ...), so the operator refuses an App container adding one
    under RuntimeDefault, unset included (talos04's kubelet runs `seccompDefault`).
    Such an App must ask for Unconfined (`defaultProfileHoleCapability`).
  - The filter lists the 32-bit arches, or Proton's 32-bit games are killed.
  - Never set a rule's `ErrnoRet`: containerd 2.2.6 vendors NRI v0.11.0, whose
    `ToOCILinuxSyscalls` writes it through a nil pointer, and containerd panicked on talos04 at the
    first Steam container. runc already returns EPERM for an ERRNO action. Recheck before using
    any other seccomp field once containerd bumps NRI.
  - seccomp can't see a mount's fstype or path, so devtmpfs and procfs mounts stay possible.
  - Tests: `TestConfinedSeccomp*`, `TestCreateContainerConfinesOnlyApp`.
- `cmd/nri-input` grants by container name (`pkg/api/v1alpha1/containers.go`): only `wolf` gets
  `/dev/uinput`; wolf-agent, pulseaudio and the init containers get nothing (wolf-agent's mknod
  is covered by containerd's default `c *:* m`, and it opens nodes only `O_PATH`); every other
  container, i.e. the App's, gets `13:*` and `241:*`. Tests: `TestAdjustment*` (`cmd/nri-input`),
  `TestSessionPodOperatorContainerNames`.
- Only char major 13 under `/dev/input/` is created (`CreateDeviceNode`); a Wolf event can't make
  a disk node. `/dev/hidraw*` is not handled: pads are uinput Xbox pads (no uhid on Talos).
- An App that mounts its own `/dev/input` (not hostPath) keeps it (`withHotplugMounts` skips
  taken paths).
  A wolfAgent sidecar policy may not mount there (`validateNoHotplugOverride`).
  - Compare mounts via `mountTarget`: the apiserver accepts relative mountPaths (`run/udev/data`),
    which the runtime resolves against `/`; `/var/run` (Alpine symlink) is reserved as an alias.
  - It guards misconfiguration only: a User can already mount hostPath into sidecars.
  - Keep the hotplug mounts appended after policy mounts: a policy mount at `/var/run` or `/run`
    only loses to `/run/udev` because it is mounted first.
- Wolf sends no unplug when a stream stops: it destroys the devices on StopStream. The agent
  clears both dirs on pause (`clearDevices`), or a stale 0666 node would open whatever device the
  kernel gives that minor next (another session's keyboard). Tests: `TestAgentPauseClearsDevices`.
- Joining a stream to the Wolf lobby moves its joypads with an unplug addressed to the stream; once
  joined, real unplugs are re-fired addressed to the lobby. So the agent skips unplugs to a
  joining/joined stream (`ignoresUnplug`). Tests: `TestAgentKeepsDevicesMovedToLobby`.
- The dirs are writable by the app container, which can swap a node for a symlink at any moment:
  never chmod/chown by path after mknod (`setNodeMode` goes through an O_PATH fd).
  Tests: `TestSetNodeModeRefusesSwappedNode`.
- CI runs tests non-root, so mknod EPERMs there: logic tests stub `makeCharNode`, or a refusal
  test passes for the wrong reason.
- No synthetic udev netlink event: under hostNetwork it needs CAP_NET_ADMIN on the host netns and
  then reaches every hostNetwork process on the node. The App hears the node's udevd instead
  (XERK-1337, measured on talos04):
  - libudev subscribes to udev events only if `/run/udev/control` exists; without it SDL2 missed
    every pad plugged after it started. The `init` init container creates it in the udev volume, as
    libudev checks once, at monitor creation. Tests: `TestSessionPodCreatesUdevControl`.
  - Ordering relies on udevd being slower than wolf-agent: its event came 29-71ms after the mknod
    (14/14). SDL does not retry an add whose node is missing, so a slow agent loses the pad.
  - The App also sees every other uevent on the node (host netns); it can open only its own nodes.
  - `SDL_JOYSTICK_DISABLE_UDEV=1` (inotify on /dev/input) also works, but only for SDL.
  - talos04 has no `joydev`: pads get `eventN` only, no `jsN`.
- Steam Input can't work in an App: Steam builds its virtual pad on `/dev/uinput`, which only `wolf`
  gets, and a uinput node would land in host devtmpfs, not the agent's `/dev/input` (XERK-1931).
  - With Steam Input on for a game, Steam still lists Wolf's pad (`0x045e/0x02ea`) in the game's
    `SDL_GAMECONTROLLER_IGNORE_DEVICES`; Proton's winebus drops it and the game sees no pad, while
    Big Picture works. Steam logs `Couldn't initialize virtual gamepad: Couldn't open /dev/uinput`.
  - Fix: Steam Input "Disabled" (`localconfig.vdf` `UseSteamControllerConfig` "0"); Steam
    then drops the ID from the list. `SDL_GAMECONTROLLER_IGNORE_DEVICES_EXCEPT` in the App env
    does not help.
  - The Steam image forces that for every app in `libraryfolders.vdf`, in both places Steam keeps
    it, before Steam starts (`images/gow-nvidia32` cont-init `20-steam-input-off.sh`, XERK-1953).
    It overrides the user's choice; it never fails the pod and leaves an unparseable file alone.
  - Not chosen: uinput for Apps with wolf-agent mirroring their nodes; it undoes XERK-1433's
    confinement. Tests: `images/gow-nvidia32/steam_input_off_test.go`.
  - Repro without Moonlight: privileged pod with the App image and Steam home, a uinput Xbox pad
    (045e:02ea v0408) mknod'ed into a tmpfs `/dev/input`, sway headless, `PROTON_LOG=1
    WINEDEBUG=+hid,+xinput`; grep the log for `bus_create_hid_device desc {vid 045e`.
- Pod-spec tests: `TestSessionPodSharesHotplugVolumes`, `TestSessionPodKeepsAppsOwnInputMount`
  (`pkg/controllers/session_test.go`); `TestCreateAndRemoveDeviceNode` (`pkg/fakeudev`).
