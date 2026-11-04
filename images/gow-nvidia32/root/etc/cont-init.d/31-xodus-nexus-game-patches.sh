# Sourced as root by the GOW entrypoint (no `exit`), after
# 30-xodus-nexus-update.sh. Before the game starts, gives the listed games the
# patched xgameruntime.dll CI built for the shared home's active runtime
# (xodus-nexus-game-patches, XERK-2654): a runtime the Library just installed
# gets its DLL at the first session after CI publishes it, not only at the next
# Library start. It never updates the runtime; the Library does. Bounded, so a
# slow GitHub delays a launch by at most 90 s. Only the nexus target ships it.
if [ -x /usr/local/bin/xodus-nexus-game-patches ]; then
    timeout 90 gosu "${UNAME}" /usr/local/bin/xodus-nexus-game-patches ||
        gow_log "Could not refresh patched xgameruntime.dll; games keep the one they have"
fi
