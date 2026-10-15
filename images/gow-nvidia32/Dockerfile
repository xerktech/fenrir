# syntax=docker/dockerfile:1
# games-on-whales' Steam and Heroic images plus NVIDIA's 32-bit (i386) OpenGL,
# EGL and Vulkan driver libraries (XERK-1930). A session's GPU claim injects
# the 64-bit driver only: Talos' nvidia-container-toolkit extension ships no
# lib32, so the CDI spec has none to inject. Without these, every 32-bit game
# (most Proton/Wine titles' D3D9-11 via DXVK, 32-bit native games) finds no
# NVIDIA ICD and renders on the CPU through Mesa's lavapipe/llvmpipe.
#
# NVIDIA's userspace must match the node's kernel module exactly. Bump
# NVIDIA_VERSION and NVIDIA_SHA256 together with Talos'
# nvidia-open-gpu-kernel-modules extension; until then the cont-init hook
# removes the libraries on a mismatch, so 32-bit games fall back to the CPU as
# before rather than fail to start. One Dockerfile, one target per image.

FROM ghcr.io/games-on-whales/steam:edge@sha256:ded0b1b47acd9adb8af9f068342f26ac31008904d9bbb91045d1a04e7d66a632 AS driver
ARG NVIDIA_VERSION=595.71.05
ARG NVIDIA_SHA256=36203b8960b7e49c8a42f6ba1f5863cde34a05e93d2b24b798af06dd846f6b82
ADD --checksum=sha256:${NVIDIA_SHA256} \
  https://download.nvidia.com/XFree86/Linux-x86_64/${NVIDIA_VERSION}/NVIDIA-Linux-x86_64-${NVIDIA_VERSION}.run /tmp/nvidia.run
# Only the client libraries graphics needs (no CUDA/NVENC/OpenCL), and not the
# glvnd dispatchers (libGL, libEGL, libGLX): the base image's own are used.
RUN set -eu; \
  sh /tmp/nvidia.run -x --target /tmp/nvidia >/dev/null; \
  out=/out/usr/lib/i386-linux-gnu; mkdir -p "$out"; \
  for lib in libGLX_nvidia libEGL_nvidia libGLESv1_CM_nvidia libGLESv2_nvidia \
      libnvidia-glcore libnvidia-eglcore libnvidia-glsi libnvidia-tls \
      libnvidia-glvkspirv libnvidia-gpucomp; do \
    cp "/tmp/nvidia/32/${lib}.so.${NVIDIA_VERSION}" "$out/"; \
  done; \
  mkdir -p /out/etc/direwolf; echo "${NVIDIA_VERSION}" > /out/etc/direwolf/nvidia-lib32-version

# ldconfig creates each library's soname link (libGLX_nvidia.so.0, which the
# Vulkan ICD manifest names) and the cache entry pressure-vessel resolves from.
FROM ghcr.io/games-on-whales/steam:edge@sha256:ded0b1b47acd9adb8af9f068342f26ac31008904d9bbb91045d1a04e7d66a632 AS steam
COPY --from=driver /out/ /
COPY root/ /
RUN ldconfig && ldconfig -p | grep -q 'libGLX_nvidia.so.0 (libc6)'

FROM ghcr.io/games-on-whales/heroic-games-launcher:edge@sha256:a207a39367801a8a8bf24b68dac2c9945052d3117cb03b061b27bd6672aab62f AS heroic
COPY --from=driver /out/ /
COPY root/ /
RUN ldconfig && ldconfig -p | grep -q 'libGLX_nvidia.so.0 (libc6)'
