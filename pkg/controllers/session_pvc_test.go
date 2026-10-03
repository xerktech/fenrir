package controllers

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	v1alpha1api "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
)

// rejectImmutablePVCChanges makes the fake clientset refuse a PVC apply the
// way the apiserver does: any change to the spec but resources.requests, and,
// with resizable false, a storage request change too. A declared field left
// out counts as a change, since server-side apply then removes it.
func rejectImmutablePVCChanges(t *testing.T, k8s *k8sfake.Clientset, resizable bool) {
	t.Helper()
	pvcGVR := corev1.SchemeGroupVersion.WithResource("persistentvolumeclaims")
	k8s.PrependReactor("patch", "persistentvolumeclaims", func(action k8stesting.Action) (bool, runtime.Object, error) {
		patch, ok := action.(k8stesting.PatchAction)
		if !ok || patch.GetPatchType() != types.ApplyPatchType {
			return false, nil, nil
		}
		obj, err := k8s.Tracker().Get(pvcGVR, patch.GetNamespace(), patch.GetName())
		if errors.IsNotFound(err) {
			return false, nil, nil
		} else if err != nil {
			return true, nil, fmt.Errorf("get PVC: %w", err)
		}
		pvc, ok := obj.(*corev1.PersistentVolumeClaim)
		if !ok {
			return true, nil, fmt.Errorf("tracker returned %T", obj)
		}
		live := pvc.Spec
		var applied corev1.PersistentVolumeClaim
		if err := json.Unmarshal(patch.GetPatch(), &applied); err != nil {
			return true, nil, fmt.Errorf("decode apply patch: %w", err)
		}
		want := applied.Spec
		if !resizable {
			if !want.Resources.Requests.Storage().Equal(*live.Resources.Requests.Storage()) {
				return true, nil, errors.NewForbidden(pvcGVR.GroupResource(), patch.GetName(),
					stderrors.New("only dynamically provisioned pvc can be resized"))
			}
		}
		want.Resources.Requests, live.Resources.Requests = nil, nil
		live.VolumeName = ""
		if !equality.Semantic.DeepEqual(want, live) {
			return true, nil, errors.NewInvalid(corev1.SchemeGroupVersion.WithKind("PersistentVolumeClaim").GroupKind(),
				patch.GetName(), nil)
		}
		return false, nil, nil
	})
}

// editPVCTemplate replaces the cached App's volumeClaimTemplate, as an App
// edit reaching the informer would.
func editPVCTemplate(t *testing.T, sc *SessionController, sess *v1alpha1api.Session, edit func(*corev1.PersistentVolumeClaimTemplate)) {
	t.Helper()
	cached, err := sc.AppInformer.Namespaced(sess.Namespace).Get(sess.Spec.GameReference.Name)
	if err != nil {
		t.Fatal(err)
	}
	app := cached.DeepCopy()
	edit(app.Spec.VolumeClaimTemplate)
	if err := sc.AppInformer.GetIndexer().Update(app); err != nil {
		t.Fatal(err)
	}
}

// bindPVC gives the session PVC what binding and an out-of-band resize leave
// on it: a defaulted StorageClass, a volume, and the given storage request.
func bindPVC(t *testing.T, k8s *k8sfake.Clientset, sc *SessionController, sess *v1alpha1api.Session, storage string) {
	t.Helper()
	ctx := context.Background()
	pvcs := k8s.CoreV1().PersistentVolumeClaims(sess.Namespace)
	pvc, err := pvcs.Get(ctx, sc.pvcName(sess), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get PVC: %v", err)
	}
	class := "local-path"
	pvc.Spec.StorageClassName = &class
	pvc.Spec.VolumeName = "pv-1"
	pvc.Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse(storage)
	if _, err := pvcs.Update(ctx, pvc, metav1.UpdateOptions{FieldManager: "kubectl"}); err != nil {
		t.Fatalf("bind PVC: %v", err)
	}
}

func getPVC(t *testing.T, k8s *k8sfake.Clientset, sc *SessionController, sess *v1alpha1api.Session) *corev1.PersistentVolumeClaim {
	t.Helper()
	pvc, err := k8s.CoreV1().PersistentVolumeClaims(sess.Namespace).Get(context.Background(), sc.pvcName(sess), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get PVC: %v", err)
	}
	return pvc
}

// TestSessionPVCSurvivesImmutableTemplateEdit edits the App's template in
// ways the apiserver forbids on an existing PVC. Reconcile must keep the live
// spec, report the drift, and still apply the template's metadata (XERK-1519).
func TestSessionPVCSurvivesImmutableTemplateEdit(t *testing.T) {
	sc, k8s, sess, _ := reconcileFixtures(t, "../../examples/user.yaml", "testdata/app-pod-labels.yaml")
	bindPVC(t, k8s, sc, sess, "20Gi")
	rejectImmutablePVCChanges(t, k8s, true)
	editPVCTemplate(t, sc, sess, func(tmpl *corev1.PersistentVolumeClaimTemplate) {
		tmpl.Labels["example.com/tier"] = "gold"
		tmpl.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}
		// Smaller than the PVC was grown to out of band.
		tmpl.Spec.Resources.Requests = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}
		tmpl.Spec.DataSource = &corev1.TypedLocalObjectReference{Kind: "PersistentVolumeClaim", Name: "seed"}
	})

	drift, err := sc.reconcilePVC(context.Background(), sess)
	if err != nil {
		t.Fatalf("reconcilePVC after an immutable template edit: %v", err)
	}
	if want := []string{"accessModes", "dataSource", "resources.requests.storage"}; !slices.Equal(drift, want) {
		t.Errorf("drift = %v, want %v", drift, want)
	}
	pvc := getPVC(t, k8s, sc, sess)
	if got := pvc.Spec.AccessModes; !slices.Equal(got, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}) {
		t.Errorf("accessModes = %v, want the live ReadWriteOnce kept", got)
	}
	if got := pvc.Spec.Resources.Requests.Storage(); got.Cmp(resource.MustParse("20Gi")) != 0 {
		t.Errorf("storage request = %s, want the live 20Gi kept", got)
	}
	if got := pvc.Spec.StorageClassName; got == nil || *got != "local-path" {
		t.Errorf("storageClassName = %v, want the live local-path kept", got)
	}
	if got := pvc.Spec.DataSource; got != nil {
		t.Errorf("dataSource = %v, want the live none kept", got)
	}
	if got := pvc.Labels["example.com/tier"]; got != "gold" {
		t.Errorf("PVC label example.com/tier = %q, want the edited template's gold", got)
	}
}

// TestSessionPVCKeepsDroppedStorageClass removes a storageClassName the
// operator applied earlier from the template. Server-side apply would remove
// the field, which the apiserver rejects as an immutable change (XERK-1519).
func TestSessionPVCKeepsDroppedStorageClass(t *testing.T) {
	sc, k8s, sess, _ := reconcileFixtures(t, "../../examples/user.yaml", "testdata/app-pod-labels.yaml")
	class := "fast"
	ctx := context.Background()
	if err := k8s.CoreV1().PersistentVolumeClaims(sess.Namespace).Delete(ctx, sc.pvcName(sess), metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	editPVCTemplate(t, sc, sess, func(tmpl *corev1.PersistentVolumeClaimTemplate) { tmpl.Spec.StorageClassName = &class })
	if _, err := sc.reconcilePVC(ctx, sess); err != nil {
		t.Fatalf("create PVC: %v", err)
	}
	rejectImmutablePVCChanges(t, k8s, true)
	editPVCTemplate(t, sc, sess, func(tmpl *corev1.PersistentVolumeClaimTemplate) { tmpl.Spec.StorageClassName = nil })

	drift, err := sc.reconcilePVC(ctx, sess)
	if err != nil {
		t.Fatalf("reconcilePVC after dropping storageClassName: %v", err)
	}
	if len(drift) != 0 {
		t.Errorf("drift = %v, want none: an unset field is the apiserver's to default", drift)
	}
	if got := getPVC(t, k8s, sc, sess).Spec.StorageClassName; got == nil || *got != class {
		t.Errorf("storageClassName = %v, want %q kept", got, class)
	}
}

// TestSessionPVCGrowsStorage raises the template's storage request: the one
// spec change reconcile still makes to an existing PVC.
func TestSessionPVCGrowsStorage(t *testing.T) {
	sc, k8s, sess, _ := reconcileFixtures(t, "../../examples/user.yaml", "testdata/app-pod-labels.yaml")
	bindPVC(t, k8s, sc, sess, "5Gi")
	rejectImmutablePVCChanges(t, k8s, true)
	editPVCTemplate(t, sc, sess, func(tmpl *corev1.PersistentVolumeClaimTemplate) {
		tmpl.Spec.Resources.Requests = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("30Gi")}
	})

	drift, err := sc.reconcilePVC(context.Background(), sess)
	if err != nil {
		t.Fatalf("reconcilePVC: %v", err)
	}
	if len(drift) != 0 {
		t.Errorf("drift = %v, want none", drift)
	}
	if got := getPVC(t, k8s, sc, sess).Spec.Resources.Requests.Storage(); got.Cmp(resource.MustParse("30Gi")) != 0 {
		t.Errorf("storage request = %s, want the template's 30Gi", got)
	}
}

// TestSessionPVCResizeRejected raises the storage request on a class that
// cannot expand. Reconcile keeps the live size and reports it rather than
// failing every Session of this user and App.
func TestSessionPVCResizeRejected(t *testing.T) {
	sc, k8s, sess, _ := reconcileFixtures(t, "../../examples/user.yaml", "testdata/app-pod-labels.yaml")
	bindPVC(t, k8s, sc, sess, "5Gi")
	rejectImmutablePVCChanges(t, k8s, false)
	editPVCTemplate(t, sc, sess, func(tmpl *corev1.PersistentVolumeClaimTemplate) {
		tmpl.Labels["example.com/tier"] = "gold"
		tmpl.Spec.Resources.Requests = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("30Gi")}
	})

	drift, err := sc.reconcilePVC(context.Background(), sess)
	if err != nil {
		t.Fatalf("reconcilePVC on a class that cannot resize: %v", err)
	}
	if want := []string{"resources.requests.storage (resize rejected)"}; !slices.Equal(drift, want) {
		t.Errorf("drift = %v, want %v", drift, want)
	}
	pvc := getPVC(t, k8s, sc, sess)
	if got := pvc.Spec.Resources.Requests.Storage(); got.Cmp(resource.MustParse("5Gi")) != 0 {
		t.Errorf("storage request = %s, want the live 5Gi kept", got)
	}
	if got := pvc.Labels["example.com/tier"]; got != "gold" {
		t.Errorf("PVC label example.com/tier = %q, want metadata still applied", got)
	}
}

// TestSessionTemplateDriftCondition reconciles a Session whose PVC no longer
// matches the App's template: the Session proceeds, and VolumeCreated says
// which template fields the PVC keeps its live values for.
func TestSessionTemplateDriftCondition(t *testing.T) {
	f := newPortsFixture(t)
	ctx := context.Background()
	cached, err := f.sc.AppInformer.Namespaced(portsTestNS).Get(f.app)
	if err != nil {
		t.Fatal(err)
	}
	app := cached.DeepCopy()
	fast, slow := "fast", "slow"
	app.Spec.VolumeClaimTemplate = &corev1.PersistentVolumeClaimTemplate{Spec: corev1.PersistentVolumeClaimSpec{
		AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
		StorageClassName: &fast,
		Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("5Gi")}},
	}}
	if err = f.sc.AppInformer.GetIndexer().Update(app); err != nil {
		t.Fatal(err)
	}
	if _, err = f.sc.K8sClient.CoreV1().PersistentVolumeClaims(portsTestNS).Create(ctx, &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: f.user + "-" + f.app, Namespace: portsTestNS},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &slow,
			Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("20Gi")}},
		},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	sessions := f.dw.DirewolfV1alpha1().Sessions(portsTestNS)
	sess := f.session("alex-1", f.user)
	// A zero creation time reads as long unstarted, and the reaper ends it.
	sess.CreationTimestamp = metav1.Now()
	sess, err = sessions.Create(ctx, sess, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// The fake pod never goes ready, so Reconcile requeues; the status is
	// written first.
	_ = f.sc.Reconcile(portsTestNS, sess.Name, sess)
	got, err := sessions.Get(ctx, sess.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, "VolumeCreated")
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != "TemplateDrift" {
		t.Fatalf("VolumeCreated = %+v, want True/TemplateDrift", cond)
	}
	if want := "accessModes, storageClassName, resources.requests.storage"; !strings.Contains(cond.Message, want) {
		t.Errorf("VolumeCreated message = %q, want it to list %q", cond.Message, want)
	}
}
