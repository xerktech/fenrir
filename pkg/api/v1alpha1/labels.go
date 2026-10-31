package v1alpha1

// SessionPodLabel marks a pod as a Direwolf session worker. The operator sets
// it on every session pod, and the nri-input plugin grants input devices
// (/dev/input, /dev/hidraw, /dev/uinput) only to pods carrying it.
//
// The value is always SessionPodLabelValue; the operator's pod informer selects
// on it.
const SessionPodLabel = "direwolf/session"

// SessionPodLabelValue is the value the operator sets for SessionPodLabel.
const SessionPodLabelValue = "true"

// LibraryPodLabel marks the operator's on-demand Library pod (Steam + Heroic
// desktop). moonlight-proxy refuses launches while a pod carrying it exists:
// the Library and game sessions share one Steam home, which has one writer.
//
// The value is always LibraryPodLabelValue.
const LibraryPodLabel = "direwolf/library"

// LibraryPodLabelValue is the value the operator sets for LibraryPodLabel.
const LibraryPodLabelValue = "true"

// LibraryPodName is the shared library's Library pod. A fixed name makes
// concurrent visits converge on one Create (the rest get AlreadyExists).
const LibraryPodName = "direwolf-library"

// LibraryCloseAnnotation, on a Pairing, asks the operator to close the
// Library pod whose UID it holds ("Close Library" in Moonlight). The operator
// acts on it only if the Pairing's User plays from that pod's library.
// moonlight-proxy, which is internet-facing, may not delete pods itself.
const LibraryCloseAnnotation = "direwolf/library-close"

// LibraryOwnerLabel names the User whose own library (User.spec.library) a
// Library pod or catalogue App belongs to. Absent: the shared library.
const LibraryOwnerLabel = "direwolf/library-owner"

// LibraryPodNameFor is the Library pod of owner's library ("" for the shared
// one), fixed for the same reasons as LibraryPodName.
func LibraryPodNameFor(owner string) string {
	if owner == "" {
		return LibraryPodName
	}
	return LibraryPodName + "-" + owner
}

// LibraryOwnerOf is the library owner a Library pod or catalogue App carries.
func LibraryOwnerOf(labels map[string]string) string {
	return labels[LibraryOwnerLabel]
}

// libraryStores are the catalogue stores read from a Library: their Apps
// launch from that library's home and games, so only its owner may see them.
var libraryStores = map[string]bool{
	CatalogueStoreSteam:     true,
	CatalogueStoreEpic:      true,
	CatalogueStoreGOG:       true,
	CatalogueStoreNexus:     true,
	CatalogueStoreBattlenet: true,
}

// AppVisibleTo reports whether a User playing from owner's library ("" the
// shared one) may see and launch app. A game catalogued from a Library is
// installed in that library only; every other App is everyone's.
func AppVisibleTo(app *App, owner string) bool {
	if appOwner, ok := app.Labels[LibraryOwnerLabel]; ok {
		return appOwner == owner
	}
	return !libraryStores[app.Labels[CatalogueLabel]] || owner == ""
}

// CatalogueLabel marks an App the operator's catalogue scanner created from
// an installed game; its value is the store (CatalogueStoreSteam, ...). The
// scanner only ever updates or deletes Apps carrying it.
const CatalogueLabel = "direwolf/catalogue"

const (
	CatalogueStoreSteam = "steam"
	CatalogueStoreEpic  = "epic"
	CatalogueStoreGOG   = "gog"
	// CatalogueStoreRomM marks an App made from a RomM ROM entry (a RetroArch
	// session); synced from RomM's API, not the Library.
	CatalogueStoreRomM = "romm"
	// CatalogueStoreNexus marks an App made from a Microsoft Store (Xbox PC)
	// game installed with Xodus Nexus in the Library.
	CatalogueStoreNexus = "nexus"
	// CatalogueStoreBattlenet marks an App made from a Blizzard game
	// installed with Battle.net in the Library.
	CatalogueStoreBattlenet = "battlenet"
)
