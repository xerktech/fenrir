package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Represents a Session CRD.
// +kubebuilder:object:root=true
// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:subresource:status
type Session struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec SessionSpec `json:"spec"`

	//+kubebuilder:validation:Optional
	//+optional
	Status SessionStatus `json:"status"`
}

// SessionSpec describes a session: a certain user playing a specific game with a specific
// client. This object is meant to live for the duration of the user's session.
//
// A session is created in response to a user's /launch request.
//
// It is meant to be deleted by the session controller once the agent has reported
// that it is ended.
//
// A user can have multiple sessions active at a time, and even multiple copies
// of the same "game" if the underlying persistentVolumeClass supports it multiple
// binding.
//
// A session is created when moonlight calls /launch to launch a game.
//
// A VR-mode Session (XERK-2667) has no Moonlight client: no Pairing, Wolf
// config or gateway. The Moonlight fields stay required for Moonlight mode.
// +kubebuilder:validation:XValidation:rule="has(self.mode) && self.mode == 'VR' ? !has(self.config) && !has(self.pairingReference) && !has(self.gateway) : has(self.config) && has(self.pairingReference) && has(self.gateway)",message="mode Moonlight needs config, pairingReference and gateway; mode VR takes none of them"
type SessionSpec struct {
	// Mode is how the user reaches the session. Moonlight (the default)
	// streams it through Wolf to a Moonlight client. VR runs only the App,
	// with no Wolf or wolf-agent, and a headset connects straight to the
	// App's VR server on status.ports.vr (the App needs spec.vrPort).
	// Fixed for the Session's life: its pod is built for one or the other.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Enum=Moonlight;VR
	// +kubebuilder:default=Moonlight
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="mode is immutable"
	Mode string `json:"mode,omitempty"`

	//+kubebuilder:validation:Required
	UserReference UserReference `json:"userReference"`

	//+kubebuilder:validation:Required
	GameReference GameReference `json:"gameReference"`

	// The Moonlight client's Pairing; required in Moonlight mode, absent in VR.
	//+kubebuilder:validation:Optional
	PairingReference PairingReference `json:"pairingReference,omitzero"`

	// The name of the Gateway used to access the moonlight server.
	// The gateway IP used for the stream session must be the same as the IP of
	// the moonlight server used to initiate the connection due to moonlight
	// protocol restrictions. Moonlight mode only.
	//+kubebuilder:validation:Optional
	GatewayReference GatewayReference `json:"gateway,omitzero"`

	// Wolf-specific config for the session. Moonlight mode only.
	//+kubebuilder:validation:Optional
	Config SessionInfo `json:"config,omitzero"`
}

// Session modes (SessionSpec.Mode).
const (
	SessionModeMoonlight = "Moonlight"
	SessionModeVR        = "VR"
)

// IsVR reports whether the Session is a native VR session (no Moonlight).
func (s *SessionSpec) IsVR() bool { return s.Mode == SessionModeVR }

const (
	// SessionHeadsetConnectedCondition is a VR-mode Session's headset: True
	// while one is connected to its VR server (WiVRn's HeadsetConnected).
	// Its lastTransitionTime is when the headset came or went; a False after
	// a True also sets status.disconnectedAt. Never set in Moonlight mode.
	SessionHeadsetConnectedCondition = "HeadsetConnected"

	// SessionStreamStartedCondition is the Session condition for its stream.
	// In VR mode it is True while the VR server streams to the headset
	// (WiVRn's SessionRunning).
	SessionStreamStartedCondition = "StreamStarted"
	// SessionStreamRefusedReason marks a SessionStreamStartedCondition that
	// is False for good: the operator will never start a stream for this
	// spec.config (e.g. a client IP Wolf cannot stream to), so waiting for
	// one is pointless. Any other False reason may still clear on a retry.
	// Its observedGeneration is the generation it was judged on.
	SessionStreamRefusedReason = "StreamRefused"
)

// Session State machine
// Pending -> Initializing -> WaitForPing -> Streaming -> Ended
type SessionStatus struct {
	// Represents the observations of a session's state.
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,1,rep,name=conditions"`

	// The host ports allocated to the session's pod. Freed when the Session
	// is deleted. A VR-mode Session has only ports.vr: with no Wolf it gets
	// no port block.
	Ports SessionPorts `json:"ports"`

	// The RTSP url to access the stream.
	WolfSessionID string `json:"wolfSessionID,omitempty"`
	StreamURL     string `json:"streamURL,omitempty"`

	// The metadata.generation whose spec.config the current (or last) Wolf
	// session was created from. /resume updates spec.config with the
	// client's new keys, so a generation above this one asks the operator to
	// re-attach the running pod.
	AttachedGeneration int64 `json:"attachedGeneration,omitempty"`

	// When the client's stream went away. The pod is kept for the operator's
	// disconnect grace period so /resume can re-attach, then the Session is
	// deleted. Cleared on re-attach. In VR mode: when the headset
	// disconnected; cleared when one connects again within the grace period.
	DisconnectedAt *metav1.Time `json:"disconnectedAt,omitempty"`

	// VRServerKey is the raw X448 public key (lowercase hex) of the identity
	// key the VR server of a VR-mode Session's pod serves, as the pod's
	// vr-agent first reported it (XERK-2808). The VR front door hands it to
	// the paired headset, which pins it rather than trusting the server on
	// first use. Set once; a later, different report is ignored.
	// +optional
	//+kubebuilder:validation:Pattern=`^[0-9a-f]{112}$`
	VRServerKey string `json:"vrServerKey,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type SessionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Session `json:"items"`
}

type SessionInfo struct {
	// The client's IP as moonlight-proxy saw it (zone stripped). Bounded so
	// the errors quoting it stay within a condition message's limit.
	//+kubebuilder:validation:Optional
	//+kubebuilder:validation:MaxLength=45
	ClientIP string `json:"clientIP,omitempty"`

	//+kubebuilder:validation:Required
	AESKey string `json:"aesKey,omitempty"`

	//+kubebuilder:validation:Required
	AESIV string `json:"aesIV,omitempty"`

	//+kubebuilder:validation:Required
	VideoWidth int `json:"videoWidth,omitempty"`

	//+kubebuilder:validation:Required
	VideoHeight int `json:"videoHeight,omitempty"`

	//+kubebuilder:validation:Required
	VideoRefreshRate int `json:"videoRefreshRate,omitempty"`

	//+kubebuilder:validation:Required
	SurroundAudioFlags int `json:"surroundAudioFlags"` // no omitempty: clients send 0, and a missing required field is refused (XERK-2709)

	// The client supports Sunshine's encrypted RTSP (it sent corever>=1 on
	// /launch or /resume), so Wolf is asked to encrypt RTSP with AESKey.
	// status.streamURL is rtspenc:// only once Wolf confirms it does.
	//+kubebuilder:validation:Optional
	EncryptedRTSP bool `json:"encryptedRTSP,omitempty"`
}

// SessionPorts is a session pod's block of host ports. Session pods use
// hostNetwork and share the node IP with moonlight-proxy
// (Moonlight can be redirected to another port, not another host), so each
// session pod gets its own contiguous block of host ports from the operator's
// configured range. Wolf is started with the matching WOLF_*_PORT variables.
type SessionPorts struct {
	// Wolf's own pairing server; bound but unused, moved off the defaults so
	// it does not collide with moonlight-proxy or another session.
	HTTP  int32 `json:"http,omitempty"`
	HTTPS int32 `json:"https,omitempty"`

	RTSP     int32 `json:"rtsp,omitempty"`
	Control  int32 `json:"control,omitempty"`
	VideoRTP int32 `json:"videoRTP,omitempty"`
	AudioRTP int32 `json:"audioRTP,omitempty"`

	// wolf-agent's HTTPS port (bearer-token protected).
	WolfAgent int32 `json:"wolfAgent,omitempty"`

	// The App pod's VR server port (TCP and UDP), for an App with
	// spec.vrPort. Not part of the block: it comes from the operator's
	// separate --vr-port-range, so adding it moved no block.
	VR int32 `json:"vr,omitempty"`
}
