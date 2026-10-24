---
paths:
  - images/**
  - examples/**
  - pkg/controllers/app_pod.go
  - pkg/controllers/library*.go
---

# Hand-testing an App or the Library on talos04

Pitfalls from driving a hand-made App and the Library by `kubectl exec` (XERK-2257 Phase 0 spike).

## Where things run

- With `--app-pod-host-dir` set (it is, on talos04), the game container is in the `<session>-app`
  pod (containers `app`, `udev-relay`), not the Wolf pod; `kubectl logs <session> -c app` errors.
- The session pod is deleted when the Session ends, and its logs go with it. To debug a container
  that exits early, have the App command tee its output to a file on a PVC first.
- A Session ends `disconnect-grace-period` (10m) after the Moonlight client leaves; a game you
  started by hand is gone with it.

## Environments differ

- Library pod (`direwolf-library`): Debian trixie, user `abc` (uid 1000), labwc on Wayland,
  `WAYLAND_DISPLAY=wayland-0`, Xwayland `DISPLAY=:0`, `XDG_RUNTIME_DIR=/config/.XDG`, home `/config`.
- GOW App images (heroic): Ubuntu, user `retro` (uid 1000), sway, `WAYLAND_DISPLAY=wayland-1`,
  `DISPLAY=:0`, `XDG_RUNTIME_DIR=/tmp/.X11-unix`. A package list from one does not install on the other.
- A hand-made App on the GOW heroic image must keep the heroic App's `/home/retro` mount of the
  home and the `home-owner` cont-init script; without them the image's entrypoint exits 1 about
  two seconds after the display comes up, and Moonlight reports "Connection Terminated".
- Root `kubectl exec` cannot read uid-1000 processes' `/proc/<pid>/environ`; read it via
  `s6-setuidgid abc` (Library) or `setpriv --reuid=1000` (GOW).
- Run Wine and Steam-adjacent tools as uid 1000, never root: Wine refuses a prefix it does not own,
  and root-created files in the shared home break the Library.

## exec mechanics

- Long `kubectl exec` streams drop with `websocket: close 1006` after about five minutes; start
  long work with `nohup … &` inside the pod and poll its log.
- `pgrep -f` / `pkill -f` match the exec'd `bash -c` command line itself: a pattern naming the
  target kills your own shell (exit 143). Use `pkill -x <name>` or a `[x]yz` bracket pattern.
- `kubectl cp` drops the execute bit; run copied scripts as `bash script.sh`.
- The first Library start after a new image digest pulls a multi-GB image: "Starting the Library…"
  for several minutes is a pull, not a hang (`kubectl describe pod direwolf-library`).
