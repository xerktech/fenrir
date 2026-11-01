#!/bin/bash
# Run by the games-on-whales entrypoint once the session's compositor
# environment is set up: runs the App's PS5 game ($PS5_TITLE_ID) under sway.
set -e

source /opt/gow/bash-lib/utils.sh
source /opt/gow/launch-comp.sh

gow_log "Starting PS5 title ${PS5_TITLE_ID:-(unset)} with AnyPS5 $(cat /opt/anyps5/VERSION)"
# GoW's launcher appends `&& killall sway`, so a game that crashed (AnyPS5 exits
# on anything it does not support) would leave sway, and the session, running.
# ps5-session stops sway however the game ends.
export SWAY_STOP_ON_APP_EXIT=no
launcher /opt/direwolf/ps5-session
