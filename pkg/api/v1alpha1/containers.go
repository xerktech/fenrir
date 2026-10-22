package v1alpha1

// Names of the containers the operator adds to every session pod. cmd/nri-input
// grants input devices by container name, so these are a contract between it
// and the operator: every other container in a session pod is the App's.
//
// Pod container names (init containers included) are unique, so an App can't
// claim one of these names.
const (
	// WolfContainerName is the only container that creates virtual input
	// devices, so it alone gets /dev/uinput.
	WolfContainerName = "wolf"
	// WolfAgentContainerName opens no device nodes: it mknods and chmods them
	// (through O_PATH fds), which the device cgroup doesn't gate.
	WolfAgentContainerName = "wolf-agent"
	// PulseAudioContainerName opens no device nodes.
	PulseAudioContainerName = "pulseaudio"
	// InitContainerName seeds Wolf's config; it opens no device nodes.
	InitContainerName = "init"
	// SteamRelinkContainerName repoints the Library's Steam links in a shared
	// Steam home; it opens no device nodes.
	SteamRelinkContainerName = "steam-relink"
	// InputBridgeContainerName publishes device nodes into a user-namespaced
	// App pod (pkg/inputbridge). It is privileged, so it needs no grant.
	InputBridgeContainerName = "input-bridge"
	// UdevRelayContainerName announces hotplugged devices in a user-namespaced
	// App pod (pkg/inputbridge); it opens no device nodes.
	UdevRelayContainerName = "udev-relay"
)
