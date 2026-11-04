# XERK-2660 VR spike (scratch, not for main): the XERK-2659 WiVRn spike image plus
# the tools a hand-driven Assetto Corsa + Content Manager + CSP test needs:
# winetricks (CM's .NET in the Proton prefix), xdotool/grim (drive and screenshot
# the headless sway desktop from kubectl exec), unzip/7z/cabextract (CM, CSP).
FROM ghcr.io/xerktech/fenrir/vr-spike:xerk-2659@sha256:a669e5eb568c9608c1a13451d45c035b8d05cf51214b3b5bb9a822edf88844eb
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      winetricks cabextract unzip p7zip-full xdotool x11-utils grim wtype procps psmisc curl ca-certificates \
      zenity wget \
 && rm -rf /var/lib/apt/lists/*
