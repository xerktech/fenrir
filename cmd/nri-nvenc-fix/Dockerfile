# nri-nvenc-fix has its own Dockerfile because it ships a C shim beside the Go
# binary. Build from the repo root:
#   docker build -f cmd/nri-nvenc-fix/Dockerfile .

# The shim is loaded into other people's images, so it is built against an old
# glibc: symbols then resolve to versions every newer glibc still provides. A
# current toolchain binds dlsym@GLIBC_2.34 and sscanf to __isoc23_sscanf@GLIBC_2.38,
# and the preload fails in any image older than that. manylinux_2_28 (glibc 2.28,
# gcc preinstalled) is the PyPA image maintained for exactly this.
FROM quay.io/pypa/manylinux_2_28_x86_64:2026.09.30-1@sha256:c2261579b9c2e5d45aa93312f73e2a302182e3e977b558581a1838d6fed3d8e6 AS shim
COPY cmd/nri-nvenc-fix/shim/nvenc_fix.c /src/
RUN gcc -O2 -Wall -shared -fPIC -o /src/libnvenc_fix.so /src/nvenc_fix.c -ldl -lpthread

FROM golang:1.26 AS builder
ARG CGO_ENABLED=0
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o /app/nri-nvenc-fix ./cmd/nri-nvenc-fix

FROM alpine:3.24
WORKDIR /app
COPY --from=builder --chmod=755 /app/nri-nvenc-fix /app/nri-nvenc-fix
COPY --from=shim /src/libnvenc_fix.so /app/libnvenc_fix.so
ENTRYPOINT ["/app/nri-nvenc-fix"]
