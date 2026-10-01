---
paths:
  - pkg/controllers/session*.go
  - pkg/controllers/catalogue*.go
  - pkg/controllers/library*.go
  - pkg/controllers/romm*.go
  - pkg/generic/controller*.go
---

# Objects from informers are the cache's own

- `SessionController` runs 2 workers, so two Sessions (often of the same App/User) reconcile
  at once. Anything read from an informer/lister is shared cache memory.
- Never write into it: `DeepCopy()` structs/ObjectMeta, `maps.Clone` / `slices.Clone` before
  adding to a map or appending to a slice taken from it.
- `append` onto a cached slice is a write too: JSON-decoded slices carry spare capacity
  (3 items decode as len 3, cap 4), so the append lands in the cache's backing array.
- Exception: the object a `generic.Controller` reconciler is handed is already a deep copy
  (`controller.reconcile`), so Reconcile may write its status. Keep that copy: without it a
  failed `UpdateStatus` leaves unpersisted status in the cache. Objects it then reads from
  listers (App, User, Pod, other Sessions) are still the cache's.
- That copy can lag a write that just landed, so `endSession` deletes with a resourceVersion
  precondition. A failed delete (`errEndSessionFailed`) must requeue without a status write:
  recorded as PodCreated=False, the next reconcile recreates a gone pod.
  Tests: `TestFailedEndSessionDoesNotRecreatePod`, `TestEndSessionPreconditionsResourceVersion`.
- Tests: `TestReconcilerGetsCopyOfCachedObject` (pkg/generic/controller_test.go).
- Tests: `TestBuildPodLeavesCachedObjects` (session_test.go) — run it under `-race`.
