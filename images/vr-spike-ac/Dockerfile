# XERK-2660 VR spike (scratch, not for main): the XERK-2659 WiVRn spike image plus
# the tools a hand-driven Assetto Corsa + Content Manager + CSP test needs:
# winetricks (CM's .NET in the Proton prefix), xdotool/grim/wtype (drive and screenshot
# the headless sway desktop from kubectl exec), unzip/7z/cabextract (CM, CSP), and
# OpenComposite as the OpenVR fallback to xrizer (xrizer put AC's seat far too low).
ARG BASE=ghcr.io/xerktech/fenrir/vr-spike:xerk-2659@sha256:a669e5eb568c9608c1a13451d45c035b8d05cf51214b3b5bb9a822edf88844eb

FROM ${BASE} AS oc
USER root
ARG OC_COMMIT=cff07db75c4823afe93ed7027b03d5f7bc86f164
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      build-essential cmake ninja-build git pkg-config ca-certificates python3 \
      libvulkan-dev libgl-dev libglx-dev libx11-dev libxrandr-dev libxxf86vm-dev libwayland-dev
RUN git clone https://gitlab.com/znixian/OpenOVR.git /src/oc \
 && git -C /src/oc checkout "${OC_COMMIT}" \
 && git -C /src/oc submodule update --init --recursive \
 && cmake -S /src/oc -B /src/oc/build -GNinja -DCMAKE_BUILD_TYPE=Release \
 && cmake --build /src/oc/build \
 && find /src/oc/build -name 'vrclient*.so' \
 && install -D "$(find /src/oc/build -name vrclient.so | head -1)" /out/opt/opencomposite/bin/linux64/vrclient.so

FROM ${BASE}
USER root
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      winetricks cabextract unzip p7zip-full xdotool x11-utils grim wtype procps psmisc curl ca-certificates \
      zenity wget \
 && rm -rf /var/lib/apt/lists/*
COPY --from=oc /out/ /
