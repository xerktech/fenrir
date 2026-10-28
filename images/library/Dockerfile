# Game library desktop: linuxserver's Selkies Steam image plus Heroic Games
# Launcher (Epic/GOG/Amazon), Xodus Nexus (Microsoft Store) and Battle.net.
# Served over Selkies behind Authentik via the operator, which sets PASSWORD/CUSTOM_USER
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
  # --appimage-extract makes its directories 0700 root: the desktop user
  # must read them, and nobody but root may write.
  chmod -R u=rwX,go=rX /tmp/squashfs-root; \
  test -z "$(find /tmp/squashfs-root ! -type l \( ! -perm -o=r -o -perm /022 \))"; \
  mv /tmp/squashfs-root /opt/xodus-nexus; \
  # xodus-nexus-update compares releases against this.
  echo "${NEXUS_VERSION#v}" >/opt/xodus-nexus/.direwolf-version

# Battle.net runs under this Proton (the spike's, XERK-2360) via `proton run`:
# root-owned in the image, so nothing the desktop runs can change it. Its
# release publishes a .sha512sum beside the archive; bump both together.
FROM debian:trixie-slim@sha256:a29215f6a35e51e22adffa17f89e9d2ef06214e64a2bad10d765c46aea49f11f AS battlenet
ARG PROTON_CACHYOS_VERSION=cachyos-11.0-20261005-slr
ARG PROTON_CACHYOS_SHA512=457bee6051ee671509e1cd6b865246df00685e508908cd0cf98c62fd311d0c781ce80fca3e9500e89fd7e48463fa26335806cdb1c38e528fd0e3e2b6b66c4469
# Blizzard's installer, fetched from its versioned URL and pinned by hash:
# Battle.net updates itself once installed, so this only has to install it.
# When Blizzard drops the version (old ones 404), bump both together.
ARG BATTLENET_SETUP_VERSION=1.0.68
ARG BATTLENET_SETUP_SHA256=f4643a2086c0cecb1ab68cc601bf38c1a0a6f7ef5914ad03375cc9449385bec6
RUN set -eu; \
  apt-get update; apt-get install -y --no-install-recommends ca-certificates curl xz-utils; \
  name="proton-${PROTON_CACHYOS_VERSION}-x86_64"; \
  curl -fsSL -o /tmp/proton.tar.xz \
    "https://github.com/CachyOS/proton-cachyos/releases/download/${PROTON_CACHYOS_VERSION}/${name}.tar.xz"; \
  echo "${PROTON_CACHYOS_SHA512}  /tmp/proton.tar.xz" | sha512sum -c -; \
  tar -xJf /tmp/proton.tar.xz --no-same-owner -C /tmp; \
  test -x "/tmp/${name}/proton"; \
  # The default prefix ships prebuilt: Proton writes into its own directory
  # only to build a missing one, which would fail here (read-only to abc).
  test -f "/tmp/${name}/files/share/default_pfx/system.reg"; \
  chmod -R u=rwX,go=rX "/tmp/${name}"; \
  mv "/tmp/${name}" /opt/proton-cachyos; \
  mkdir -p /opt/battlenet; \
  curl -fsSL -o /opt/battlenet/Battle.net-Setup.exe \
    "https://downloader.battle.net/download/installer/win/${BATTLENET_SETUP_VERSION}/Battle.net-Setup.exe"; \
  echo "${BATTLENET_SETUP_SHA256}  /opt/battlenet/Battle.net-Setup.exe" | sha256sum -c -; \
  chmod 0644 /opt/battlenet/Battle.net-Setup.exe

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
COPY --from=battlenet /opt/proton-cachyos /opt/proton-cachyos
COPY --from=battlenet /opt/battlenet /opt/battlenet

RUN \
  echo "**** install heroic ****" && \
  curl -fsSL -o /tmp/heroic.deb \
    "https://github.com/Heroic-Games-Launcher/HeroicGamesLauncher/releases/download/${HEROIC_VERSION}/Heroic-${HEROIC_VERSION#v}-linux-amd64.deb" && \
  echo "${HEROIC_SHA256}  /tmp/heroic.deb" | sha256sum -c - && \
  apt-get update && \
  apt-get install -y --no-install-recommends /tmp/heroic.deb && \
  # init-heroic-update and wow-addons-update need it; the base only brings it in.
  python3 -I -c 'import zipfile, fnmatch, hashlib, json' && \
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
  echo "**** install battle.net's 32-bit libraries ****" && \
  # Battle.net is 32-bit and runs on Proton's i386 side, which uses these from
  # the system (XERK-2360): without freetype/fontconfig every label is blank,
  # without ldap sign-in fails with 3018; the rest are Lutris's list.
  apt-get install -y --no-install-recommends \
    libfreetype6:i386 libfontconfig1:i386 libldap2:i386 libkrb5-3:i386 libgssapi-krb5-2:i386 \
    libsqlite3-0:i386 libgpg-error0:i386 libpulse0:i386 libdbus-1-3:i386 libxrandr2:i386 \
    libxcomposite1:i386 libxcursor1:i386 libcups2t64:i386 && \
  echo "**** keep battle.net's login window its own size ****" && \
  # The desktop's rc.xml is rebuilt from /defaults/labwc.xml at every start,
  # and its catch-all rule maximizes every window. Battle.net's fixed-size
  # login window then fights the resize forever and its page never renders
  # (XERK-2361 QA). Its class, steam_proton, is every Proton window's.
  sed -i 's|<windowRule identifier="\*"><action name="Maximize" /></windowRule>|&\n    <windowRule identifier="steam_proton" title="Battle.net Login"><action name="UnMaximize" /></windowRule>|' /defaults/labwc.xml && \
  grep -qF '<windowRule identifier="steam_proton" title="Battle.net Login"><action name="UnMaximize" /></windowRule>' /defaults/labwc.xml && \
  echo "**** add heroic, xodus nexus and battle.net to the openbox menus ****" && \
  for menu in /defaults/menu.xml /defaults/menu_wayland.xml; do \
    sed -i 's|</menu>|<item label="Heroic" icon="/usr/share/icons/hicolor/48x48/apps/heroic.png"><action name="Execute"><command>/usr/bin/wrapped-heroic</command></action></item>\n</menu>|' "$menu" && \
    grep -q wrapped-heroic "$menu" || exit 1; \
    sed -i 's|</menu>|<item label="Xodus Nexus" icon="/opt/xodus-nexus/.DirIcon"><action name="Execute"><command>/usr/bin/xodus-nexus</command></action></item>\n</menu>|' "$menu" && \
    grep -q '/usr/bin/xodus-nexus' "$menu" || exit 1; \
    sed -i 's|</menu>|<item label="Battle.net" icon="/usr/share/icons/Adwaita/scalable/devices/input-gaming.svg"><action name="Execute"><command>/usr/bin/battlenet</command></action></item>\n</menu>|' "$menu" && \
    grep -q '/usr/bin/battlenet' "$menu" || exit 1; \
  done && \
  # Battle.net's menu icon: the theme has no applications-games, so it showed none.
  test -f /usr/share/icons/Adwaita/scalable/devices/input-gaming.svg && \
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
