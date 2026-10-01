---
paths:
  - pkg/controllers/catalogue*.go
  - pkg/controllers/gpu_claim.go
  - pkg/controllers/library.go
  - pkg/controllers/testdata/catalogue/**
  - pkg/api/v1alpha1/app.go
  - pkg/moonlight/rest.go
  - pkg/moonlight/apps_test.go
  - cmd/operator/**
---

# Catalogue (App per installed game) and App.spec.gpu

- The scan execs `tar` in the Library pod; the operator never mounts the library.
  - The game library is talos04 local-path and the operator runs on any node.
  - Games only change while the Library runs, so it scans every `catalogueScanInterval` while
    the pod is ready, and once more in `stop()` after `steam -shutdown` (manifests flushed).
  - A scan error never holds the Library up or blocks its shutdown (it would hold the Steam lock).
- Fail closed on deletes: any exec, tar or parse error (store read mid-write) syncs nothing.
  - A launcher whose base App is missing is skipped: its Apps stay, never "uninstalled".
  - Only Apps labelled `direwolf/catalogue=<store>` are ever updated or deleted.
- Generated Apps are a copy of `--catalogue-steam-app` / `--catalogue-heroic-app`.
  - The base App's container must run `$DIREWOLF_LAUNCH_COMMAND` (a shell command line).
  - The base should be `hidden: true`; copies reset `hidden` to false on create.
  - `hidden` and `gpu` are per-game settings: set from the base on create, then the user's.
    Everything else (template, wolfConfig, volumes) follows the base on every scan.
- The Library's files are user-writable, so treat them as untrusted:
  - Heroic appNames must match `heroicAppName` (they go into a shell line and a URL).
  - Box art is fetched only over https from `artHosts`, every redirect re-checked, at most
    1 MiB, PNG/JPEG/WebP only. Anything else is SSRF whose reply lands in a readable App.
  - More than `catalogueMaxGames` games fails the scan.
- Moonlight IDs are an FNV hash in 2^30..2^31-1, clear of hand-written Apps' small IDs.
- moonlight-proxy serves `appAssetWebP` as WebP, PNG or JPEG (Steam's CDN is JPEG).
- A hidden App is neither listed nor launchable by ID.
- `App.spec.gpu` is a per-Session ResourceClaim `<session>-gpu` owned by the Session:
  `capacity.requests.memory` for a share, none for the whole card (NVIDIA DRA consumable shares).
  - Pods reference it by name, so a template's generated claims' GC doesn't cover it.
  - A claim still owned by an earlier same-named Session is waited out, never adopted.
  - Don't combine it with a template GPU claim: the pod would get two cards.
- Tests: `pkg/controllers/catalogue_test.go` (fixtures in `testdata/catalogue`, run through
  the real scan script); `TestHiddenAppsAreNotListed`, `TestAppAssetServesJPEGAsPNG` in
  `pkg/moonlight/apps_test.go`.
