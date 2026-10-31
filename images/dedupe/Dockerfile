# fclones for the game-library dedupe CronJob (xerktech/ArgoCD, XERK-2500).
# Each User with their own library has their own games volume; `fclones group
# | fclones dedupe` makes identical files in them share extents by reflink
# (FIDEDUPERANGE, XFS on talos04), so a game two Users own takes its space
# once. A later patch to one copy un-shares only what changed.
# duperemove was rejected: it keys files by the block device's UUID, which an
# unprivileged pod can't read, and then scans nothing (seen on talos04).
FROM alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6

RUN apk add --no-cache fclones && fclones --version

ENTRYPOINT ["fclones"]
