# Game library desktop: linuxserver's Selkies Steam image plus Heroic Games
# Launcher (Epic/GOG/Amazon). Served over Selkies behind Authentik, so the
# built-in Selkies password (PASSWORD/CUSTOM_USER) is deliberately left unset.
# Run it with /dev/shm as a memory-backed emptyDir: Steam's CEF and Heroic's
# Electron both fall over on the 64Mi container default.
FROM lscr.io/linuxserver/steam:latest@sha256:d1c9f2c6d522169f6dcede9b17076c0e31ddbc5f9c4a102562cecddfe2d4ecdb

# renovate: datasource=github-releases depName=Heroic-Games-Launcher/HeroicGamesLauncher
ARG HEROIC_VERSION=v2.22.3

RUN \
  echo "**** install heroic ****" && \
  curl -fsSL -o /tmp/heroic.deb \
    "https://github.com/Heroic-Games-Launcher/HeroicGamesLauncher/releases/download/${HEROIC_VERSION}/Heroic-${HEROIC_VERSION#v}-linux-amd64.deb" && \
  apt-get update && \
  apt-get install -y --no-install-recommends /tmp/heroic.deb && \
  echo "**** add heroic to the openbox menus ****" && \
  for menu in /defaults/menu.xml /defaults/menu_wayland.xml; do \
    sed -i 's|</menu>|<item label="Heroic" icon="/usr/share/icons/hicolor/48x48/apps/heroic.png"><action name="Execute"><command>/usr/bin/wrapped-heroic</command></action></item>\n</menu>|' "$menu" && \
    grep -q wrapped-heroic "$menu" || exit 1; \
  done && \
  echo "**** cleanup ****" && \
  apt-get autoclean && \
  rm -rf \
    /var/lib/apt/lists/* \
    /var/tmp/* \
    /tmp/*

COPY --chmod=755 root/ /
