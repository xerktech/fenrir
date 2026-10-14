# Wolf for direwolf sessions: games-on-whales' Wolf image with its
# waylanddisplaysrc GStreamer plugin rebuilt with the fix on our fork
# (xerktech/gst-wayland-display, tag xerk-1740; XERK-1740). Unpatched, every stream's
# waylanddisplaysrc releases Wolf's process-wide cached CUDA context once more
# than it takes it; after a few streams the context is finalized under a live
# pipeline and the stream freezes.
# Drop this image (back to wolf:stable) once Wolf ships a gst-wayland-display
# with the fix. The fix commit sits on Wolf facb8e0's own pin (016b4fc), so the
# rest of the plugin is what the base image ships.
ARG WOLF_IMAGE=ghcr.io/games-on-whales/wolf:stable@sha256:b0ef253739f9bd49b71eba7e150c955fd05990c0d409e666d89236995351551a

# The base Wolf's own builder (docker/wolf.Dockerfile BASE_IMAGE), so the
# plugin links the GStreamer it runs against.
FROM ghcr.io/games-on-whales/gstreamer:1.26.7@sha256:512b53b03b256a87fb4bd204ed1d91219148b0b77ff4ee4fc121c37d869f8d29 AS plugin

# Tag xerk-1740 on xerktech/gst-wayland-display: 016b4fc + the fix. The tag
# keeps the commit fetchable whatever happens to its branch.
ARG GWD_REPO=https://github.com/xerktech/gst-wayland-display
ARG GWD_COMMIT=58c55f7d9b9466c2d8b0b78a5d32bf6465fc2189
ARG RUST_VERSION=1.96.0
ENV DEBIAN_FRONTEND=noninteractive HOME=/root PATH=/root/.cargo/bin:$PATH
RUN apt-get update -y && \
    apt-get install -y --no-install-recommends \
    libicu76 libssl-dev curl ca-certificates pkg-config git build-essential clang \
    libwayland-dev libinput-dev libxkbcommon-dev libgbm-dev libudev-dev libdrm-dev \
    libglib2.0-dev libegl-dev libgles-dev libopengl-dev && \
    rm -rf /var/lib/apt/lists/*
RUN curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y --default-toolchain $RUST_VERSION && \
    cargo install cargo-c@0.10.23 --locked
# The plugin crate's install_subdir puts it in <libdir>/gstreamer-1.0/.
RUN git clone $GWD_REPO /tmp/gwd && \
    cd /tmp/gwd && git checkout $GWD_COMMIT && \
    cargo cinstall --release --features=cuda \
      --prefix=/out --libdir=/out && \
    test -f /out/gstreamer-1.0/libgstwaylanddisplaysrc.so

FROM $WOLF_IMAGE
# The base sets GST_PLUGIN_PATH to this directory.
COPY --from=plugin /out/gstreamer-1.0/libgstwaylanddisplaysrc.so /usr/local/lib/x86_64-linux-gnu/gstreamer-1.0/
