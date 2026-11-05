#!/bin/bash
# The vr session's startup (images/gow-nvidia32 target vr; .claude/rules/vr.md).
# GoW's startup.sh execs it as the desktop user in place of Steam's startup-app,
# which the image keeps beside it as steam-startup-app.sh. It checks what the
# operator and the App must hand it, writes WiVRn's config, starts the VR
# services (vr-services), then Steam with the game's VR and audio environment.
# The session ends when either Steam or the VR services end.
set -euo pipefail

here=$(dirname "$(readlink -f "$0")")

die() {
  echo "direwolf-vr: $*" >&2
  exit 1
}

# The port is the session's, from its port block (XERK-2664): never a default,
# or two sessions on the node would fight over WiVRn's 9757.
port=${DIREWOLF_VR_PORT:-}
if ! [[ $port =~ ^[1-9][0-9]{0,4}$ ]] || ((port > 65535)); then
  die "DIREWOLF_VR_PORT must be this session's VR port (1-65535), got '${port}'; refusing to start"
fi

# The OpenVR layer games load: both ran Assetto Corsa (XERK-2660).
case ${DIREWOLF_VR_OPENVR:-xrizer} in
xrizer) layer=/opt/xrizer ;;
opencomposite) layer=/opt/opencomposite ;;
*) die "DIREWOLF_VR_OPENVR must be xrizer or opencomposite, got '${DIREWOLF_VR_OPENVR}'" ;;
esac

[[ -n ${XDG_RUNTIME_DIR:-} && -d $XDG_RUNTIME_DIR ]] || die "XDG_RUNTIME_DIR is not a directory"

# WiVRn keeps its pairing keys in ~/.config/wivrn and rewrites
# ~/.config/openxr/1/active_runtime.json and ~/.config/openvr/openvrpaths.vrpath
# while it runs. In the shared Steam home every Proton game would then find a VR
# runtime, and all users' headsets would share one key list: each must be a
# per-user volume the App mounts there, writable.
for d in wivrn openxr openvr; do
  dir=$HOME/.config/$d
  # mountpoint follows a link: one into the shared home would pass.
  [[ ! -L $dir ]] || die "$dir is a symlink; it must be a per-user volume mounted by the App"
  mountpoint -q "$dir" || die "$dir must be a per-user volume mounted by the App, not the shared home"
  [[ -w $dir ]] || die "$dir is not writable"
done

# WiVRn's config, rewritten whole at every start. No user keys are kept: WiVRn
# reads its keys in one try block, so one of the wrong type (any process of the
# session can write the file) would drop the port and leave it on 9757.
# "publish-service": "none": no Avahi (it needs the node's system bus); the
# headset dials the node's IP and port. "hid-forwarding": false: it needs the
# uinput device, which App pods never get. No "application": WiVRn would start
# it on every headset connect, beside the game Steam runs.
config=$HOME/.config/wivrn/config.json
rm -f "$config.tmp"
cat >"$config.tmp" <<JSON
{
  "port": $port,
  "publish-service": "none",
  "hid-forwarding": false,
  "openvr-compat-path": "$layer",
  "encoder": "nvenc"
}
JSON
# -T: a directory named config.json is an error, not a place to move it into.
mv -fT "$config.tmp" "$config"

# PipeWire's sockets (and its pulse server's) go in a directory of their own:
# Wolf's pulseaudio already serves this runtime dir.
audio=$XDG_RUNTIME_DIR/vr-audio
mkdir -p "$audio"
chmod 0700 "$audio"

# Its own session bus (WiVRn needs one), listening in this container's /tmp.
dbus-run-session --config-file="$here/dbus-session.conf" -- "$here/vr-services" "$port" "$audio" &
services=$!
app=
# Stops the direct children when this exits. In the image it is PID 1, so its
# exit ends the container, grandchildren included.
trap 'kill $services $app 2>/dev/null || true' EXIT

timeout=${DIREWOLF_VR_START_TIMEOUT:-60}
# Steam starts after both: a game finds no runtime without the active runtime
# WiVRn writes, and no sound without the pulse server.
for what in pulse runtime; do
  for ((i = 0; ; i++)); do
    case $what in
    pulse) [[ -S $audio/pulse/native ]] && break ;;
    runtime) [[ -e $HOME/.config/openxr/1/active_runtime.json ]] && break ;;
    esac
    kill -0 "$services" 2>/dev/null || die "the VR services exited before the $what was up"
    ((i < timeout)) || die "no $what after ${timeout}s"
    sleep 1
  done
done

# Game audio goes to the headset through PipeWire's pulse server.
export PULSE_SERVER=unix:$audio/pulse/native
# Pressure-vessel hides /usr/local and /opt: it imports the OpenXR runtime the
# active runtime names, and needs the OpenVR layer's directory let in.
export PRESSURE_VESSEL_IMPORT_OPENXR_1_RUNTIMES=1
export PRESSURE_VESSEL_FILESYSTEMS_RO=$layer${PRESSURE_VESSEL_FILESYSTEMS_RO:+:$PRESSURE_VESSEL_FILESYSTEMS_RO}

"$here/steam-startup-app.sh" "$@" &
app=$!

status=0
wait -n "$services" "$app" || status=$?
if ! kill -0 "$services" 2>/dev/null; then
  echo "direwolf-vr: the VR services exited (status $status); ending the session" >&2
  ((status != 0)) || status=1
fi
exit "$status"
