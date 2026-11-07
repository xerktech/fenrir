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
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

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

// placeholderPrefix starts a placeholder's UID field: "~<unix seconds>",
// written by recordPairedHeadsets before its create.
const placeholderPrefix = "~"

// placeholderPending is how long a placeholder with no Headset behind it
// still holds: its create may be in flight (another worker can read the pod
// between the two). Past it, the record counts as revoked.
const placeholderPending = time.Minute

func placeholderEntry(name string, now time.Time) string {
	return name + "/" + placeholderPrefix + strconv.FormatInt(now.Unix(), 10)
}

// pendingPlaceholder is whether want is a placeholder younger than
// placeholderPending at now.
func pendingPlaceholder(want seededHeadset, now time.Time) bool {
	ts, ok := strings.CutPrefix(want.uid, placeholderPrefix)
	if !ok {
		return false
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	return err == nil && now.Sub(time.Unix(sec, 0)) < placeholderPending
}

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
//
// A placeholder (UID field "~<time>", written by recordPairedHeadsets before
// its create) holds while the Headset of that name is the operator's record
// of its own User and key (whose name it is). With no Headset it holds only
// while pending (headsetRevokedReason); then the record was revoked (or
// never made), so the Session ends.
func stillSeeded(h *v1alpha1types.Headset, want seededHeadset) bool {
	if h.DeletionTimestamp != nil {
		return false
	}
	if strings.HasPrefix(want.uid, placeholderPrefix) {
		return h.Name == headsetObjectName(h.Spec.UserReference.Name, h.Spec.PublicKey)
	}
	return string(h.UID) == want.uid
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
		case errors.IsNotFound(err) && pendingPlaceholder(want, time.Now()):
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

// maxUserHeadsets caps a User's Headsets the operator records from pairings:
// the game shares WiVRn's config dir and could list any number of keys.
const maxUserHeadsets = 8

// wivrnKeyPattern is the CRD's publicKey pattern: WiVRn's stored form.
var wivrnKeyPattern = regexp.MustCompile(`^([A-Za-z0-9+/]{4})*([A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$`)

// IsWiVRnKey is whether key is in WiVRn's stored form, as the CRD admits it.
func IsWiVRnKey(key string) bool {
	return len(key) >= 4 && len(key) <= 1024 && wivrnKeyPattern.MatchString(key)
}

// CleanHeadsetName is a headset's own name as a Headset's displayName: valid
// UTF-8 with no control, format (bidi) or line/paragraph-separator runes, cut
// to the CRD's 128 bytes on a rune boundary.
func CleanHeadsetName(name string) string {
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(name, ""))
	for len(name) > 128 {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	return name
}

// headsetObjectName is the Headset recordPairedHeadsets makes for user's key:
// one per key, so a second report of it finds the first.
func headsetObjectName(user, key string) string {
	sum := sha256.Sum256([]byte(key))
	if len(user) > 200 {
		user = user[:200]
	}
	return strings.TrimRight(user, ".-") + "-" + hex.EncodeToString(sum[:6])
}

// seededKeys returns the keys pod's WiVRn was seeded with, from its App
// containers' vrHeadsetsEnv.
func seededKeys(pod *corev1.Pod) map[string]bool {
	res := map[string]bool{}
	for _, ctrs := range [][]corev1.Container{pod.Spec.InitContainers, pod.Spec.Containers} {
		for i := range ctrs {
			for _, e := range ctrs[i].Env {
				if e.Name != vrHeadsetsEnv {
					continue
				}
				var keys []wivrnKnownKey
				if json.Unmarshal([]byte(e.Value), &keys) == nil {
					for _, k := range keys {
						res[k.Key] = true
					}
				}
			}
		}
	}
	return res
}

// recordPairedHeadsets turns each key WiVRn admits but no Headset holds (a
// PIN pairing this session) into a Headset of the session's User, so the
// next session admits it, and adds every Headset WiVRn admits to pod's
// vrHeadsetsAnnotation, so revoking it ends this session too. The
// annotation is read live and patched each poll until it holds them all: a
// failed patch or create is retried by the next poll.
//
// Nothing is recorded while any Headset the pod records is revoked (gone,
// terminating or re-created): the revoke may have landed during this poll,
// WiVRn still lists its key, and recording it under any name would undo the
// revoke for the next session. The Session ends on its own reconcile. Nor
// is a key the pod was seeded with, for the same reason.
func (c *SessionController) recordPairedHeadsets(ctx context.Context, session *v1alpha1types.Session, pod *corev1.Pod, known []VRKnownKey) error {
	if c.HeadsetClient == nil || len(known) == 0 {
		return nil
	}
	live, err := c.K8sClient.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get pod %s: %w", pod.Name, err)
	}
	// The User's Headsets first, the revoke check second: a delete after
	// this snapshot leaves its key in byKey (adopted, never created again),
	// and one before it is caught by the check's live GET.
	user := session.Spec.UserReference.Name
	existing, err := c.userHeadsets(session.Namespace, user)
	if err != nil {
		return err
	}
	if reason, revokeErr := c.headsetRevokedReason(ctx, live); revokeErr != nil || reason != "" {
		return revokeErr
	}
	recorded := map[string]bool{}
	for _, h := range podHeadsets(live) {
		recorded[h.name] = true
		recorded[h.name+"/"+h.uid] = true
	}
	byKey := map[string]*v1alpha1types.Headset{}
	for _, h := range existing {
		byKey[h.Spec.PublicKey] = h
	}
	seeded := seededKeys(live)
	var add []string
	var errs []error
	count := len(existing)
	for _, k := range known {
		if h := byKey[k.Key]; h != nil {
			if entry := h.Name + "/" + string(h.UID); !recorded[entry] {
				recorded[entry] = true
				add = append(add, entry)
			}
			continue
		}
		// A key whose Headset the pod records by the operator's name but no
		// Headset holds was revoked: never again (also covered by the check
		// above, unless the delete lands between its reads).
		if seeded[k.Key] || recorded[headsetObjectName(user, k.Key)] || !IsWiVRnKey(k.Key) {
			continue
		}
		if count >= maxUserHeadsets {
			klog.V(2).Infof("Session %s/%s: user %s has %d headsets; not recording another", session.Namespace, session.Name, user, count)
			break
		}
		// A placeholder first: once the Headset exists, a revoke must find
		// this pod (reconcileHeadset) and stop a later poll re-creating it,
		// even if the operator dies or the UID's patch fails right after.
		name := headsetObjectName(user, k.Key)
		// Writes only when the name is free and no create failed lately:
		// each pod write re-queues the Session, which polls again.
		if c.headsetBackoff.active(name) {
			continue
		}
		if taken, err := c.HeadsetClient.Get(ctx, name, metav1.GetOptions{}); err == nil {
			if taken.Spec.UserReference.Name == user && taken.Spec.PublicKey == k.Key && taken.DeletionTimestamp == nil {
				// Ours, not yet in the cache.
				entry := taken.Name + "/" + string(taken.UID)
				byKey[k.Key] = taken
				count++
				recorded[entry] = true
				add = append(add, entry)
			} else {
				klog.V(2).Infof("Session %s/%s: headset name %s is taken; not recording the key", session.Namespace, session.Name, name)
			}
			continue
		} else if !errors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("failed to get headset %s: %w", name, err))
			break
		}
		placeholder := placeholderEntry(name, time.Now())
		reserved, err := c.patchSeededHeadsets(ctx, live, withSeededEntries(live.Annotations[vrHeadsetsAnnotation], placeholder))
		if err != nil {
			errs = append(errs, err)
			break
		}
		live = reserved
		recorded[name] = true
		h, err := c.createHeadset(ctx, session, k)
		if err != nil || h == nil {
			c.headsetBackoff.set(name)
			// No Headset: the placeholder would end the Session, so drop
			// it. Failing that, the Session ends, which is safe.
			if p, undoErr := c.patchSeededHeadsets(ctx, live, withoutSeededEntry(live.Annotations[vrHeadsetsAnnotation], placeholder)); undoErr != nil {
				errs = append(errs, undoErr)
			} else {
				live = p
			}
			if err != nil {
				errs = append(errs, err)
				break
			}
			continue
		}
		byKey[k.Key] = h
		count++
		klog.Infof("Session %s/%s: recorded headset %s for user %s", session.Namespace, session.Name, h.Name, user)
		entry := h.Name + "/" + string(h.UID)
		recorded[entry] = true
		add = append(add, entry)
	}
	if len(add) > 0 {
		if _, err := c.patchSeededHeadsets(ctx, live, withSeededEntries(live.Annotations[vrHeadsetsAnnotation], add...)); err != nil {
			errs = append(errs, err)
		}
	}
	return stderrors.Join(errs...)
}

// createHeadset records k for session's User, or returns the Headset already
// recording it (nil: its name is taken by another key or User).
func (c *SessionController) createHeadset(ctx context.Context, session *v1alpha1types.Session, k VRKnownKey) (*v1alpha1types.Headset, error) {
	user := session.Spec.UserReference.Name
	display := CleanHeadsetName(k.Name)
	h := &v1alpha1types.Headset{
		ObjectMeta: metav1.ObjectMeta{
			Name:      headsetObjectName(user, k.Key),
			Namespace: session.Namespace,
		},
		Spec: v1alpha1types.HeadsetSpec{
			UserReference: v1alpha1types.UserReference{Name: user},
			PublicKey:     k.Key,
			DisplayName:   display,
		},
	}
	// Garbage collected with its User, read live on a cache miss: without
	// an owner it would outlive the User and pass to a new one of its name.
	u, err := c.UserInformer.Namespaced(session.Namespace).Get(user)
	if errors.IsNotFound(err) {
		u, err = c.UserClient.Get(ctx, user, metav1.GetOptions{})
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get user %s: %w", user, err)
	}
	h.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: v1alpha1types.GroupVersion.String(), Kind: "User", Name: u.Name, UID: u.UID,
	}}
	created, err := c.HeadsetClient.Create(ctx, h, metav1.CreateOptions{})
	if err == nil {
		return created, nil
	} else if !errors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("failed to create headset %s: %w", h.Name, err)
	}
	live, err := c.HeadsetClient.Get(ctx, h.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get headset %s: %w", h.Name, err)
	}
	if live.Spec.UserReference.Name != user || live.Spec.PublicKey != k.Key || live.DeletionTimestamp != nil {
		klog.V(2).Infof("Session %s/%s: headset name %s is taken; not recording the key", session.Namespace, session.Name, h.Name)
		return nil, nil
	}
	return live, nil
}

// withSeededEntries adds entries to a vrHeadsetsAnnotation value; an entry
// replaces that name's placeholder.
func withSeededEntries(value string, entries ...string) string {
	var list []string
	if value != "" {
		list = strings.Split(value, ",")
	}
	for _, e := range entries {
		name, _, _ := strings.Cut(e, "/")
		list = slices.DeleteFunc(list, func(x string) bool { return strings.HasPrefix(x, name+"/"+placeholderPrefix) })
		if !slices.Contains(list, e) {
			list = append(list, e)
		}
	}
	return strings.Join(list, ",")
}

// withoutSeededEntry removes entry from a vrHeadsetsAnnotation value.
func withoutSeededEntry(value, entry string) string {
	if value == "" {
		return ""
	}
	return strings.Join(slices.DeleteFunc(strings.Split(value, ","), func(x string) bool { return x == entry }), ",")
}

// patchSeededHeadsets sets live's vrHeadsetsAnnotation to value, guarded by
// its resourceVersion so a concurrent change is not lost (a conflict is
// retried by the next poll), and returns the patched pod.
func (c *SessionController) patchSeededHeadsets(ctx context.Context, live *corev1.Pod, value string) (*corev1.Pod, error) {
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{
		"resourceVersion": live.ResourceVersion,
		"annotations":     map[string]string{vrHeadsetsAnnotation: value},
	}})
	if err != nil {
		return nil, fmt.Errorf("failed to encode pod patch: %w", err)
	}
	patched, err := c.K8sClient.CoreV1().Pods(live.Namespace).Patch(ctx, live.Name, types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to record headsets on pod %s: %w", live.Name, err)
	}
	return patched, nil
}

// headsetBackoffPeriod is how long recordPairedHeadsets leaves a key whose
// create failed: retried every poll, its placeholder's two pod writes would
// re-queue the Session in a loop.
const headsetBackoffPeriod = 5 * time.Minute

// headsetBackoff remembers failed creates by Headset name.
type headsetBackoff struct {
	mu    sync.Mutex
	until map[string]time.Time
}

func (b *headsetBackoff) active(name string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.until[name]
	if ok && time.Now().After(t) {
		delete(b.until, name)
		return false
	}
	return ok
}

func (b *headsetBackoff) set(name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.until == nil {
		b.until = map[string]time.Time{}
	}
	b.until[name] = time.Now().Add(headsetBackoffPeriod)
}
