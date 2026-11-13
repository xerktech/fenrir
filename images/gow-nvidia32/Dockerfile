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
ARG NVIDIA_VERSION=595.91.07
ARG NVIDIA_SHA256=ca23c88dd24b07a191644e1e11cfb7bcdd7537305749af40f980018b095e6313
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
# GoW's images set UMASK=000 (10-setup_user.sh applies it before the entrypoint
# drops to the user), so sessions left world-writable files in the shared home
# and /games, e.g. ~/.cache/vkd3d-proton 0777 (XERK-2351). Sessions and Library
# all write them as uid 1000, so group/other write is never needed.
ENV UMASK=022
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
# GoW ships some cont-init and startup scripts world-writable (0777), and runs
# them as root (the entrypoint sources cont-init.d; system-services.sh starts
# steamos-dbus-watchdog.sh): only root may change them.
RUN d='/etc/cont-init.d /opt/gow /usr/local/bin /usr/bin' \
  && find $d ! -type l -perm /022 -exec chmod go-w {} + && test -z "$(find $d ! -type l -perm /022)"
# Assetto Corsa's Documents folder in the shared Proton prefix is a link to
# /srv/direwolf/ac-documents (examples/vr.yaml, XERK-2713). Here it leads to
# the shared flat copy; the vr image points it at its User's mount instead.
# Never under /srv/direwolf/state, the stateMounts root: the runtime would
# follow an image link there into the home. /srv: pressure-vessel shares it.
# AC is in Steam's own library in the shared home, like every Steam game.
RUN mkdir -p /srv/direwolf \
 && ln -s '/home/retro/.steam/debian-installation/steamapps/compatdata/244210/pfx/drive_c/users/steamuser/Documents/Assetto Corsa.flat' \
      /srv/direwolf/ac-documents \
 && test ! -e /srv/direwolf/state

FROM ghcr.io/games-on-whales/heroic-games-launcher:edge@sha256:a207a39367801a8a8bf24b68dac2c9945052d3117cb03b061b27bd6672aab62f AS heroic
# As in steam: it writes the same home and /games. nexus inherits it.
ENV UMASK=022
COPY --from=driver /out/ /
COPY --chmod=755 root/etc/cont-init.d/05-nvidia-lib32.sh /etc/cont-init.d/
RUN ldconfig && ldconfig -p | grep -q 'libGLX_nvidia.so.0 (libc6)' \
  && gosu nobody sh -c 'cd /usr/lib/nvidia/wine && test -r nvngx.dll -a -r _nvngx.dll -a -r nvngx_dlssg.dll'
RUN d='/etc/cont-init.d /opt/gow /usr/local/bin /usr/bin' \
  && find $d ! -type l -perm /022 -exec chmod go-w {} + && test -z "$(find $d ! -type l -perm /022)"

# Xodus Nexus (Microsoft Store / Xbox PC games, github.com/domsvoboda/xodus-nexus),
# unpacked from its AppImage: pods have no FUSE to mount one. Run unpacked, it
# never self-updates (Nexus updates only a running AppImage), so the version
# moves only with this pin.
FROM debian:trixie-slim@sha256:a29215f6a35e51e22adffa17f89e9d2ef06214e64a2bad10d765c46aea49f11f AS nexus-app
# renovate: datasource=github-releases depName=domsvoboda/xodus-nexus
ARG NEXUS_VERSION=v0.4.2
# Must be updated by hand with the version (the release's .sha512sum asset): a
# Renovate bump without it fails the build closed.
ARG NEXUS_SHA512=dbc22dbd3c4311f3a0f2d021fdc34bfed97c4117146a85dd20a51297714a12aaa75cae272874c183a460b4715496e305b4ebc2dbb7300b7d478b09d2d972a320
RUN set -eu; \
  apt-get update; apt-get install -y --no-install-recommends ca-certificates curl; \
  curl -fsSL -o /tmp/nexus.AppImage \
    "https://github.com/domsvoboda/xodus-nexus/releases/download/${NEXUS_VERSION}/Xodus-Nexus-${NEXUS_VERSION#v}-x86_64.AppImage"; \
  echo "${NEXUS_SHA512}  /tmp/nexus.AppImage" | sha512sum -c -; \
  chmod +x /tmp/nexus.AppImage; \
  cd /tmp && ./nexus.AppImage --appimage-extract >/dev/null; \
  test -x /tmp/squashfs-root/AppRun; \
  # Drop the setuid sandbox helper: xodus-nexus uses user namespaces or --no-sandbox.
  chmod 0755 /tmp/squashfs-root/chrome-sandbox 2>/dev/null || true; \
  # --appimage-extract makes its directories 0700 root: the desktop user
  # must read them, and nobody but root may write.
  chmod -R u=rwX,go=rX /tmp/squashfs-root; \
  test -z "$(find /tmp/squashfs-root ! -type l \( ! -perm -o=r -o -perm /022 \))"; \
  mv /tmp/squashfs-root /opt/xodus-nexus; \
  # xodus-nexus-update compares releases against this.
  echo "${NEXUS_VERSION#v}" >/opt/xodus-nexus/.direwolf-version

# Proton-CachyOS for Battle.net, the Library's (images/library/Dockerfile,
# XERK-2361) at the same pin: both run the one prefix in the shared home, and
# Proton upgrades or warns about a prefix another version last ran. Bump both
# together (TestBattlenetProtonMatchesLibrary). Root-owned, read-only to the user.
FROM debian:trixie-slim@sha256:a29215f6a35e51e22adffa17f89e9d2ef06214e64a2bad10d765c46aea49f11f AS proton-cachyos
ARG PROTON_CACHYOS_VERSION=cachyos-11.0-20261005-slr
ARG PROTON_CACHYOS_SHA512=457bee6051ee671509e1cd6b865246df00685e508908cd0cf98c62fd311d0c781ce80fca3e9500e89fd7e48463fa26335806cdb1c38e528fd0e3e2b6b66c4469
RUN set -eu; \
  apt-get update; apt-get install -y --no-install-recommends ca-certificates curl xz-utils; \
  name="proton-${PROTON_CACHYOS_VERSION}-x86_64"; \
  curl -fsSL -o /tmp/proton.tar.xz \
    "https://github.com/CachyOS/proton-cachyos/releases/download/${PROTON_CACHYOS_VERSION}/${name}.tar.xz"; \
  echo "${PROTON_CACHYOS_SHA512}  /tmp/proton.tar.xz" | sha512sum -c -; \
  tar -xJf /tmp/proton.tar.xz --no-same-owner -C /tmp; \
  test -x "/tmp/${name}/proton"; \
  # Proton writes into its own directory only to build a missing default prefix.
  test -f "/tmp/${name}/files/share/default_pfx/system.reg"; \
  chmod -R u=rwX,go=rX "/tmp/${name}"; \
  mv "/tmp/${name}" /opt/proton-cachyos

# What the game-session targets (nexus, battlenet) share over heroic: one
# fullscreen game, no desktop, and the session ends when the game exits.
FROM heroic AS game-session
# NVIDIA's driver trims its shader cache past its size cap in the first OpenGL
# process of a session, sway, before the game starts: Gears of War: E-Day's
# alone passes the cap (about 1.2 GB), so every launch recompiled (XERK-2349).
# Session-wide, so sway and Xwayland inherit it, not just the game. Steam sets
# it too.
ENV __GL_SHADER_DISK_CACHE_SKIP_CLEANUP=1
# Easy Anti-Cheat games need CAP_SYS_PTRACE under Talos' ptrace_scope=2 (see
# run-as-user, XERK-2349): the entrypoint drops to the user through it instead
# of gosu. The grep fails the build if a GoW bump changes that line.
COPY --chmod=755 root/opt/direwolf/run-as-user /opt/direwolf/run-as-user
RUN grep -qx 'exec gosu "${UNAME}" /opt/gow/startup.sh' /entrypoint.sh \
  && sed -i 's|^exec gosu "${UNAME}" /opt/gow/startup.sh$|exec /opt/direwolf/run-as-user /opt/gow/startup.sh|' /entrypoint.sh \
  && grep -qx 'exec /opt/direwolf/run-as-user /opt/gow/startup.sh' /entrypoint.sh
# No waybar in game sessions: the game fills the screen (XERK-2290; see
# direwolf-game.conf). launch-comp.sh copies /cfg/sway/config into the home at
# every start; the include goes last so its values override GoW's. The grep
# fails the build if a GoW bump renames the bar line.
COPY --chmod=644 root/cfg/sway/direwolf-game.conf /cfg/sway/direwolf-game.conf
RUN grep -qx 'bar swaybar_command waybar' /cfg/sway/config \
  && sed -i '/^bar swaybar_command waybar$/d' /cfg/sway/config \
  && ! grep -q '^bar' /cfg/sway/config \
  && echo 'include /cfg/sway/direwolf-game.conf' >>/cfg/sway/config

# Sessions of Microsoft Store games installed with Xodus Nexus (catalogue
# store "nexus"): Nexus, its keyring, and a startup that runs
# $DIREWOLF_LAUNCH_COMMAND.
FROM game-session AS nexus
RUN apt-get update \
  && apt-get install -y --no-install-recommends gnome-keyring libsecret-1-0 dbus \
  && rm -rf /var/lib/apt/lists/*
COPY --from=nexus-app /opt/xodus-nexus /opt/xodus-nexus
COPY --chmod=755 root/usr/bin/xodus-nexus /usr/bin/xodus-nexus
COPY --chmod=755 root/opt/direwolf/launch-nexus /opt/direwolf/launch-nexus
COPY --chmod=755 root/opt/gow/nexus-startup-app.sh /opt/gow/startup-app.sh
COPY --chmod=755 root/usr/local/bin/xodus-nexus-update /usr/local/bin/xodus-nexus-update
COPY --chmod=755 root/etc/cont-init.d/30-xodus-nexus-update.sh /etc/cont-init.d/
# The Library's patched-xgameruntime step, re-run at session start (XERK-2654);
# identical to images/library's (TestXodusNexusGamePatchesMatchSessionImage).
COPY --chmod=755 root/usr/local/bin/xodus-nexus-game-patches /usr/local/bin/xodus-nexus-game-patches
COPY --chmod=644 root/usr/share/xodus-nexus/xgameruntime/ /usr/share/xodus-nexus/xgameruntime/
COPY --chmod=755 root/etc/cont-init.d/31-xodus-nexus-game-patches.sh /etc/cont-init.d/
RUN test -x /opt/xodus-nexus/AppRun && command -v gnome-keyring-daemon dbus-run-session dbus-send >/dev/null \
  && command -v python3 curl sha256sum gosu timeout >/dev/null \
  && chmod 0755 /usr/share/xodus-nexus /usr/share/xodus-nexus/xgameruntime /usr/share/xodus-nexus/xgameruntime/patches \
  && command -v flock >/dev/null \
  && gosu nobody cat /usr/share/xodus-nexus/xgameruntime/games /usr/share/xodus-nexus/xgameruntime/patches/*.patch >/dev/null \
  && d='/etc/cont-init.d /opt/gow /usr/local/bin /usr/bin /opt/direwolf /opt/xodus-nexus /usr/share/xodus-nexus' \
  && find $d ! -type l -perm /022 -exec chmod go-w {} + && test -z "$(find $d ! -type l -perm /022)"

# Sessions of one Battle.net game (XERK-2363): launch-battlenet starts the
# client the Library installed, in its prefix in the shared home, launches the
# game, and stops the client when the game exits. Battle.net's 32-bit client
# needs the i386 freetype, fontconfig and ldap libraries heroic already has.
FROM game-session AS battlenet
COPY --from=proton-cachyos /opt/proton-cachyos /opt/proton-cachyos
COPY --chmod=755 root/opt/direwolf/launch-battlenet /opt/direwolf/launch-battlenet
COPY --chmod=644 root/opt/direwolf/battlenet-pad-repair.py /opt/direwolf/battlenet-pad-repair.py
COPY --chmod=755 root/opt/gow/battlenet-startup-app.sh /opt/gow/startup-app.sh
RUN for lib in libfreetype.so.6 libfontconfig.so.1 libldap.so.2; do \
    ldconfig -p | grep -q "${lib} (libc6)" || { echo "no i386 ${lib}" >&2; exit 1; }; \
  done \
  && command -v pgrep killall paste python3 swaymsg >/dev/null \
  && d='/etc/cont-init.d /opt/gow /usr/local/bin /usr/bin /opt/direwolf /opt/proton-cachyos' \
  && find $d ! -type l -perm /022 -exec chmod go-w {} + && test -z "$(find $d ! -type l -perm /022)"

# PC VR sessions (epic XERK-2656; spike report XERK-2662, .claude/rules/vr.md):
# the steam image plus the WiVRn server (OpenXR runtime, streams to a Quest
# running the WiVRn client) and two OpenVR-on-OpenXR layers, xrizer and
# OpenComposite; DIREWOLF_VR_OPENVR picks one per App. Files: vr/.
#
# WiVRn is built here from the xerktech/WiVRn fork, not upstream, a distro or
# flatpak: the fork authenticates the stream (AES-GCM, XERK-2691) and has a
# long-term server key the headset pins (XERK-2699). Its socket code differs
# from upstream's, so the Quest runs the fork's APK, built from this same
# commit (a protocol mismatch is refused): bump both together. Built on the
# steam image itself so it links the libraries the session has.
FROM steam AS vr-wivrn
ARG WIVRN_REPO=https://github.com/xerktech/WiVRn
# The fork's master at the commit built. Its CMakeLists pins every
# FetchContent dependency by URL_HASH, and monado by commit (monado-rev).
ARG WIVRN_COMMIT=f1a4ef00354c0be51fc2f3584a567a5f770d1974
RUN apt-get update && apt-get install -y --no-install-recommends \
      build-essential cmake ninja-build git pkg-config ca-certificates python3 \
      glslang-tools spirv-tools gettext libssl-dev \
      libavahi-client-dev libavahi-glib-dev libavcodec-dev libavfilter-dev libavutil-dev \
      libswscale-dev libdrm-dev libbsd-dev libcap-dev libcjson-dev libcli11-dev libeigen3-dev \
      libgl-dev libglx-dev libgl1-mesa-dev libglu1-mesa-dev mesa-common-dev libnotify-dev \
      libpipewire-0.3-dev libsystemd-dev libudev-dev libva-dev libvulkan-dev libwayland-dev \
      libx11-xcb-dev libx264-dev libxcb-randr0-dev libxrandr-dev libxxf86vm-dev \
      nlohmann-json3-dev libglib2.0-dev libfreetype-dev libusb-1.0-0-dev \
      librsvg2-dev libarchive-dev libpng-dev bsdextrautils
# PipeWire on: it is WiVRn's only audio backend, and the image ships the daemon
# (vr-services starts it). No dashboard, no client, no Lighthouse/SolarXR
# drivers, no VA-API (NVIDIA only).
RUN git init -q /src/wivrn \
 && git -C /src/wivrn fetch -q --depth 1 "${WIVRN_REPO}" "${WIVRN_COMMIT}" \
 && git -C /src/wivrn checkout -q FETCH_HEAD \
 && test "$(git -C /src/wivrn rev-parse HEAD)" = "${WIVRN_COMMIT}" \
 && cmake -S /src/wivrn -B /src/wivrn/build -GNinja -DCMAKE_BUILD_TYPE=RelWithDebInfo \
      -DCMAKE_INSTALL_PREFIX=/usr/local -DWIVRN_BUILD_CLIENT=OFF -DWIVRN_BUILD_DASHBOARD=OFF \
      -DWIVRN_USE_SYSTEM_BOOST=OFF -DWIVRN_USE_SYSTEM_OPENXR=OFF \
      -DWIVRN_USE_NVENC=ON -DWIVRN_USE_VAAPI=OFF -DWIVRN_USE_X264=ON -DWIVRN_USE_PIPEWIRE=ON \
      -DWIVRN_FEATURE_STEAMVR_LIGHTHOUSE=OFF -DWIVRN_FEATURE_SOLARXR=OFF \
      -DWIVRN_OPENXR_MANIFEST_TYPE=absolute \
      -DGIT_DESC="xerktech-${WIVRN_COMMIT}" -DGIT_COMMIT="${WIVRN_COMMIT}" \
 && cmake --build /src/wivrn/build \
 && DESTDIR=/out cmake --install /src/wivrn/build \
 && test -x /out/usr/local/bin/wivrn-server \
 && test -f /out/usr/local/share/openxr/1/openxr_wivrn.json

# OpenComposite (gitlab.com/znixian/OpenOVR), at a commit: its submodules are
# pinned by that commit's gitlinks.
FROM steam AS vr-opencomposite
ARG OPENCOMPOSITE_COMMIT=cff07db75c4823afe93ed7027b03d5f7bc86f164
RUN apt-get update && apt-get install -y --no-install-recommends \
      build-essential cmake ninja-build git pkg-config ca-certificates python3 \
      libvulkan-dev libgl-dev libglx-dev libx11-dev libxrandr-dev libxxf86vm-dev libwayland-dev
RUN git clone https://gitlab.com/znixian/OpenOVR.git /src/oc \
 && git -C /src/oc checkout --detach "${OPENCOMPOSITE_COMMIT}" \
 && test "$(git -C /src/oc rev-parse HEAD)" = "${OPENCOMPOSITE_COMMIT}" \
 && git -C /src/oc submodule update --init --recursive \
 && cmake -S /src/oc -B /src/oc/build -GNinja -DCMAKE_BUILD_TYPE=Release \
 && cmake --build /src/oc/build \
 && install -D -m 0755 "$(find /src/oc/build -name vrclient.so | head -1)" \
      /out/opt/opencomposite/bin/linux64/vrclient.so

# xrizer (github.com/Supreeeme/xrizer), built from its release tag's commit
# with fenrir's patches in vr/xrizer/ (XERK-2847: a Quest recenter after an
# in-game one applied twice). Its Cargo.lock pins every crate (--locked).
# static-openxr links the OpenXR loader in, as the release zip does.
# Bookworm keeps the glibc floor below the steam image's.
FROM rust:1.89-slim-bookworm@sha256:d7fc7de78bb8c1469933aeecbf801314d30d7d6e9f0578bba4cfa285bfa37fe6 AS vr-xrizer
ARG XRIZER_COMMIT=a1216c7b53fb562a5bc4de473163e97a483042af
ARG XRIZER_VERSION=v0.6-direwolf1
# The openxr-sys loader build's cmake would default to make, which is not installed.
ENV CMAKE_GENERATOR=Ninja
RUN apt-get update && apt-get install -y --no-install-recommends \
      git ca-certificates cmake ninja-build clang libclang-dev g++ glslc \
      libvulkan-dev libx11-xcb-dev libgl-dev
RUN git clone https://github.com/Supreeeme/xrizer.git /src/xrizer \
 && git -C /src/xrizer checkout --detach "${XRIZER_COMMIT}" \
 && test "$(git -C /src/xrizer rev-parse HEAD)" = "${XRIZER_COMMIT}"
COPY vr/xrizer/ /src/patches/
# The patches' own tests run first: a patch that no longer does its job fails
# the build rather than shipping.
RUN set -eu; cd /src/xrizer; \
  git apply --index /src/patches/*.patch; \
  export XRIZER_VERSION; \
  cargo test --release --locked --features static-openxr --lib openxr_data::tests; \
  cargo build --release --locked --features static-openxr; \
  install -D -m 0755 target/release/libxrizer.so /out/opt/xrizer/bin/linux64/vrclient.so; \
  printf '%s\n' "${XRIZER_VERSION}" >/out/opt/xrizer/bin/version.txt; \
  chmod 0644 /out/opt/xrizer/bin/version.txt

# Selawik (OFL), Assetto Corsa's segoeui*.ttf stand-in (vr/ac-setup), from
# its release zip, checked against its SHA256 (update both together).
FROM debian:trixie-slim@sha256:a29215f6a35e51e22adffa17f89e9d2ef06214e64a2bad10d765c46aea49f11f AS vr-fonts
ARG SELAWIK_VERSION=1.01
ARG SELAWIK_SHA256=3f62c51e05e3b5a1e6241cf92a371f0be2ea1183aa87b30718bbd40832a8d423
ADD --checksum=sha256:${SELAWIK_SHA256} \
  https://github.com/microsoft/Selawik/releases/download/${SELAWIK_VERSION}/Selawik_Release.zip /tmp/selawik.zip
RUN set -eu; \
  apt-get update; apt-get install -y --no-install-recommends unzip; \
  mkdir -p /out/opt/direwolf/vr/fonts; \
  unzip -q -j /tmp/selawik.zip 'selawk*.ttf' -d /out/opt/direwolf/vr/fonts; \
  test "$(ls /out/opt/direwolf/vr/fonts | wc -l)" = 5; \
  chmod 0644 /out/opt/direwolf/vr/fonts/*

FROM steam AS vr
# PipeWire, its pulse server and WirePlumber run per session (vr-services):
# WiVRn 26.9 segfaults on every headset connect without PipeWire's config and
# modules, and the game's audio reaches the headset through them. iproute2's ss
# is the supervisor's port check. openssl makes WiVRn's identity key
# (startup-app.sh). libopengl0: OpenComposite's vrclient.so links
# libOpenGL.so.0, which the GoW base lacks.
RUN apt-get update && apt-get install -y --no-install-recommends \
      pipewire pipewire-pulse wireplumber iproute2 \
      libavahi-client3 libavahi-glib1 libavfilter10 libswscale8 libbsd0 libcap2 libcjson1 \
      libnotify4 libpipewire-0.3-0t64 libsystemd0 libudev1 libva2 libvulkan1 libx264-164 \
      libxxf86vm1 libusb-1.0-0 librsvg2-2 libarchive13t64 libpng16-16t64 \
      libopengl0 cabextract openssl \
 && rm -rf /var/lib/apt/lists/*
COPY --from=vr-wivrn /out/ /
COPY --from=vr-opencomposite /out/ /
COPY --from=vr-xrizer /out/ /
COPY --from=vr-fonts /out/ /
# Assetto Corsa's setup (vr/ac-setup, XERK-2726): winetricks at a release,
# checked against its SHA256 (update both together); cabextract is its
# corefonts unpacker.
ARG WINETRICKS_VERSION=20260125
ARG WINETRICKS_SHA256=431f82fc74000e6c864409f1d8fb495d696c03928808e3e8acffc45179312a7b
ADD --chmod=755 --checksum=sha256:${WINETRICKS_SHA256} \
  https://raw.githubusercontent.com/Winetricks/winetricks/${WINETRICKS_VERSION}/src/winetricks /opt/direwolf/vr/winetricks
# AC's shared Documents link leads to this User's stateMount. Nothing of the
# image's sits under the stateMounts root: the runtime follows links at a
# mount point (XERK-2713).
RUN ln -sfnT /srv/direwolf/state/ac-documents /srv/direwolf/ac-documents \
 && test ! -e /srv/direwolf/state
# The session starts the VR services, then Steam as before: GoW's startup-app
# moves beside the vr scripts (startup-app.sh finds its siblings by its own
# path) and ours takes its place.
COPY --chmod=644 vr/dbus-session.conf vr/dbus-session-agent.conf vr/ac-csp-versions.json /opt/direwolf/vr/
COPY --chmod=755 vr/startup-app.sh vr/vr-services vr/vr-notice vr/ac-setup /opt/direwolf/vr/
# GoW's Steam startup sets WINEDLLOVERRIDES outright, so an App's own (CSP's
# dwrite=n,b for Assetto Corsa) never reached the game: it now appends
# DIREWOLF_WINEDLLOVERRIDES. The grep fails the build if a GoW bump moves it.
RUN chmod 0755 /opt/direwolf /opt/direwolf/vr \
 && mv /opt/gow/startup-app.sh /opt/direwolf/vr/steam-startup-app.sh \
 && grep -qx 'export WINEDLLOVERRIDES=dxgi=n' /opt/direwolf/vr/steam-startup-app.sh \
 && sed -i 's|^export WINEDLLOVERRIDES=dxgi=n$|export WINEDLLOVERRIDES="dxgi=n${DIREWOLF_WINEDLLOVERRIDES:+;$DIREWOLF_WINEDLLOVERRIDES}"|' /opt/direwolf/vr/steam-startup-app.sh \
 && ln -s /opt/direwolf/vr/startup-app.sh /opt/gow/startup-app.sh
# No setcap anywhere (with file caps gdbus ignores DBUS_SESSION_BUS_ADDRESS),
# and nothing may link-fail at run time.
RUN ldconfig \
 && for f in /usr/local/bin/wivrn-server /usr/local/lib/wivrn/libopenxr_wivrn.so \
      /opt/xrizer/bin/linux64/vrclient.so /opt/opencomposite/bin/linux64/vrclient.so; do \
    ldd "$f" >/tmp/ldd || exit 1; ! grep 'not found' /tmp/ldd || { echo "$f: missing libraries" >&2; exit 1; }; \
  done && rm /tmp/ldd \
 && python3 -I -c 'import os,sys; bad=[os.path.join(d,f) for r in ("/usr/local","/opt") for d,_,fs in os.walk(r) for f in fs if not os.path.islink(os.path.join(d,f)) and "security.capability" in os.listxattr(os.path.join(d,f))]; bad and sys.exit("file capabilities on %s" % bad)' \
 && for c in pipewire pipewire-pulse wireplumber dbus-run-session ss mountpoint setsid swaymsg swaynag pkill cabextract curl openssl; do \
    command -v "$c" >/dev/null || { echo "no $c" >&2; exit 1; }; \
  done \
 && test -d /usr/share/pipewire && test -f /usr/share/pipewire/client.conf \
 && gosu nobody sh -c 'test -r /usr/local/share/openxr/1/openxr_wivrn.json -a -r /opt/xrizer/bin/linux64/vrclient.so -a -r /opt/opencomposite/bin/linux64/vrclient.so' \
 && d='/etc/cont-init.d /opt/gow /usr/local /usr/bin /opt/direwolf /opt/xrizer /opt/opencomposite' \
 && find $d ! -type l -perm /022 -exec chmod go-w {} + && test -z "$(find $d ! -type l -perm /022)"
