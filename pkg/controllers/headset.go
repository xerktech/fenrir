package controllers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	v1alpha1client "games-on-whales.github.io/direwolf/pkg/generated/clientset/versioned/typed/api/v1alpha1"
	"games-on-whales.github.io/direwolf/pkg/generic"
)

// Headsets (XERK-2669): a User's Headset objects are the only keys WiVRn lets
// connect. The vr image writes vrHeadsetsEnv whole into WiVRn's
// known_keys.json at every start, so a key left on the User's volume by an
// earlier session (or written there by the game) is dropped. With no
// Headsets the list is empty and WiVRn pairs by PIN, which only the
// authenticated mirror shows. Deleting a Headset ends the Sessions whose pod
// was seeded with it.
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
// Without it a VR App's WiVRn is seeded with no keys: every session pairs by
// PIN.
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
	errs = append(errs, c.endSessionsOfUnrecordedHeadset(ctx, namespace, name))
	return stderrors.Join(errs...)
}

// endSessionsOfUnrecordedHeadset ends the VR Sessions whose pending key the
// User confirmed as Headset name, if that Headset is gone again before a poll
// named it in the pod (XERK-2747): WiVRn trusts the key in that Session, so
// the revoke must end it, as for a recorded one. A confirmed Headset's name
// is recordedHeadsetName, so the pending key names it. Only a Headset event
// gets here, so the Headset existed: a key nobody confirmed is not revoked.
func (c *SessionController) endSessionsOfUnrecordedHeadset(ctx context.Context, namespace, name string) error {
	sessions, err := c.SessionInformer.Namespaced(namespace).List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list sessions: %w", err)
	}
	sessions = slices.DeleteFunc(slices.Clone(sessions), func(s *v1alpha1types.Session) bool {
		return !s.Spec.IsVR() || s.DeletionTimestamp != nil || !slices.ContainsFunc(s.Status.PendingHeadsets, func(p v1alpha1types.PendingHeadset) bool {
			return recordedHeadsetName(s.Spec.UserReference.Name, p.Key) == name
		})
	})
	if len(sessions) == 0 {
		return nil
	}
	// The watch can lag: a Headset just created (the confirm itself) is
	// not revoked. Any other error is retried, never read as gone.
	h, err := c.HeadsetClient.Get(ctx, name, metav1.GetOptions{})
	if err == nil && h.DeletionTimestamp == nil {
		return nil
	} else if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("failed to get headset %s/%s: %w", namespace, name, err)
	}
	var errs []error
	for _, s := range sessions {
		errs = append(errs, c.endSession(ctx, s, fmt.Sprintf("its headset %s was revoked", name)))
	}
	return stderrors.Join(errs...)
}

// maxPendingHeadsets bounds a Session's pendingHeadsets: a PIN pairing adds
// one key, so more are the game's.
const maxPendingHeadsets = 4

// wivrnKeyPattern is the CRD's publicKey pattern: WiVRn's stored form.
var wivrnKeyPattern = regexp.MustCompile(`^([A-Za-z0-9+/]{4})*([A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$`)

// IsWiVRnKey is whether key is in WiVRn's stored form, as the CRD admits it.
func IsWiVRnKey(key string) bool {
	return len(key) >= 4 && len(key) <= 1024 && wivrnKeyPattern.MatchString(key)
}

// headsetNameMaxLen is the CRDs' limit on a headset's name. OpenAPI's
// maxLength counts characters (code points), not bytes.
const headsetNameMaxLen = 128

// CleanHeadsetName is a headset's own name as a Headset's displayName: valid
// UTF-8 with no control, format (bidi) or line/paragraph-separator runes, cut
// to the CRD's 128 characters.
func CleanHeadsetName(name string) string {
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(name, ""))
	if r := []rune(name); len(r) > headsetNameMaxLen {
		name = string(r[:headsetNameMaxLen])
	}
	return name
}

// pairingWasOpen is whether pod's WiVRn was seeded with no Headsets, so it
// paired by PIN: only there can a headset have paired.
func pairingWasOpen(pod *corev1.Pod) bool {
	v, ok := pod.Annotations[vrHeadsetsAnnotation]
	return ok && v == ""
}

// recordedHeadsetName names the Headset confirmed for user's key: the same
// on every confirm, so a repeat finds the one it made.
func recordedHeadsetName(user, key string) string {
	sum := sha256.Sum256([]byte(user + "\x00" + key))
	return "headset-" + hex.EncodeToString(sum[:10])
}

// trackPairedHeadsets follows the keys pod's WiVRn knows, as its vr-agent
// reported them, while the pod paired by PIN (XERK-2742, XERK-2747).
//
// A key no Headset of the User holds goes to the Session's pendingHeadsets,
// never into a Headset: the game runs as WiVRn's uid and owns its bus, so it
// can plant a key or pair one itself, and nothing here tells that from the
// User's headset. The User confirms on the library page (confirmHeadset).
//
// A key a Headset of the User does hold (one just confirmed) is named in the
// pod's vrHeadsetsAnnotation: WiVRn trusts it in this Session, so revoking
// it must end it, as for a seeded Headset. The pod then no longer paired by
// PIN, and its keys stop being followed; its other pending keys are dropped,
// as a pairing adds only the one confirmed.
func (c *SessionController) trackPairedHeadsets(ctx context.Context, session *v1alpha1types.Session, pod *corev1.Pod, known []VRKnownKey) error {
	if c.HeadsetClient == nil || !pairingWasOpen(pod) {
		return nil
	}
	held, err := c.userHeadsets(session.Namespace, session.Spec.UserReference.Name)
	if err != nil {
		return err
	}
	seen := map[string]metav1.Time{}
	for _, p := range session.Status.PendingHeadsets {
		seen[p.Key] = p.FirstSeen
	}
	// Whether a key the User confirmed was pending here: only then is it
	// this Session's pairing. A key of an older Headset can be the game's
	// (every App container's seed env lists them), and must not drop the
	// real pairing's key from the list (QA).
	paired := false
	var (
		pending   []v1alpha1types.PendingHeadset
		confirmed []string
		errs      []error
	)
	for _, k := range known {
		if !IsWiVRnKey(k.Key) {
			errs = append(errs, stderrors.New("WiVRn reports a key not in its stored form; ignoring it"))
			continue
		}
		if i := slices.IndexFunc(held, func(h *v1alpha1types.Headset) bool { return h.Spec.PublicKey == k.Key }); i >= 0 {
			if entry := held[i].Name + "/" + string(held[i].UID); !slices.Contains(confirmed, entry) {
				confirmed = append(confirmed, entry)
			}
			if _, ok := seen[k.Key]; ok {
				paired = true
			}
			continue
		}
		if slices.ContainsFunc(pending, func(p v1alpha1types.PendingHeadset) bool { return p.Key == k.Key }) {
			continue
		}
		if len(pending) == maxPendingHeadsets {
			errs = append(errs, fmt.Errorf("WiVRn knows more than %d unconfirmed keys; listing only the first", maxPendingHeadsets))
			break
		}
		first, ok := seen[k.Key]
		if !ok {
			first = metav1.Now()
		}
		pending = append(pending, v1alpha1types.PendingHeadset{Key: k.Key, Name: CleanHeadsetName(k.Name), FirstSeen: first})
	}
	if paired {
		// A PIN pairing adds one key and the User confirmed it: any other
		// key here is the game's, never to be offered again (QA).
		pending = nil
	}
	session.Status.PendingHeadsets = pending
	if len(confirmed) > 0 {
		slices.Sort(confirmed)
		errs = append(errs, c.annotateHeadsets(ctx, pod, strings.Join(confirmed, ",")))
	}
	return stderrors.Join(errs...)
}

// annotateHeadsets records value as the Headsets pod's WiVRn trusts.
func (c *SessionController) annotateHeadsets(ctx context.Context, pod *corev1.Pod, value string) error {
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{
		// Fails the patch if the pod changed since it was read: another
		// poll may have recorded first, or the pod is no longer the one
		// whose empty annotation said it paired by PIN.
		"resourceVersion": pod.ResourceVersion,
		"annotations":     map[string]string{vrHeadsetsAnnotation: value},
	}})
	if err != nil {
		return fmt.Errorf("failed to encode pod patch: %w", err)
	}
	if _, err := c.K8sClient.CoreV1().Pods(pod.Namespace).Patch(ctx, pod.Name, types.MergePatchType, patch, metav1.PatchOptions{
		FieldManager: "direwolf-session-controller-pod",
	}); err != nil {
		return fmt.Errorf("failed to record headsets %s on pod %s/%s: %w", value, pod.Namespace, pod.Name, err)
	}
	klog.Infof("Pod %s/%s: WiVRn trusts confirmed headsets %s", pod.Namespace, pod.Name, value)
	return nil
}
