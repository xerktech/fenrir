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
  `direwolf-home-<name>` / `direwolf-games-<name>`. Off unless `--provision-group` or
  `--provision-allowed-users` (chart `provisioning.group` / `.allowedUsers`); it needs
  `--library-port`.
- The Library page calls it for an eligible name with no User (XERK-2540). Eligible = in the
  group OR on the allowlist (`Provisioner.Eligible`, XERK-2542); the allowlist is for proxies
  with no groups header. Its names match the user header exactly (no case folding).
  - The groups header is read only after the peer and proxy-secret checks; the proxy must
    overwrite it, as it does the username header.
  - Authentik's proxy outpost does overwrite it (`headers.Set`, 2026.8.5), so it sends one line
    (XERK-2581); every line is still read (`Header.Values`) for oauth2-proxy's one per group.
  - It lists direct memberships only: the `profile` scope mapping sends `user.groups.all()`, not
    `all_groups()`. A child group's member passes Authentik's group binding but is not
    provisioned. Add members to the provision group itself; never nest groups under it.
  - The chart joins `allowedUsers` with commas and `fail`s on a name containing one.
  - Only a user-gesture visit (`userNavigation`) or the Start POST creates: a tab's script
    reload after an admin deleted the User must not re-create it (it gets the stopped page).
    So the start path's method/site check runs before provisioning.
  - The new User reaches the informer cache seconds later; until then each request reads it live
    (no writes). Only a User carrying `ProvisionedLabel` then skips the gesture rule, so the setup
    page's own refresh starts its Library; an uncached hand-written User does not.
  - Ensure runs under `libraryProvisionTimeout`; a failure gets a refreshing error page, a
    refused username a 403 asking for the admin. Tests: `library_provision_test.go`.
- Header names come from the chart's `proxyAuth` block (Authentik defaults), shared by the
  operator (`LIBRARY_USER_HEADER`, `LIBRARY_GROUPS_*`) and moonlight-proxy (`PIN_USER_HEADER`,
  `PIN_GROUPS_*`).
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
- Chart values reach the operator as env (`PROVISION_*`, `LIBRARY_*_HEADER`,
  `LIBRARY_GROUPS_SEPARATOR`), which the flags default from, as `LIBRARY_WOW_ADDONS` does. CI renders them set and sets
  `DIREWOLF_CHART_PROVISIONING=1`, so `charts/provisioning_test.go` fails, not skips, on a lost value.
- The user template is parsed strictly (`ParseProvisionUserTemplate`): a typo'd field would
  silently drop a limit. It may not set `library`, and must hold one non-empty YAML document:
  sigs.k8s.io/yaml reads only the first, and only a full parser sees a `...` end marker.
- The pairing page (moonlight-proxy) never creates Users: it is internet-facing. Someone eligible
  (`--pin-provision-group` member or `--pin-provision-allowed-users`) with no User gets a page
  sending them to `--pin-library-url` (XERK-2541); chart `proxyAuth.*` and `provisioning.*` feed
  both binaries, so they agree. Its `eligible` must match `Provisioner.Eligible`.
- Groups are read from every header line (`Header.Values`): oauth2-proxy sends one per group.
  - Its groups header is read only after the peer and proxy-secret checks, as the user header is.
  - "Not set up yet" only for a name `Ensure` would provision (`UserNameFor` accepts it, no User
    holds it); else the Library refuses it too and the user would loop between the two pages.
  - It cannot see PVCs, so a name whose `direwolf-*-<name>` claim belongs to another username
    is still sent to the Library, which refuses it with "ask the admin". Accepted (XERK-2581):
    no loop, and moonlight-proxy holds no PVC read rights.
  - Tests: `TestPinPageUnprovisionedGroupMember`, `TestProvisioningValuesReachPairingPage`.
- Tests: `pkg/controllers/provisioning_test.go`, `pkg/api/v1alpha1/username_test.go`,
  `TestLibraryServerMapsUsernames`, `TestLibraryServerUserHeaderFromFlag`,
  `TestPinPageUserHeaderFromFlag`, `TestProvisionerOptionsAllowedUsers`.
