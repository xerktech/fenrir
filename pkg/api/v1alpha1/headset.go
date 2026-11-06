package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Headset is a VR headset paired with a User (XERK-2669): the public key the
// VR server (WiVRn) lets connect to that User's VR Sessions. The operator
// writes a User's Headsets into WiVRn's known keys at every VR session start,
// so these objects, not the keys file on the User's volume, decide which
// headsets connect. Deleting one revokes the headset and ends the User's VR
// Sessions, as deleting a Pairing does for a Moonlight client.
//
// The spec is immutable: a changed key would let a different headset in
// without ending the Sessions the old one may hold.
//
// +kubebuilder:object:root=true
// +kubebuilder:validation:XValidation:rule="self.spec == oldSelf.spec",message="spec is immutable; delete the Headset to revoke it"
// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type Headset struct {
	metav1.TypeMeta `json:",inline"`
	// Standard object metadata; More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	//+kubebuilder:validation:Required
	Spec HeadsetSpec `json:"spec"`
}

type HeadsetSpec struct {
	//+kubebuilder:validation:Required
	UserReference UserReference `json:"userReference"`

	// PublicKey is the headset's long-term public key as WiVRn stores it in
	// known_keys.json: a PEM "PUBLIC KEY" block.
	//+kubebuilder:validation:Required
	//+kubebuilder:validation:MaxLength=1024
	//+kubebuilder:validation:Pattern=`^-----BEGIN PUBLIC KEY-----\n[A-Za-z0-9+/=\n]+-----END PUBLIC KEY-----\n?$`
	PublicKey string `json:"publicKey"`

	// DisplayName is the name WiVRn shows for the key (the headset's model
	// at pairing).
	//+kubebuilder:validation:MaxLength=128
	// +optional
	DisplayName string `json:"displayName,omitempty"`
}

// HeadsetList is a list of Headsets.
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type HeadsetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Headset `json:"items"`
}
