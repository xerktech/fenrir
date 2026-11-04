# XERK-2659 VR spike: the live steam session image plus a WiVRn server, xrizer
# and hello_xr, for a hand-written test pod on talos04. Not built by CI; it
# feeds the real VR image ticket. Build context: this directory.

ARG BASE=ghcr.io/xerktech/fenrir/steam:main@sha256:12914845b23370b2ad8800e26d3c6b8c55fb364a77c2c73221838d7aba69122a

FROM ${BASE} AS build
ARG WIVRN_VERSION=v26.9
ARG OPENXR_SDK_VERSION=release-1.1.58
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      build-essential cmake ninja-build git pkg-config ca-certificates curl xz-utils python3 \
      glslang-tools spirv-tools gettext libssl-dev \
      libavahi-client-dev libavahi-glib-dev libavcodec-dev libavfilter-dev libavutil-dev \
      libswscale-dev libdrm-dev libbsd-dev libcap-dev libcjson-dev libcli11-dev libeigen3-dev \
      libgl-dev libglx-dev libgl1-mesa-dev libglu1-mesa-dev mesa-common-dev libnotify-dev \
      libpipewire-0.3-dev libsystemd-dev libudev-dev libva-dev libvulkan-dev libwayland-dev \
      libx11-xcb-dev libx264-dev libxcb-randr0-dev libxrandr-dev libxxf86vm-dev \
      nlohmann-json3-dev libglib2.0-dev libfreetype-dev libusb-1.0-0-dev \
      librsvg2-dev libarchive-dev libpng-dev bsdextrautils
RUN git clone --depth 1 -b "${WIVRN_VERSION}" https://github.com/WiVRn/WiVRn /src/wivrn \
 && cmake -S /src/wivrn -B /src/wivrn/build -GNinja -DCMAKE_BUILD_TYPE=RelWithDebInfo \
      -DCMAKE_INSTALL_PREFIX=/usr/local -DWIVRN_BUILD_CLIENT=OFF -DWIVRN_BUILD_DASHBOARD=OFF \
      -DWIVRN_USE_SYSTEM_BOOST=OFF -DWIVRN_USE_SYSTEM_OPENXR=OFF \
      -DWIVRN_USE_NVENC=ON -DWIVRN_USE_VAAPI=OFF -DWIVRN_USE_X264=ON -DWIVRN_USE_PIPEWIRE=ON \
      -DWIVRN_FEATURE_STEAMVR_LIGHTHOUSE=OFF -DWIVRN_FEATURE_SOLARXR=OFF \
      -DWIVRN_OPENXR_MANIFEST_TYPE=absolute \
 && cmake --build /src/wivrn/build \
 && DESTDIR=/out cmake --install /src/wivrn/build
# hello_xr (and the OpenXR loader it links) for the "basic OpenXR sample".
RUN git clone --depth 1 -b "${OPENXR_SDK_VERSION}" https://github.com/KhronosGroup/OpenXR-SDK-Source /src/openxr \
 && cmake -S /src/openxr -B /src/openxr/build -GNinja -DCMAKE_BUILD_TYPE=Release \
      -DCMAKE_INSTALL_PREFIX=/usr/local -DBUILD_TESTS=ON -DBUILD_API_LAYERS=OFF \
      -DBUILD_WITH_WAYLAND_HEADERS=OFF \
 && cmake --build /src/openxr/build --target hello_xr openxr_loader \
 && install -D /src/openxr/build/src/tests/hello_xr/hello_xr /out/usr/local/bin/hello_xr \
 && cp -a /src/openxr/build/src/loader/libopenxr_loader.so* /out/usr/local/lib/
# xrizer: OpenVR on top of OpenXR, for SteamVR titles later (not used by hello_xr).
ARG XRIZER_VERSION=v0.6
ARG XRIZER_SHA256=9b2105433e48a20f789e20a14113dbd98650afd116bd232419725a4438dd2149
RUN curl -fsSLo /tmp/xrizer.zip "https://github.com/Supreeeme/xrizer/releases/download/${XRIZER_VERSION}/xrizer-${XRIZER_VERSION}.zip" \
 && echo "${XRIZER_SHA256}  /tmp/xrizer.zip" | sha256sum -c - \
 && python3 -c 'import zipfile,sys; zipfile.ZipFile(sys.argv[1]).extractall(sys.argv[2])' /tmp/xrizer.zip /tmp/xrizer \
 && mkdir -p /out/opt && mv "/tmp/xrizer/xrizer-${XRIZER_VERSION}" /out/opt/xrizer

FROM ${BASE}
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      libavahi-client3 libavahi-glib1 libavfilter10 libswscale8 libbsd0 libcap2 libcjson1 \
      libnotify4 libpipewire-0.3-0t64 libsystemd0 libudev1 libva2 libvulkan1 libx264-164 \
      libxxf86vm1 libusb-1.0-0 librsvg2-2 libarchive13t64 libpng16-16t64 iproute2 \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/ /
RUN ldconfig \
 && ! ldd /usr/local/bin/wivrn-server /usr/local/bin/hello_xr /usr/local/lib/wivrn/libopenxr_wivrn.so | grep 'not found'
