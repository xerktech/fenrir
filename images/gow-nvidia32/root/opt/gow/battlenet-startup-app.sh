#!/bin/bash
# Run by the games-on-whales entrypoint once the session's compositor
# environment is set up. The App names the game in $BATTLENET_PRODUCT (see
# launch-battlenet); sway stops, and so the session ends, when it exits.
set -e

source /opt/gow/launch-comp.sh
# No arguments: sway's exec line is a shell line, so the product must not be
# spliced into it. launch-battlenet reads it from the environment.
launcher /opt/direwolf/launch-battlenet
