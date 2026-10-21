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
#
# Also NVIDIA's NGX Wine DLLs (XERK-2073), which the CDI spec leaves out too.
# Proton copies nvngx.dll and _nvngx.dll into a prefix only when they sit in
# <dir of libGLX_nvidia.so.0>/nvidia/wine; Talos injects that library into
# /usr/lib. Without them games offer no DLSS or DLSS Frame Generation.

# Only unpacks the .run, so any base with sh and coreutils does (it carries its
# own zstd): not a GoW image, which the heroic leg would pull just for this,
# and which builder.yml's mode=max cache would store once more per leg.
FROM debian:trixie-slim@sha256:a29215f6a35e51e22adffa17f89e9d2ef06214e64a2bad10d765c46aea49f11f AS driver
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
  # The .run stores these 0700 (its installer applies the manifest's 0755), and
  # Proton, running as the desktop user, aborts every launch on an unreadable one.
  install -D -m 0755 -t /out/usr/lib/nvidia/wine \
    /tmp/nvidia/nvngx.dll /tmp/nvidia/_nvngx.dll /tmp/nvidia/nvngx_dlssg.dll; \
  mkdir -p /out/etc/direwolf; echo "${NVIDIA_VERSION}" > /out/etc/direwolf/nvidia-lib32-version; \
  # The 1.6 GB unpacked tree would otherwise sit in this layer and the cache.
  rm -rf /tmp/nvidia

# ldconfig creates each library's soname link (libGLX_nvidia.so.0, which the
# Vulkan ICD manifest names) and the cache entry pressure-vessel resolves from.
FROM ghcr.io/games-on-whales/steam:edge@sha256:ded0b1b47acd9adb8af9f068342f26ac31008904d9bbb91045d1a04e7d66a632 AS steam
COPY --from=driver /out/ /
COPY --chmod=755 root/etc/cont-init.d/05-nvidia-lib32.sh /etc/cont-init.d/
RUN ldconfig && ldconfig -p | grep -q 'libGLX_nvidia.so.0 (libc6)' \
  && gosu nobody sh -c 'cd /usr/lib/nvidia/wine && test -r nvngx.dll -a -r _nvngx.dll -a -r nvngx_dlssg.dll'
# Steam Input off for every installed game: it needs /dev/uinput (XERK-1953).
COPY --chmod=755 root/etc/cont-init.d/20-steam-input-off.sh /etc/cont-init.d/
# No --chmod here: it also applies to the /opt/direwolf it creates, and a 644
# directory hides the script from the desktop user. Run it as one to be sure.
COPY root/opt/direwolf/steam-input-off.py /opt/direwolf/
RUN gosu nobody python3 -I /opt/direwolf/steam-input-off.py /nonexistent
# GoW's cont-init starts NetworkManager, which in a hostNetwork session pod
# runs in the node's network namespace: keep it off the node's links.
COPY root/etc/NetworkManager/conf.d/99-direwolf-hostnetwork.conf /etc/NetworkManager/conf.d/
RUN NetworkManager --print-config | grep -qx 'no-auto-default=\*' \
  && NetworkManager --print-config | grep -qx 'unmanaged-devices=\*,except:type:loopback,except:type:bridge'
# GoW's cont-init starts Decky Loader as root on 127.0.0.1:1337, the node's
# loopback under hostNetwork, and serves its auth token there to any caller;
# installing it also turns on Steam's CEF debugger, which Decky drives. Nothing
# uses it (XERK-2128). Cut the block, and fail the build if GoW reworks it: a
# missed end line would delete through EOF, so the cut must hash to the block
# as pinned (the GoW digest above); re-read it and update the hash on a bump.
RUN f=/etc/cont-init.d/system-services.sh \
  && r='/^# Install Decky Loader/,/^\$HOME\/homebrew\/services\/PluginLoader &$/' \
  && test "$(sed -n "${r}p" "$f" | sha256sum)" = \
    'f2d3dbe79ec4569e0adcd18aabefefa62fbed560551f25301e50d39b81b06990  -' \
  && sed -i "${r}d" "$f" \
  && ! grep -n -i -e decky -e PluginLoader -e homebrew -e cef-enable "$f" \
  && grep -qx 'steamos-dbus-watchdog.sh &' "$f" && grep -qx disown "$f" \
  && rm -r /opt/decky
# GoW ships some cont-init scripts world-writable (0777), and its entrypoint
# sources each as root: only root may change them.
RUN chmod -R go-w /etc/cont-init.d && test -z "$(find /etc/cont-init.d ! -type l -perm /022)"

FROM ghcr.io/games-on-whales/heroic-games-launcher:edge@sha256:a207a39367801a8a8bf24b68dac2c9945052d3117cb03b061b27bd6672aab62f AS heroic
COPY --from=driver /out/ /
COPY --chmod=755 root/etc/cont-init.d/05-nvidia-lib32.sh /etc/cont-init.d/
RUN ldconfig && ldconfig -p | grep -q 'libGLX_nvidia.so.0 (libc6)' \
  && gosu nobody sh -c 'cd /usr/lib/nvidia/wine && test -r nvngx.dll -a -r _nvngx.dll -a -r nvngx_dlssg.dll'
RUN chmod -R go-w /etc/cont-init.d && test -z "$(find /etc/cont-init.d ! -type l -perm /022)"
