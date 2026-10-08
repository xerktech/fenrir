# wolf-agent has its own Dockerfile because its image also carries the C shim
# session pods preload into Wolf (shim/wolf_loopback.c). Build from the repo root:
#   docker build -f cmd/wolf-agent/Dockerfile .

# The shim is loaded into the Wolf image, so it is built against an old glibc:
# symbols then resolve to versions every newer glibc still provides (a current
# toolchain binds dlsym@GLIBC_2.34). Same builder as cmd/nri-nvenc-fix.
FROM quay.io/pypa/manylinux_2_28_x86_64:2026.09.30-1@sha256:c2261579b9c2e5d45aa93312f73e2a302182e3e977b558581a1838d6fed3d8e6 AS shim
COPY cmd/wolf-agent/shim/wolf_loopback.c /src/
RUN mkdir -p /out \
 && gcc -O2 -Wall -Werror -shared -fPIC -o /out/libwolf_loopback.so /src/wolf_loopback.c -ldl

FROM golang:1.26 AS builder
ARG CGO_ENABLED=0
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o /app/wolf-agent ./cmd/wolf-agent

FROM alpine:3.24
WORKDIR /app
COPY --from=builder --chmod=755 /app/wolf-agent /app/wolf-agent
# Path is pkg/controllers' wolfLoopbackShimImagePath.
COPY --from=shim /out/libwolf_loopback.so /app/lib/libwolf_loopback.so
ENTRYPOINT ["/app/wolf-agent"]
