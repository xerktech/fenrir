#!/usr/bin/env bash
# Prints the next chart release version, MAJOR.MINOR.PATCH, from the repo's
# v<MAJOR>.<MINOR>.<PATCH> tags (builder.yml pushes one per published chart).
#   patch (every main merge): latest tag's PATCH + 1
#   minor / major (manual workflow_dispatch): bump it, lower parts reset to 0
# The tags are the only record, so nothing is committed and a release can't
# retrigger itself. The first release is 0.1.0 (1.0.0 for major).
#
# Given the commit to release, it also refuses one the latest release is not an
# ancestor of: ArgoCD deploys the highest version, so re-running an old build
# would roll production back under a newer number. And a patch of an already
# released commit, which would ship nothing new.
# Needs the full history and tags (actions/checkout fetch-depth: 0, fetch-tags).
set -euo pipefail

kind="${1:-patch}"
commit="${2:-}"
case "$kind" in
  patch | minor | major) ;;
  *)
    echo "usage: $0 patch|minor|major [commit]" >&2
    exit 2
    ;;
esac

# Fail closed: outside a repo, or if git fails, there is no "first release".
tags="$(git tag -l 'v*')"
# At most 9 digits a part, so the arithmetic below can't overflow.
part='(0|[1-9][0-9]{0,8})'
latest="$({ grep -E "^v$part\.$part\.$part\$" <<<"$tags" || [ $? -eq 1 ]; } |
  sed 's/^v//' | sort -t. -k1,1n -k2,2n -k3,3n | tail -n1)"

if [ -z "$latest" ]; then
  if [ "$kind" = major ]; then echo 1.0.0; else echo 0.1.0; fi
  exit 0
fi

if [ -n "$commit" ]; then
  if ! git merge-base --is-ancestor "v$latest" "$commit"; then
    echo "$commit does not contain v$latest: refusing to release older code as a newer version" >&2
    exit 1
  fi
  if [ "$kind" = patch ] && [ "$(git rev-parse "v$latest^{commit}")" = "$(git rev-parse "$commit^{commit}")" ]; then
    echo "$commit is already released as v$latest" >&2
    exit 1
  fi
fi

IFS=. read -r major minor patch <<<"$latest"
case "$kind" in
  patch) echo "$major.$minor.$((patch + 1))" ;;
  minor) echo "$major.$((minor + 1)).0" ;;
  major) echo "$((major + 1)).0.0" ;;
esac
