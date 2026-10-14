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
	// The rest open no device nodes: wolf-agent mknods and chmods them
	// (through O_PATH fds), which the device cgroup doesn't gate.
	WolfAgentContainerName    = "wolf-agent"
	PulseAudioContainerName   = "pulseaudio"
	InitContainerName         = "init"
	LoopbackShimContainerName = "wolf-loopback-shim"
)
