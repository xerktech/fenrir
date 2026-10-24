#!/bin/bash
# Run by the games-on-whales entrypoint once the session's compositor
# environment is set up. A catalogue session (pkg/controllers/catalogue.go)
# sets $DIREWOLF_LAUNCH_COMMAND to `xodus-nexus --launch-game <id>`; without
# it, the Nexus launcher opens.
set -e

source /opt/gow/launch-comp.sh
# Through a script: sway's exec line would re-split the command's quoting.
launcher /opt/direwolf/launch-nexus
