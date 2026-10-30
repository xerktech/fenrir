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
ARG NEXUS_VERSION=v0.4.0
# Must be updated by hand with the version (the release's .sha512sum asset): a
# Renovate bump without it fails the build closed.
ARG NEXUS_SHA512=55b4ff16b1ec312b5ba96ee701dade439a4a500483f674fdb87e65b0af812688859e14c288d8b4eedb0ec048fdeb583ed083a25140fb104ad0df4633db270716
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

# Ferestre (github.com/icex/ferestre), an alternative Xbox GDK launcher on trial
# against Xodus Nexus (XERK-2471). Its release tarball, not the AppImage: pods
# have no FUSE, and the tarball is the same tree unpacked.
FROM debian:trixie-slim@sha256:a29215f6a35e51e22adffa17f89e9d2ef06214e64a2bad10d765c46aea49f11f AS ferestre-app
# renovate: datasource=github-releases depName=icex/ferestre
ARG FERESTRE_VERSION=v0.1.14
# Must be updated by hand with the version (the tarball's line in the release's
# SHA256SUMS): a Renovate bump without it fails the build closed.
ARG FERESTRE_SHA256=587bd05941bd27752ae11ebedb19e6478cde45254d03fb45d9cc63ff0d9892bd
RUN set -eu; \
  apt-get update; apt-get install -y --no-install-recommends ca-certificates curl zstd; \
  v="${FERESTRE_VERSION#v}"; \
  curl -fsSL -o /tmp/ferestre.tar.zst \
    "https://github.com/icex/ferestre/releases/download/${FERESTRE_VERSION}/ferestre-${v}-x86_64.tar.zst"; \
  echo "${FERESTRE_SHA256}  /tmp/ferestre.tar.zst" | sha256sum -c -; \
  mkdir /tmp/x; tar -I zstd -xf /tmp/ferestre.tar.zst -C /tmp/x; \
  test -x "/tmp/x/ferestre-${v}/usr/bin/ferestre" && test -x "/tmp/x/ferestre-${v}/usr/bin/ferestre-gui"; \
  chmod -R u=rwX,go=rX "/tmp/x/ferestre-${v}"; \
  mv "/tmp/x/ferestre-${v}" /opt/ferestre

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
COPY --chmod=644 root/opt/xodus-nexus.hold /opt/xodus-nexus.hold
RUN test -x /opt/xodus-nexus/AppRun && command -v gnome-keyring-daemon dbus-run-session dbus-send >/dev/null \
  && command -v python3 curl sha256sum gosu timeout >/dev/null \
  && d='/etc/cont-init.d /opt/gow /usr/local/bin /usr/bin /opt/direwolf /opt/xodus-nexus' \
  && find $d ! -type l -perm /022 -exec chmod go-w {} + && test -z "$(find $d ! -type l -perm /022)"

# Trial sessions of Ferestre (XERK-2471): the nexus session image plus
# Ferestre, whose Xbox client links WebKitGTK 4.1 for sign-in (the only library
# the nexus image lacks). Its App sets $DIREWOLF_LAUNCH_COMMAND to ferestre-gui;
# Ferestre installs its own runtime into the home, so no Nexus runtime is used.
FROM nexus AS ferestre
RUN apt-get update \
  && apt-get install -y --no-install-recommends libwebkit2gtk-4.1-0 \
  && rm -rf /var/lib/apt/lists/*
COPY --from=ferestre-app /opt/ferestre /opt/ferestre
RUN ln -s /opt/ferestre/usr/bin/ferestre /usr/bin/ferestre \
  && ln -s /opt/ferestre/usr/bin/ferestre-gui /usr/bin/ferestre-gui \
  && ldconfig -p | grep -q libwebkit2gtk-4.1.so.0 \
  && ! ldd /opt/ferestre/usr/bin/ferestre-gui /opt/ferestre/usr/lib/ferestre/client/xodus-cli | grep 'not found' \
  && d=/opt/ferestre && find $d ! -type l -perm /022 -exec chmod go-w {} + && test -z "$(find $d ! -type l -perm /022)"

# Sessions of one Battle.net game (XERK-2363): launch-battlenet starts the
# client the Library installed, in its prefix in the shared home, launches the
# game, and stops the client when the game exits. Battle.net's 32-bit client
# needs the i386 freetype, fontconfig and ldap libraries heroic already has.
FROM game-session AS battlenet
COPY --from=proton-cachyos /opt/proton-cachyos /opt/proton-cachyos
COPY --chmod=755 root/opt/direwolf/launch-battlenet /opt/direwolf/launch-battlenet
COPY --chmod=755 root/opt/gow/battlenet-startup-app.sh /opt/gow/startup-app.sh
RUN for lib in libfreetype.so.6 libfontconfig.so.1 libldap.so.2; do \
    ldconfig -p | grep -q "${lib} (libc6)" || { echo "no i386 ${lib}" >&2; exit 1; }; \
  done \
  && command -v pgrep killall paste python3 swaymsg >/dev/null \
  && d='/etc/cont-init.d /opt/gow /usr/local/bin /usr/bin /opt/direwolf /opt/proton-cachyos' \
  && find $d ! -type l -perm /022 -exec chmod go-w {} + && test -z "$(find $d ! -type l -perm /022)"
