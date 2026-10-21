# Sourced as root by the GOW entrypoint (no `exit`), after 10-setup_user.sh.
# Turns Steam Input off for every installed game before Steam starts: Apps get
# no /dev/uinput, and with Steam Input on a game sees no controller (XERK-1953).
# Runs as the desktop user: the Steam home is theirs and user-writable. The
# entrypoint runs under `set -e`, and a failure must not end the Session.
if [ -d "${HOME}/.steam/steam/" ]; then
    gosu "${UNAME}" python3 -I /opt/direwolf/steam-input-off.py "${HOME}/.steam/steam" ||
        gow_log "Could not turn Steam Input off; games with it on will see no controller"
fi
# Heroic (in the Library, sharing this install) writes this flag when it
# cannot reach Steam to manage a shortcut. Steam would then serve its CEF
# remote debugger on 127.0.0.1:8080, which under hostNetwork is the node's
# loopback: any process on the node could drive this Steam client. GoW's
# Decky install (cut from this image, XERK-2128) wrote it into
# debian-installation, which may exist without ~/.steam/steam or not be its link.
gosu "${UNAME}" rm -f "${HOME}/.steam/steam/.cef-enable-remote-debugging" \
    "${HOME}/.steam/debian-installation/.cef-enable-remote-debugging" ||
    gow_log "Could not remove .cef-enable-remote-debugging; Steam's debugger is open on the node"
