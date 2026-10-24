# Game library desktop: linuxserver's Selkies Steam image plus Heroic Games
# Launcher (Epic/GOG/Amazon) and Xodus Nexus (Microsoft Store). Served over
# Selkies behind Authentik via the operator, which sets PASSWORD/CUSTOM_USER
# at runtime and adds them itself.
# Run it with /dev/shm as a memory-backed emptyDir: Steam's CEF and Heroic's
# Electron both fall over on the 64Mi container default.

# Xodus Nexus (Microsoft Store / Xbox PC games, github.com/domsvoboda/xodus-nexus),
# unpacked from its AppImage: pods have no FUSE to mount one. Run unpacked, it
# never self-updates (Nexus updates only a running AppImage), so the version
# moves only with this pin.
FROM debian:trixie-slim@sha256:a29215f6a35e51e22adffa17f89e9d2ef06214e64a2bad10d765c46aea49f11f AS nexus
# renovate: datasource=github-releases depName=domsvoboda/xodus-nexus
ARG NEXUS_VERSION=v0.3.11
# Must be updated by hand with the version (the release's .sha512sum asset): a
# Renovate bump without it fails the build closed.
ARG NEXUS_SHA512=0d6ef2d254aa26eb80565653c3b08d4b1c66e77ac0c62360fbb0725079a91cef209e33123fecd995783374fdc1d4c4af39595c3a529a6f1d31aaaadab0759c99
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
  chmod -R go-w /tmp/squashfs-root; \
  mv /tmp/squashfs-root /opt/xodus-nexus

FROM lscr.io/linuxserver/steam:latest@sha256:d1c9f2c6d522169f6dcede9b17076c0e31ddbc5f9c4a102562cecddfe2d4ecdb

# The baseline: init-heroic-update installs a newer release at pod start.
# Renovate bumps the SHA256 with the version (github-release-attachments
# hashes the new release's matching asset); a mismatch fails the build.
# renovate: datasource=github-release-attachments depName=Heroic-Games-Launcher/HeroicGamesLauncher
ARG HEROIC_VERSION=v2.22.3
ARG HEROIC_SHA256=f89eed7e0eb900fbe3051edfedbe7532db14328241cbf8c1fb766462dfc0b849

# First, so the RUN below can point Heroic's own launchers at the wrapper.
COPY --chmod=755 root/ /
COPY --from=nexus /opt/xodus-nexus /opt/xodus-nexus

RUN \
  echo "**** install heroic ****" && \
  curl -fsSL -o /tmp/heroic.deb \
    "https://github.com/Heroic-Games-Launcher/HeroicGamesLauncher/releases/download/${HEROIC_VERSION}/Heroic-${HEROIC_VERSION#v}-linux-amd64.deb" && \
  echo "${HEROIC_SHA256}  /tmp/heroic.deb" | sha256sum -c - && \
  apt-get update && \
  apt-get install -y --no-install-recommends /tmp/heroic.deb && \
  echo "**** route heroic through wrapped-heroic ****" && \
  # Also run by init-heroic-update after it installs a newer release.
  heroic-postinstall && \
  echo "**** route steam.desktop through wrapped-steam ****" && \
  # /usr/lib/steam/steam.desktop, which bin_steam.sh copies to ~/Desktop on a
  # fresh home, is a symlink to this file, so it is covered too.
  sed -i 's|^Exec=/usr/bin/steam|Exec=/usr/bin/wrapped-steam|' /usr/share/applications/steam.desktop && \
  ! grep -q '^Exec=/usr/bin/steam' /usr/share/applications/steam.desktop && \
  echo "**** install xodus nexus's keyring ****" && \
  # Nexus stores the Microsoft account in a Secret Service keyring; see xodus-nexus.
  apt-get install -y --no-install-recommends gnome-keyring libsecret-1-0 dbus && \
  echo "**** add heroic and xodus nexus to the openbox menus ****" && \
  for menu in /defaults/menu.xml /defaults/menu_wayland.xml; do \
    sed -i 's|</menu>|<item label="Heroic" icon="/usr/share/icons/hicolor/48x48/apps/heroic.png"><action name="Execute"><command>/usr/bin/wrapped-heroic</command></action></item>\n</menu>|' "$menu" && \
    grep -q wrapped-heroic "$menu" || exit 1; \
    sed -i 's|</menu>|<item label="Xodus Nexus" icon="/opt/xodus-nexus/.DirIcon"><action name="Execute"><command>/usr/bin/xodus-nexus</command></action></item>\n</menu>|' "$menu" && \
    grep -q '/usr/bin/xodus-nexus' "$menu" || exit 1; \
  done && \
  echo "**** resolve game sessions' Steam paths ****" && \
  # Sessions mount the shared Steam home at /home/retro, and Steam records
  # absolute paths (~/.steam/steam, library folders); see init-steam-home.
  mkdir -p /home && ln -s /config /home/retro && \
  echo "**** cleanup ****" && \
  apt-get autoclean && \
  rm -rf \
    /var/lib/apt/lists/* \
    /var/tmp/* \
    /tmp/*
