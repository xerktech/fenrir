package v1alpha1

import (
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type App struct {
	metav1.TypeMeta `json:",inline"`
	// Standard object metadata; More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Spec AppSpec `json:"spec"`
}

// AppSpec is an App's spec. vrPort needs requireAppPod: the VR port is a
// hostPort on the App pod, off the host network; in the host-networked
// session pod a VR server would listen on the node's own network.
// +kubebuilder:validation:XValidation:rule="!has(self.vrPort) || !self.vrPort || (has(self.requireAppPod) && self.requireAppPod)",message="vrPort needs requireAppPod"
// +kubebuilder:validation:XValidation:rule="!has(self.stateMounts) || size(self.stateMounts) == 0 || has(self.volumeClaimTemplate)",message="stateMounts needs volumeClaimTemplate"
type AppSpec struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// Name of the app to be presented to the user
	Title string `json:"title" xml:"AppTitle" toml:"title"`

	// Globally unique ID of the application. If there is a collision, the app
	// will be excluded from the list of available apps.
	ID int `json:"id" xml:"ID"`

	// +kubebuilder:validation:Required
	// Whether the app supports HDR
	IsHDRSupported bool `json:"isHDRSupported" xml:"IsHdrSupported"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Format=byte
	// PNG image of the app
	AppAssetWebP []byte `json:"appAssetWebP" xml:"-"`

	// +kubebuilder:validation:Optional
	// The operator copies this metadata onto the session pod, so a key the API
	// server rejects there would fail every Session of the App: reject it here.
	// Label values get a schema pattern from hack/update-codegen.sh instead:
	// CEL over unbounded map values exceeds the CRD cost budget.
	// Plain regexes, not CEL's format library, which needs a newer API server
	// than the chart's kubeVersion floor.
	// +kubebuilder:validation:XValidation:rule="!has(self.metadata) || !has(self.metadata.labels) || self.metadata.labels.all(k, k.indexOf('/') <= 253 && k.matches(r'^([a-z\\d]+((-+|[.])[a-z\\d]+)*/)?[^\\W_]([-\\w.]{0,61}[^\\W_])?$'))",message="metadata.labels keys must be qualified names: an optional DNS subdomain prefix and '/', then at most 63 alphanumerics, '-', '_' or '.', starting and ending alphanumeric"
	// +kubebuilder:validation:XValidation:rule="!has(self.metadata) || !has(self.metadata.annotations) || self.metadata.annotations.all(k, k.indexOf('/') <= 253 && k.lowerAscii().matches(r'^([a-z\\d]+((-+|[.])[a-z\\d]+)*/)?[^\\W_]([-\\w.]{0,61}[^\\W_])?$'))",message="metadata.annotations keys must be qualified names: an optional DNS subdomain prefix and '/', then at most 63 alphanumerics, '-', '_' or '.', starting and ending alphanumeric"
	Template *v1.PodTemplateSpec `json:"template" xml:"-"`

	// Unstructured wolf configuration for app to be merged with the default
	// configuration
	// +kubebuilder:validation:Optional
	// +kubebuilder:pruning:PreserveUnknownFields
	WolfConfig WolfConfig `json:"wolfConfig" xml:"-"`

	// A template for a PersistentVolumeClaim to be created for the app's
	// home directory. If provided, the operator will create a PVC from
	// this template and mount it at /home/retro.
	// If not provided, an emptyDir volume will be used.
	// all other volumes must be defined in the pod template's spec.volumes field.
	// +kubebuilder:validation:Optional
	// Its metadata is validated as template's is; it is copied onto the PVC.
	// +kubebuilder:validation:XValidation:rule="!has(self.metadata) || !has(self.metadata.labels) || self.metadata.labels.all(k, k.indexOf('/') <= 253 && k.matches(r'^([a-z\\d]+((-+|[.])[a-z\\d]+)*/)?[^\\W_]([-\\w.]{0,61}[^\\W_])?$'))",message="metadata.labels keys must be qualified names: an optional DNS subdomain prefix and '/', then at most 63 alphanumerics, '-', '_' or '.', starting and ending alphanumeric"
	// +kubebuilder:validation:XValidation:rule="!has(self.metadata) || !has(self.metadata.annotations) || self.metadata.annotations.all(k, k.indexOf('/') <= 253 && k.lowerAscii().matches(r'^([a-z\\d]+((-+|[.])[a-z\\d]+)*/)?[^\\W_]([-\\w.]{0,61}[^\\W_])?$'))",message="metadata.annotations keys must be qualified names: an optional DNS subdomain prefix and '/', then at most 63 alphanumerics, '-', '_' or '.', starting and ending alphanumeric"
	VolumeClaimTemplate *v1.PersistentVolumeClaimTemplate `json:"volumeClaimTemplate,omitempty" xml:"-"`

	// Hidden Apps are left out of the Moonlight app list (e.g. the base Apps
	// the catalogue scanner copies). A per-game setting the scanner keeps.
	// +kubebuilder:validation:Optional
	Hidden bool `json:"hidden,omitempty" xml:"-"`

	// GPU the session pod gets through a DRA ResourceClaim the operator
	// creates per Session. Omitted: no GPU from this field (the pod template
	// may still reference its own claims). A per-game setting the scanner keeps.
	// +kubebuilder:validation:Optional
	GPU *AppGPU `json:"gpu,omitempty" xml:"-"`

	// RequireAppPod refuses Sessions unless the operator runs the App in its
	// own pod off the host network (--app-pod-host-dir). For an App whose
	// programs listen on 127.0.0.1 (Battle.net's client and Agent): in the
	// host-networked session pod that is the node's loopback, open to every
	// process on the node, and two Sessions on one node collide on fixed ports.
	// +kubebuilder:validation:Optional
	RequireAppPod bool `json:"requireAppPod,omitempty" xml:"-"`

	// VRPort gives each Session one host port, from the operator's
	// --vr-port-range, for a VR server (WiVRn) in the App pod: TCP and UDP
	// on the same number, declared as hostPorts on the App's first container
	// and handed to every App container as DIREWOLF_VR_PORT. A headset dials
	// it at the node's IP. Needs requireAppPod.
	// +kubebuilder:validation:Optional
	VRPort bool `json:"vrPort,omitempty" xml:"-"`

	// StateMounts are directories kept per User and App: each is the
	// subdirectory mounts/<name> of the volumeClaimTemplate's PVC, mounted at
	// /srv/direwolf/state/<name> in every App container. For an App whose
	// home is shared (the Steam home) but which needs some state of its own,
	// such as a VR server's pairing keys. The App can't mount that PVC itself:
	// it holds Wolf's data too. Needs volumeClaimTemplate.
	//
	// The path is fixed, never in a volume (XERK-2713): the runtime follows
	// links at a mount point, so one in a volume a game writes could put the
	// User's directory over /etc. The image links its own paths there.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=name
	StateMounts []StateMount `json:"stateMounts,omitempty" xml:"-"`
}

// StateMount is one of an App's per-User directories (AppSpec.StateMounts).
type StateMount struct {
	// Name picks the directory, mounts/<name>, on the PVC, and where it is
	// mounted, /srv/direwolf/state/<name>: renaming one starts it empty.
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`
}

// Positive means a nonzero digit before any suffix or exponent (the schema
// already checks quantity syntax). A regex, not CEL's quantity library
// (XERK-1521); both compile on the chart's 1.36 floor.

// +kubebuilder:validation:XValidation:rule="!has(self.memory) || string(self.memory).matches('^[+]?[0-9.]*[1-9]')",message="memory must be positive"

// AppGPU is the GPU share a session of the App gets.
type AppGPU struct {
	// VRAM to reserve on one card (the claim's capacity.requests.memory).
	// Omitted: the whole card.
	// +kubebuilder:validation:Optional
	Memory *resource.Quantity `json:"memory,omitempty"`

	// DRA DeviceClass to allocate from.
	// +kubebuilder:validation:Optional
	// +kubebuilder:default="gpu.nvidia.com"
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	DeviceClassName string `json:"deviceClassName,omitempty"`
}

// TODO so I can easily find it
// This entire implementation needs a rework
// It'll be modified later to better inject env vars and configs.
type RuntimeWolfVariables struct {
	// RenderNode specifies the filepath to the DRM render node device.
	// Wolf defaults to: "/dev/dri/renderD128"
	// +kubebuilder:validation:Optional
	// +kubebuilder:default="/dev/dri/renderD128"
	RenderNode string `json:"renderNode,omitempty"`

	// Time zone for the wolf container
	// +kubebuilder:validation:Optional
	// +kubebuilder:default=UTC
	TimeZone string `json:"timeZone,omitempty"`

	// Logging level for wolf.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Enum=ERROR;WARNING;INFO;DEBUG;TRACE
	// +kubebuilder:default:="DEBUG"
	LogLevel string `json:"logLevel,omitempty"`
}

type WolfConfig struct {
	StartAudioServer       *bool `json:"startAudioServer,omitempty" toml:"start_audio_server,omitempty"`
	StartVirtualCompositor *bool `json:"startVirtualCompositor,omitempty" toml:"start_video_compositor,omitempty"`

	Title string `json:"title,omitempty" toml:"title,omitempty"`
	ID    string `json:"id,omitempty" toml:"id,omitempty"`

	Audio *WolfStreamConfig `json:"audio,omitempty" toml:"audio,omitempty"`
	Video *WolfStreamConfig `json:"video,omitempty" toml:"video,omitempty"`

	Runner *WolfRunnerConfig `json:"runner,omitempty" toml:"runner,omitempty"`

	// Additional wolf configs to use.
	// +kubebuilder:validation:Optional
	RuntimeVariables *RuntimeWolfVariables `json:"runtimeVariables,omitempty"`
}

type WolfStreamConfig struct {
	// +kubebuilder:validation:Optional
	Source string `json:"source,omitempty" toml:"source,omitempty"`

	// +kubebuilder:validation:Optional
	Sink string `json:"sink,omitempty" toml:"sink,omitempty"`
}

type WolfRunnerConfig struct {
	Type       string `json:"type,omitempty" toml:"type,omitempty"`
	RunCommand string `json:"runCommand,omitempty" toml:"run_cmd,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type AppList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []App `json:"items"`
}
