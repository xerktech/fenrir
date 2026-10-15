# Sourced as root by the GOW entrypoint (no `exit`), after 10-setup_user.sh.
# Turns Steam Input off for every installed game before Steam starts: Apps get
# no /dev/uinput, and with Steam Input on a game sees no controller (XERK-1953).
# Runs as the desktop user: the Steam home is theirs and user-writable. The
# entrypoint runs under `set -e`, and a failure must not end the Session.
if [ -d "${HOME}/.steam/steam/" ]; then
    gosu "${UNAME}" python3 -I /opt/direwolf/steam-input-off.py "${HOME}/.steam/steam" ||
        gow_log "Could not turn Steam Input off; games with it on will see no controller"
fi
