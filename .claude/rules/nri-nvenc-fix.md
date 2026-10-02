---
paths:
  - cmd/nri-nvenc-fix/**
---

# nri-nvenc-fix

- Works around an NVIDIA driver regression (570 and later, still present in 595): in a container
  holding some of a node's GPUs, NVENC returns `NV_ENC_ERR_UNSUPPORTED_DEVICE` and NVDEC
  `CUDA_ERROR_NO_DEVICE`. CUDA is unaffected, so nothing but an encode/decode attempt shows it.
  - RM's `GPU_GET_ATTACHED_IDS` lists every host GPU; the video libraries then peer-init a
    "primary" GPU (smallest UUID) and need its `/dev/nvidiaN`. On talos04 that is gpu-3 (the Dell
    3090), which is why the HP 3090s looked broken (XERK-1350).
  - Delete this plugin once a driver Talos ships fixes it (reportedly 610.x). Re-test first: probe
    NVENC from a pod holding only gpu-0, annotated `inject: "false"`.
- **Automatic for every container holding some, but not all, GPU nodes (`/dev/nvidiaN`)**: what a
  DRA GPU claim gives. Opt-in left video workloads working on the primary GPU only, and no workload
  is pinned to a card.
  - Detected by device node, not CDI name: containerd 2.2 (NRI v0.11) hands the plugin an empty
    `CDIDevices`; the claimed `/dev/nvidia0` is in `Linux.Devices`. Seen on talos04 with a probe.
  - **A container holding every node is skipped.** containerd gives a privileged container all host
    devices (`oci.WithHostDevices`), and talos04 runs ~17 privileged infra pods (kube-proxy, Longhorn,
    the DRA kubelet plugin, dcgm). They never hit the bug; without the skip every ioctl of theirs,
    NVML's included, would run through the shim. The count is `/proc/driver/nvidia/gpus`, which
    procfs shows to any container; unreadable means inject.
  - Pod annotation `nvenc-fix.xerktech.com/inject`: `"false"` opts the pod out, `"true"` injects
    every container (GPU or not), either case. Anything else means the default.
  - Containers in the pod without the claim (init, sidecars) are untouched.
  - An image's own `/etc/ld.so.preload` (jemalloc and the like) is hidden in every container that
    gets the shim. That, or a proven CUDA problem, is what the opt-out is for.
  - Made the default only after a CUDA no-regression gate on a 3090 and the PRO 6000 (XERK-1388).
    It grants no access, so there is no namespace scope.
- **Injected as `/etc/ld.so.preload`, never `LD_PRELOAD`.** Images set `LD_PRELOAD` themselves and
  would replace ours; glibc always reads the file. musl ignores it, so Alpine sidecars are safe.
  - Verified on talos04: a container with its own `LD_PRELOAD` still gets the fix, and an Alpine
    sidecar in an annotated pod runs normally.
  - It hides a `/etc/ld.so.preload` the image ships itself (the plugin can only see mounts).
- **The preload entry is `/usr/lib/nvenc-fix/$PLATFORM/libnvenc_fix.so`.** One 64-bit path made every
  32-bit process (Steam client, Wine) print `wrong ELF class` on each exec.
  - `i686/` holds an EMPTY object: no 32-bit process encodes, and the shim's RM structs are 64-bit.
  - glibc names x86_64 CPUs `x86_64`, `haswell` or `xeon_phi`, so the extra names are symlinks.
    A missing one means "cannot open shared object" on every exec. Checked on Debian 13, Ubuntu
    24.04, Fedora 42, Arch, and i386 Debian 12.
- A pod volume at, above or below `/etc/ld.so.preload` or `/usr/lib/nvenc-fix` (`/etc`, `/usr/lib`,
  `/usr/lib/nvenc-fix/x`) makes the plugin skip that container: runc cannot create a mountpoint
  inside a read-only bind, so container creation would fail.
- **The shim is built on `manylinux_2_28`, not the Go toolchain's Debian.** A current glibc binds
  `dlsym@GLIBC_2.34` and `__isoc23_sscanf@GLIBC_2.38`; the preload then fails in older images.
  The built object needs only `GLIBC_2.17`.
- `shim/nvenc_fix.c` is vendored (GPLv3, `shim/COPYING`) with local changes listed in its header. It is built into a separate `.so`; nothing in fenrir links it.
  - It fails open only if it can map NO GPU ID to a device node. An ID it can't map while others map
    is dropped, so a `/proc/driver/nvidia/gpus` mismatch hides that GPU from the container.
  - IDs map to minors via `/proc/driver/nvidia/gpus` by full PCI domain:bus:device; its
    `GET_ID_INFO` path returns status 0x1f on driver 595 and is only a first attempt.
- `--host-dir` must be the same path in the plugin's container and on the node: NRI mount sources
  resolve on the host. The files are written then renamed, so running containers keep their inode.
- Like nri-input, it acts only at container CREATE. A pod started while the plugin is down gets no
  shim until it is recreated.
