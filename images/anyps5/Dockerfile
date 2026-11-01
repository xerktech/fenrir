# PS5 games through AnyPS5 (github.com/boykopovar/AnyPS5, GPL-2.0): it relinks a
# dumped PS5 executable into a native Linux program and supplies the PS5 system
# libraries as native shared libraries, so the game runs as an ordinary process.
# This image is games-on-whales' base-app (Wolf's compositor setup) plus
# AnyPS5's released Linux relinker and libraries, fallback fonts, and
# /opt/direwolf/launch-ps5, which converts the session's game once per AnyPS5
# version and runs it.
#
# The base is GoW's Fedora 44 base-app, not the Ubuntu 25.04 one the other
# session images use: AnyPS5's release libraries need glibc 2.43 (25.04 has
# 2.41), and nothing in the Ubuntu one would load them.
FROM alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6 AS anyps5

# Renovate bumps both pairs in one PR (renovate.json groups boykopovar/AnyPS5)
# and recomputes each asset's sha256: the relinker and the libraries must come
# from the same release, which the build checks.
# renovate: datasource=github-release-attachments depName=boykopovar/AnyPS5
ARG ANYPS5_VERSION=v0.1.4
ARG ANYPS5_RELINKER_SHA256=9e5afd1313520906a9af43c468d6e31a5d2237879a85c0984134d628348e92a8
# renovate: datasource=github-release-attachments depName=boykopovar/AnyPS5
ARG ANYPS5_LIBS_VERSION=v0.1.4
ARG ANYPS5_LIBS_SHA256=805ba3f455924a9fc2ca9dae7b398f9c9d4e4b88b6251d738d64afed98f8e6ef
# The openly licensed (SIL OFL) substitutes AnyPS5 looks for when a game opens
# the console's system font sets; without them some games stop at start-up.
# fonts.sha256 lists each file's path in notofonts.github.io at this commit.
# Noto Sans CJK is left out (four ~20 MB+ collections): Japanese, Chinese and
# Korean text in system fonts shows no glyphs.
ARG NOTO_COMMIT=e2ec4a11da38a23b1ecebfcb233833a153ecb841

COPY fonts.sha256 /tmp/fonts.sha256
RUN set -eu && \
  apk add --no-cache curl && \
  if [ "${ANYPS5_VERSION}" != "${ANYPS5_LIBS_VERSION}" ]; then \
    echo "relinker ${ANYPS5_VERSION} and libraries ${ANYPS5_LIBS_VERSION} differ" >&2; exit 1; \
  fi && \
  base="https://github.com/boykopovar/AnyPS5/releases/download/${ANYPS5_VERSION}" && \
  curl -fsSL -o /tmp/relinker "${base}/relinker-${ANYPS5_VERSION}" && \
  curl -fsSL -o /tmp/libs.tar.gz "${base}/prx-linux-${ANYPS5_VERSION}.tar.gz" && \
  printf '%s  %s\n' "${ANYPS5_RELINKER_SHA256}" /tmp/relinker "${ANYPS5_LIBS_SHA256}" /tmp/libs.tar.gz | sha256sum -c - && \
  mkdir -p /out/anyps5 /out/fonts && \
  install -m 755 /tmp/relinker /out/anyps5/relinker && \
  tar -xzf /tmp/libs.tar.gz -C /out/anyps5 && \
  test -f /out/anyps5/libs/libSceAgcDriver.prx && \
  echo "${ANYPS5_VERSION}" > /out/anyps5/VERSION && \
  printf '%s\n' "AnyPS5 ${ANYPS5_VERSION}: https://github.com/boykopovar/AnyPS5/tree/${ANYPS5_VERSION} (GPL-2.0-only)" > /out/anyps5/SOURCE && \
  while read -r sum path; do \
    curl -fsSL -o "/out/fonts/${path##*/}" "https://raw.githubusercontent.com/notofonts/notofonts.github.io/${NOTO_COMMIT}/fonts/${path}" && \
    printf '%s  %s\n' "${sum}" "/out/fonts/${path##*/}" | sha256sum -c - || exit 1; \
  done < /tmp/fonts.sha256 && \
  test "$(ls /out/fonts | wc -l)" -eq "$(wc -l < /tmp/fonts.sha256)"

FROM ghcr.io/games-on-whales/base-app:edge-fedora@sha256:56e5dcaab59ec827d411e18c577be23044eebfcd62d1b20bdfaa1c6507663792

# pactl: the App's command picks Wolf's virtual sink with it, as every session
# App does. The Fedora base-app leaves it out.
RUN dnf5 install -y --setopt=install_weak_deps=False pulseaudio-utils && dnf5 clean all

COPY --from=anyps5 /out/anyps5/ /opt/anyps5/
COPY --from=anyps5 /out/fonts/ /usr/share/anyps5-fonts/
COPY --chmod=755 root/ /

# GoW's bases set UMASK=000, which leaves world-writable files in the home
# (XERK-2351); the session's converted games and saves live there.
ENV UMASK=022
