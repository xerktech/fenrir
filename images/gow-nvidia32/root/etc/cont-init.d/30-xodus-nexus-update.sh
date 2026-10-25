# Sourced as root by the GOW entrypoint (no `exit`), after 10-setup_user.sh.
# Installs the Xodus Nexus the Library last fetched and verified into the
# shared home, so a session runs the Library's version without downloading it
# at launch (xodus-nexus-update). Only the nexus target ships it.
if [ -x /usr/local/bin/xodus-nexus-update ]; then
    /usr/local/bin/xodus-nexus-update "${UNAME}" --cached-only ||
        gow_log "Could not update Xodus Nexus; keeping the image's copy"
fi
