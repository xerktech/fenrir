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

// LibraryPodName is the one Library pod per namespace. A fixed name makes
// concurrent visits converge on one Create (the rest get AlreadyExists), and
// lets the chart grant moonlight-proxy delete on this pod alone.
const LibraryPodName = "direwolf-library"

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
)
