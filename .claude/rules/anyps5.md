---
paths:
  - images/anyps5/**
  - renovate.json
  - .github/workflows/builder.yml
  - .github/workflows/build.yml
---

# AnyPS5 session image (`images/anyps5`, XERK-2616)

- AnyPS5 relinks a dumped PS5 executable into a native Linux program; its system libraries are
  native shared libraries. The image ships its release assets, never a source build.
- Base is GoW's Fedora 44 `base-app:edge-fedora`, not the Ubuntu 25.04 one other session images
  use: the release libraries need glibc 2.43 (`readelf -V libs/*.prx`). Recheck on every bump.
  - The Fedora base has no `pactl`; the image installs `pulseaudio-utils` for the App command.
  - The relinker is static; the libraries dlopen Vulkan, SDL's X11/Wayland/libdecor, PulseAudio.
- Dumps must be decrypted ELF (`eboot.bin` starts `\x7fELF`); `launch-ps5` refuses a SELF.
- Conversions are cached per AnyPS5 version in `~/.cache/anyps5/<version>/<title>`; saves live in
  `~/.local/share/anyps5/<title>` (the game's cwd), so a bump never touches them.
  - The title ID names directories that get deleted: keep its `^[A-Z]{4}[0-9]{5}$` check.
- `ps5-session` kills sway however the game exits: GoW's launcher appends `&& killall sway`, so
  a crashed game (AnyPS5 exits on anything unsupported) would leave an empty session running.
- Renovate on xerktech/fenrir runs from ArgoCD's self-hosted workflow (`RENOVATE_REPOSITORIES`).
  - `renovate.json` disables every package by default; only `boykopovar/AnyPS5` is enabled and
    automerged. Removing the default-off rule opens a PR per Go module, action and base image.
  - Two `# renovate:` blocks (relinker, libraries) share one depName and group into one PR;
    the Dockerfile fails the build if their versions differ.
- builder.yml tags the image `anyps5:<ANYPS5_VERSION>` as well as `main` (matrix `version_arg`).
  ArgoCD pins `anyps5:<version>@sha256:` and its Renovate automerges both tag and digest moves.
- Tests: `images/anyps5/launch_ps5_test.go` (stub relinker; real scripts).
