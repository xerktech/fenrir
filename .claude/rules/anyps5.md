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
  use: the release libraries need glibc 2.43 (`readelf -V libs/*.prx`).
  - Bumps automerge, so the Dockerfile's `ldd` step is the only check that a release still loads
    on the base; keep it.
  - The Fedora base has no `pactl`; the image installs `pulseaudio-utils` for the App command.
  - The relinker is static; the libraries dlopen Vulkan, SDL's X11/Wayland/libdecor, PulseAudio.
- Dumps must be decrypted ELF (`eboot.bin` starts `\x7fELF`); `launch-ps5` refuses a SELF.
- AnyPS5 takes the working directory as the console's root (`/app0` is `./app0`; `data/` and
  saves are created beside it; v0.1.4 `General.cpp` `WorkingDirectory`, `AppMetadata.cpp`).
  - So the game runs in `~/.local/share/anyps5/<title>` with `app0` linked to the conversion in
    `~/.cache/anyps5/<version>/<title>`: a bump replaces the program and keeps the saves.
  - Never run it from the conversion dir: the saves would go with the next bump.
- The relinker refuses a `--module-dir` outside its input's directory and an input with no
  `sce_module`/`sce_modules`/`prx` beside it; a moduleless dump is relinked from a staged copy.
  The test stub enforces both, so keep it in step with the real relinker.
- Conversions of one title are serialised by `flock` on `<version>/<title>.lock` (every PS5
  session shares one home); the lock is closed before the game starts.
- The title ID and VERSION name directories that get deleted: keep both regexes (no `.`/`..`).
- `ps5-session` kills sway however the game exits: GoW's launcher appends `&& killall sway`, so
  a crashed game (AnyPS5 exits on anything unsupported) would leave an empty session running.
- Renovate on xerktech/fenrir runs from ArgoCD's self-hosted workflow (`RENOVATE_REPOSITORIES`).
  - `renovate.json` disables every package by default; only `boykopovar/AnyPS5` is enabled and
    automerged. The `main` ruleset requires a PR (0 approvals) and auto-merge is off, so
    Renovate merges a green PR itself on its next hourly run.
  - Removing the default-off rule opens a PR per Go module, action and base image.
  - Two `# renovate:` blocks (relinker, libraries) share one depName and group into one PR;
    the Dockerfile fails the build if their versions differ.
- builder.yml tags the image `anyps5:<ANYPS5_VERSION>` as well as `main` (matrix `version_arg`).
  ArgoCD pins `anyps5:<version>@sha256:` and its Renovate automerges both tag and digest moves.
  - Every fenrir merge rebuilds every image (new revision label), so each one also produces an
    automerged digest-only bump of `app-ps5.yaml`; that is expected, not a release.
- Tests: `images/anyps5/launch_ps5_test.go` (stub relinker; real scripts).
