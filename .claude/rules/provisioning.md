---
paths:
  - pkg/controllers/provisioning*.go
  - pkg/controllers/library_server.go
  - pkg/api/v1alpha1/username*.go
  - pkg/moonlight/pinpage*.go
  - cmd/operator/**
  - charts/**
---

# User provisioning (XERK-2537 epic; core XERK-2538)

- `Provisioner.Ensure(username)` creates a group member's User with `spec.library` on two new PVCs,
  `direwolf-home-<name>` / `direwolf-games-<name>`. Off unless `--provision-group` (chart
  `provisioning.group`); it needs `--library-port`. The Library page's call is XERK-2540.
- Never modify an existing User, with or without `spec.library`: hand-written Users stay
  authoritative. Ensure returns them as they are.
- Never delete anything, and give the PVCs no ownerReference: deleting or re-creating a User must
  never garbage-collect a games volume. Deprovisioning is an admin's call.
- Claims are created before the User, so a User never points at missing claims; claims left by a
  failed attempt carry the username annotation and the next Ensure adopts them.
- Before creating anything, Ensure checks the User live (the cache lags) and both claims: a
  refused username, or an existing User, must leave no claim behind.
- Every Create tolerates AlreadyExists (two first requests race) by reading the live object and
  accepting it only if it was made for the same username.
- Username → User name is `v1alpha1.UserNameFor`: ASCII letters lowercased, nothing else rewritten,
  then a DNS-1123 label of at most 40 characters (the CRD's spec.library limit) or refused.
  - Never use `strings.ToLower`: it folds `İ` and the Kelvin sign into ASCII.
  - Every username → User lookup (`libraryOf`, the pairing page) goes through
    `v1alpha1.LookupUser`, so they agree with the provisioner.
  - A username `UserNameFor` refuses is still looked up by its exact name: hand-written Users
    may carry any name the API server accepts (`first.last`, over 40 characters).
- Collisions are refused, never shared: a provisioned User or claim carries
  `direwolf/username` (the raw username) and matches only that username; a User without it matches
  only its exact name, so "Sam" never gets hand-written "sam".
- Ensure runs `libraryClaimsConflict` on the generated claims first, so it never creates a User
  whose Library would refuse to start.
- Chart values reach the operator as env (`PROVISION_*`, `LIBRARY_GROUPS_*`), which the flags
  default from, as `LIBRARY_WOW_ADDONS` does. CI renders them set and sets
  `DIREWOLF_CHART_PROVISIONING=1`, so `charts/provisioning_test.go` fails, not skips, on a lost value.
- The user template is parsed strictly (`ParseProvisionUserTemplate`): a typo'd field would
  silently drop a limit. It may not set `library`, and must hold one non-empty YAML document:
  sigs.k8s.io/yaml reads only the first, and only a full parser sees a `...` end marker.
- The pairing page (moonlight-proxy) never creates Users: it is internet-facing. A
  `--pin-provision-group` member with no User gets a page sending them to `--pin-library-url`
  (XERK-2541); chart `provisioning.*` feeds both binaries, so the groups agree.
  - Its groups header is read only after the peer and proxy-secret checks, as the user header is.
  - "Not set up yet" only for a name `Ensure` would provision (`UserNameFor` accepts it, no User
    holds it); else the Library refuses it too and the user would loop between the two pages.
  - Tests: `TestPinPageUnprovisionedGroupMember`, `TestProvisioningValuesReachPairingPage`.
- Tests: `pkg/controllers/provisioning_test.go`, `pkg/api/v1alpha1/username_test.go`,
  `TestLibraryServerMapsUsernames`.
