#!/bin/bash
# Prints the 12-hex id of a patch series: SHA-256 of its *.patch files in name
# order. CI names each DLL release after it and the Library recomputes it from
# its own copy, so an image only ever installs a DLL built from its patches.
# The Library's xodus-nexus-game-patches inlines the same computation.
set -euo pipefail
dir="${1:?patch directory}"
shopt -s nullglob
patches=("${dir}"/*.patch)
((${#patches[@]})) || { echo "no patches in ${dir}" >&2; exit 1; }
cat "${patches[@]}" | sha256sum | cut -c1-12
