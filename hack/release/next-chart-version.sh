#!/usr/bin/env bash
# Prints the next chart release version, MAJOR.MINOR.PATCH, from the repo's
# v<MAJOR>.<MINOR>.<PATCH> tags (builder.yml pushes one per published chart).
#   patch (every main merge): latest tag's PATCH + 1
#   minor / major (manual workflow_dispatch): bump it, lower parts reset to 0
# The tags are the only record, so nothing is committed and a release can't
# retrigger itself. The first release is 0.1.0 (1.0.0 for major).
# Needs the tags fetched (actions/checkout fetch-tags: true).
set -euo pipefail

kind="${1:-patch}"
case "$kind" in
  patch | minor | major) ;;
  *)
    echo "usage: $0 patch|minor|major" >&2
    exit 2
    ;;
esac

latest="$(git tag -l 'v*' |
  grep -E '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$' |
  sed 's/^v//' | sort -t. -k1,1n -k2,2n -k3,3n | tail -n1 || true)"

if [ -z "$latest" ]; then
  if [ "$kind" = major ]; then echo 1.0.0; else echo 0.1.0; fi
  exit 0
fi

IFS=. read -r major minor patch <<<"$latest"
case "$kind" in
  patch) echo "$major.$minor.$((patch + 1))" ;;
  minor) echo "$major.$((minor + 1)).0" ;;
  major) echo "$((major + 1)).0.0" ;;
esac
