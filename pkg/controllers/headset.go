package controllers

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	v1alpha1client "games-on-whales.github.io/direwolf/pkg/generated/clientset/versioned/typed/api/v1alpha1"
	"games-on-whales.github.io/direwolf/pkg/generic"
)

// Headsets (XERK-2669): a User's Headset objects are the only keys WiVRn lets
// connect. The vr image writes vrHeadsetsEnv whole into WiVRn's
// known_keys.json at every start, so a key left on the User's volume by an
// earlier session (or written there by the game) is dropped. Headsets are
// made only by the VR front door (XERK-2808): a User with none gets no VR
// Session (buildPod ends it InvalidSpec), as an empty list would open WiVRn's PIN pairing,
// which the game can use too (XERK-2811). Deleting a Headset ends the
// Sessions whose pod was seeded with it.
const (
	// vrHeadsetsEnv holds the User's headset keys in WiVRn's known_keys.json
	// format, on every container of a vrPort App.
	vrHeadsetsEnv = "DIREWOLF_VR_HEADSETS"
	// vrHeadsetsAnnotation on a VR App pod lists the Headsets its WiVRn was
	// seeded with, as comma-separated <name>/<uid>; revoking any ends the
	// Session. The UID tells a Headset re-created under the same name (maybe
	// another key or User) from the one seeded. Present but empty: none.
	vrHeadsetsAnnotation = "direwolf.games-on-whales.github.io/vr-headsets"
)

// wivrnKnownKey is one entry of WiVRn's known_keys.json
// (server/driver/configuration.cpp).
type wivrnKnownKey struct {
	Key  string `json:"key"`
	Name string `json:"name"`
}

// WatchHeadsets turns on headset seeding and revocation. Call it before Run.
// Without it no User has a Headset, so every VR App's Session ends.
func (c *SessionController) WatchHeadsets(client v1alpha1client.HeadsetInterface, informer generic.Informer[*v1alpha1types.Headset]) {
	c.HeadsetClient = client
	c.HeadsetInformer = informer
	c.headsetController = generic.NewController(
		informer,
		c.reconcileHeadset,
		generic.ControllerOptions{
			Name:    "session-controller-headset",
			Workers: 1,
		},
	)
}

// userHeadsets returns user's Headsets in namespace, sorted by name so a
// pod's env and annotation don't change with cache order.
func (c *SessionController) userHeadsets(namespace, user string) ([]*v1alpha1types.Headset, error) {
	if c.HeadsetInformer == nil {
		return nil, nil
	}
	all, err := c.HeadsetInformer.Namespaced(namespace).List(labels.Everything())
	if err != nil {
		return nil, fmt.Errorf("failed to list headsets: %w", err)
	}
	var res []*v1alpha1types.Headset
	for _, h := range all {
		if h.Spec.UserReference.Name == user && h.DeletionTimestamp == nil {
			res = append(res, h)
		}
	}
	slices.SortFunc(res, func(a, b *v1alpha1types.Headset) int { return strings.Compare(a.Name, b.Name) })
	return res, nil
}

// liveUserHeadsets is userHeadsets read from the API server, for a cache
// miss that would otherwise refuse a Session.
func (c *SessionController) liveUserHeadsets(namespace, user string) ([]*v1alpha1types.Headset, error) {
	ctx := c.runCtx
	if ctx == nil { // called without Run (tests)
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, wolfAgentTimeout)
	defer cancel()
	list, err := c.HeadsetClient.List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list headsets in %s: %w", namespace, err)
	}
	var res []*v1alpha1types.Headset
	for i := range list.Items {
		if h := &list.Items[i]; h.Spec.UserReference.Name == user && h.DeletionTimestamp == nil {
			res = append(res, h)
		}
	}
	slices.SortFunc(res, func(a, b *v1alpha1types.Headset) int { return strings.Compare(a.Name, b.Name) })
	return res, nil
}

// withVRHeadsets seeds the App's containers in spec with headsets, and
// returns the vrHeadsetsAnnotation value recording them. spec must hold only
// the App's containers.
func withVRHeadsets(spec *corev1.PodSpec, headsets []*v1alpha1types.Headset) (string, error) {
	keys := make([]wivrnKnownKey, 0, len(headsets))
	names := make([]string, 0, len(headsets))
	for _, h := range headsets {
		name := h.Spec.DisplayName
		if name == "" {
			name = h.Name
		}
		keys = append(keys, wivrnKnownKey{Key: h.Spec.PublicKey, Name: name})
		names = append(names, h.Name+"/"+string(h.UID))
	}
	value, err := json.Marshal(keys)
	if err != nil {
		return "", fmt.Errorf("failed to encode headset keys: %w", err)
	}
	env := corev1.EnvVar{Name: vrHeadsetsEnv, Value: string(value)}
	for _, ctrs := range [][]corev1.Container{spec.InitContainers, spec.Containers} {
		for i := range ctrs {
			// The App's own value would let any key in: it is replaced.
			ctrs[i].Env = append([]corev1.EnvVar{env}, slices.DeleteFunc(slices.Clone(ctrs[i].Env), func(e corev1.EnvVar) bool {
				return e.Name == vrHeadsetsEnv
			})...)
		}
	}
	return strings.Join(names, ","), nil
}

// seededHeadset is one entry of vrHeadsetsAnnotation.
type seededHeadset struct{ name, uid string }

// podHeadsets returns the Headsets pod was seeded with.
func podHeadsets(pod *corev1.Pod) []seededHeadset {
	v := pod.Annotations[vrHeadsetsAnnotation]
	if v == "" {
		return nil
	}
	var res []seededHeadset
	for e := range strings.SplitSeq(v, ",") {
		name, uid, _ := strings.Cut(e, "/")
		res = append(res, seededHeadset{name, uid})
	}
	return res
}

// isSeeded is whether pod was seeded with a Headset named name.
func isSeeded(pod *corev1.Pod, name string) bool {
	return slices.ContainsFunc(podHeadsets(pod), func(h seededHeadset) bool { return h.name == name })
}

// stillSeeded is whether h is the live Headset seeded as want: not
// re-created under its name, and not being deleted (held by a finalizer).
func stillSeeded(h *v1alpha1types.Headset, want seededHeadset) bool {
	return h.DeletionTimestamp == nil && string(h.UID) == want.uid
}

// headsetRevokedReason says why pod's Session must end because a Headset its
// WiVRn was seeded with is gone, or "" if it must not. Checked on every
// reconcile, so a delete the operator missed (it was down) still ends it.
func (c *SessionController) headsetRevokedReason(ctx context.Context, pod *corev1.Pod) (string, error) {
	if c.HeadsetInformer == nil {
		return "", nil
	}
	for _, want := range podHeadsets(pod) {
		cached, err := c.HeadsetInformer.Namespaced(pod.Namespace).Get(want.name)
		if err == nil && stillSeeded(cached, want) {
			continue
		} else if err != nil && !errors.IsNotFound(err) {
			return "", fmt.Errorf("failed to get headset %s/%s: %w", pod.Namespace, want.name, err)
		}
		// The Headset watch can lag the Session's and the pod's: ask the
		// API server before ending anything.
		live, err := c.HeadsetClient.Get(ctx, want.name, metav1.GetOptions{})
		switch {
		case err == nil && stillSeeded(live, want):
			continue
		case err == nil || errors.IsNotFound(err):
			return fmt.Sprintf("its headset %s was revoked", want.name), nil
		default:
			return "", fmt.Errorf("failed to get headset %s/%s: %w", pod.Namespace, want.name, err)
		}
	}
	return "", nil
}

// reconcileHeadset ends the Sessions of a revoked Headset at once, as
// reconcilePairing does for a Pairing: deleted, being deleted, or re-created
// under its name. It finds them by their pods' annotation, as a deleted
// object no longer says whose it was.
func (c *SessionController) reconcileHeadset(namespace, name string, _ *v1alpha1types.Headset) error {
	ctx := c.runCtx
	if ctx == nil { // called without Run (tests)
		ctx = context.Background()
	}
	pods, err := c.K8sClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(labels.Set{v1alpha1types.SessionPodLabel: v1alpha1types.SessionPodLabelValue}).String(),
	})
	if err != nil {
		return fmt.Errorf("failed to list session pods: %w", err)
	}
	var errs []error
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !isSeeded(pod, name) {
			continue
		}
		owner := metav1.GetControllerOf(pod)
		if owner == nil || owner.Kind != sessionKind {
			continue
		}
		session, err := c.SessionInformer.Namespaced(namespace).Get(owner.Name)
		if errors.IsNotFound(err) {
			continue
		} else if err != nil {
			errs = append(errs, fmt.Errorf("failed to get session %s/%s: %w", namespace, owner.Name, err))
			continue
		}
		if session.UID != owner.UID {
			continue
		}
		reason, err := c.headsetRevokedReason(ctx, pod)
		if err == nil && reason != "" {
			err = c.endSession(ctx, session, reason)
		}
		errs = append(errs, err)
	}
	return stderrors.Join(errs...)
}

// IsWiVRnKey reports whether key is in a Headset's publicKey form.
var IsWiVRnKey = v1alpha1types.IsWiVRnKey

// CleanHeadsetName is v1alpha1.CleanHeadsetName.
var CleanHeadsetName = v1alpha1types.CleanHeadsetName
