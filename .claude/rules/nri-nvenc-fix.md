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
    NVENC from a pod holding only gpu-0 with the annotation removed.
- **Opt-in by pod annotation `nvenc-fix.xerktech.com/inject: "true"`.** Every container of the pod
  gets the shim; nothing else is touched. It grants no access, so there is no namespace scope.
- **Injected as `/etc/ld.so.preload`, never `LD_PRELOAD`.** Images set `LD_PRELOAD` themselves and
  would replace ours; glibc always reads the file. musl ignores it, so Alpine sidecars are safe.
  - Verified on talos04: a container with its own `LD_PRELOAD` still gets the fix, and an Alpine
    sidecar in an annotated pod runs normally.
- **The shim is built on `manylinux_2_28`, not the Go toolchain's Debian.** A current glibc binds
  `dlsym@GLIBC_2.34` and `__isoc23_sscanf@GLIBC_2.38`; the preload then fails in older images.
  The built object needs only `GLIBC_2.14`.
- `shim/nvenc_fix.c` is vendored (GPLv3, `shim/COPYING`) with two local changes listed in its
  header. It is built into a separate `.so`; nothing in fenrir links it.
  - It fails open: if it cannot map a GPU ID to a device node it leaves the list unfiltered.
  - IDs map to minors via `/proc/driver/nvidia/gpus` by full PCI domain:bus:device; its
    `GET_ID_INFO` path returns status 0x1f on driver 595 and is only a first attempt.
- `--host-dir` must be the same path in the plugin's container and on the node: NRI mount sources
  resolve on the host. The files are written then renamed, so running containers keep their inode.
- Like nri-input, it acts only at container CREATE. A pod started while the plugin is down gets no
  shim until it is recreated.
