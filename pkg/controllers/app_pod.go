package controllers

import (
	"context"
	stderrors "errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
)

// With SessionControllerOptions.AppPodHostDir set, a Session runs its App in a
// pod of its own, in a user namespace (hostUsers: false), next to the Wolf pod
// (XERK-1920). An App container with SYS_ADMIN in the host user namespace
// could mount a devtmpfs (the host's keyboards, other sessions' pads) or
// remount /proc/sys read-write and write the node's core_pattern (measured on
// talos04); in a user namespace both are refused, and procMount: Unmasked lets
// Steam's bwrap mount its /proc without SYS_ADMIN at all.
//
// Only the App moves: the apiserver refuses hostUsers: false with hostNetwork,
// which Moonlight's port-only redirect needs, and a user-namespaced Wolf could
// not open /dev/uinput (0660 root:996 on the node). The pods share, through
// per-Session host directories under AppPodHostDir:
//   - runtime: XDG_RUNTIME_DIR (Wolf's Wayland and pulseaudio sockets), the
//     Wolf pod's size-limited memory emptyDir, which the App pod reaches
//     through kubelet's directory for it (appRuntimeVolume),
//   - udev: /run/udev, written by wolf-agent, read-only in the App pod,
//   - input: /dev/input, read-only in the App pod with HostToContainer
//     propagation. The runtime mounts a user-namespaced pod's volumes nodev,
//     so the input-bridge sidecar bind-mounts each node there (pkg/inputbridge).
//
// And the GPU: spec.gpu's ResourceClaim is a named one, which both pods
// reference, so Wolf encodes on the card the game renders on.
const (
	// On the Wolf pod: the name of its App pod. Its presence, not the
	// operator's flag, says the Session runs split, so a flag flip leaves
	// running Sessions as they are.
	appPodAnnotation = "direwolf/app-pod"
	// appPodCreatedCondition is True once the App pod has been created; like
	// the Wolf pod it is never recreated.
	appPodCreatedCondition = "AppPodCreated"
	// sessionUIDLabel ties the App pod to its Wolf pod's node (podAffinity).
	sessionUIDLabel = "direwolf/session-uid"
	// podRoleLabel tells a split Session's two pods apart.
	podRoleLabel = "direwolf/pod-role"
	podRoleWolf  = "wolf"
	podRoleApp   = "app"

	inputBridgeVolume    = "direwolf-input-bridge"
	appPodSessionsVolume = "direwolf-sessions"
	wolfRuntimeVolume    = "wolf-runtime"
	// The bridge's udev event socket, in <dir>/<UID>/events; only the
	// udev-relay mounts it.
	allCapabilities      = "ALL"
	udevEventsVolume     = "direwolf-udev-events"
	udevEventsSocket     = "events.sock"
	udevRelayEventsDir   = "/run/direwolf-udev-events"
	inputBridgeSocketDir = "/run/direwolf-input-bridge"
	inputBridgeSocket    = inputBridgeSocketDir + "/bridge.sock"
	// Where the bridge mounts AppPodHostDir.
	inputBridgeSessionsPath = "/sessions"
	// How much older than its Session a leftover directory must be for the
	// bridge to sweep it (inputBridgeContainer).
	sweepMargin = 10 * time.Minute
	// runtimeSizeLimit caps the runtime directory, which holds sockets and
	// lock files.
	runtimeSizeLimit = "64Mi"
	// kubeletPodsDir is where kubelet keeps each pod's volumes.
	kubeletPodsDir = "/var/lib/kubelet/pods"
	// runtimeSubPath is the runtime directory within its volume: see
	// appRuntimeVolume.
	runtimeSubPath = "xdg"
)

func (c *SessionController) appPodName(session *v1alpha1types.Session) string {
	return c.podName(session) + "-app"
}

// sessionHostDir is the Session's directory under AppPodHostDir. Its UID, not
// its name: a new Session of the same name must not see the old one's nodes.
func (c *SessionController) sessionHostDir(session *v1alpha1types.Session, sub ...string) string {
	return path.Join(append([]string{c.AppPodHostDir, string(session.UID)}, sub...)...)
}

// isWolfPodContainer reports whether name is one of the operator's containers
// that stay in the Wolf pod. steam-relink works on the App's Steam home, so it
// goes with the App.
func isWolfPodContainer(name string) bool {
	switch name {
	case v1alpha1types.WolfContainerName, v1alpha1types.WolfAgentContainerName,
		v1alpha1types.PulseAudioContainerName, v1alpha1types.InitContainerName,
		v1alpha1types.InputBridgeContainerName:
		return true
	}
	return false
}

// splitAppPod splits the single session pod buildPod renders into the Wolf
// pod and the App pod, for the Wolf pod with UID wolfUID ("" before it
// exists; the App pod is then unusable).
func (c *SessionController) splitAppPod(session *v1alpha1types.Session, combined *corev1.Pod, wolfUID types.UID) (wolf, app *corev1.Pod, err error) {
	wolf = combined.DeepCopy()
	app = combined.DeepCopy()

	wolf.Spec.InitContainers = slices.DeleteFunc(wolf.Spec.InitContainers, func(c corev1.Container) bool { return !isWolfPodContainer(c.Name) })
	wolf.Spec.Containers = slices.DeleteFunc(wolf.Spec.Containers, func(c corev1.Container) bool { return !isWolfPodContainer(c.Name) })
	app.Spec.InitContainers = slices.DeleteFunc(app.Spec.InitContainers, func(c corev1.Container) bool { return isWolfPodContainer(c.Name) })
	app.Spec.Containers = slices.DeleteFunc(app.Spec.Containers, func(c corev1.Container) bool { return isWolfPodContainer(c.Name) })

	hostDir := func(sub string) corev1.VolumeSource {
		return corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
			Path: c.sessionHostDir(session, sub),
			Type: new(corev1.HostPathDirectoryOrCreate),
		}}
	}
	shared := map[string]corev1.VolumeSource{
		hotplugUdevVolume: hostDir("udev"),
		hotplugDevVolume:  hostDir("input"),
	}

	// wolf-agent publishes nodes and udev entries through the bridge; it
	// can't write their locked directories anyway.
	for i := range wolf.Spec.Containers {
		ctr := &wolf.Spec.Containers[i]
		if ctr.Name != v1alpha1types.WolfAgentContainerName {
			continue
		}
		ctr.VolumeMounts = slices.DeleteFunc(ctr.VolumeMounts, func(m corev1.VolumeMount) bool {
			return m.Name == hotplugDevVolume || m.Name == hotplugUdevVolume
		})
		ctr.VolumeMounts = append(ctr.VolumeMounts, corev1.VolumeMount{Name: inputBridgeVolume, MountPath: inputBridgeSocketDir})
		ctr.Args = append(ctr.Args, "--input-bridge-socket="+inputBridgeSocket)
	}
	bridge, err := c.inputBridgeContainer(session)
	if err != nil {
		return nil, nil, err
	}
	wolf.Spec.Containers = append(wolf.Spec.Containers, bridge)
	wolf.Spec.Volumes = append(wolf.Spec.Volumes,
		corev1.Volume{Name: inputBridgeVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		corev1.Volume{Name: appPodSessionsVolume, VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
			Path: c.AppPodHostDir,
			Type: new(corev1.HostPathDirectoryOrCreate),
		}}},
	)

	// Both pods use the same subdirectory of the runtime volume.
	for _, pod := range []*corev1.Pod{wolf, app} {
		for _, ctrs := range [][]corev1.Container{pod.Spec.InitContainers, pod.Spec.Containers} {
			for i := range ctrs {
				for j := range ctrs[i].VolumeMounts {
					if m := &ctrs[i].VolumeMounts[j]; m.Name == wolfRuntimeVolume {
						m.SubPath = runtimeSubPath
					}
				}
			}
		}
	}

	// The App only reads the hotplug directories; wolf-agent and the bridge
	// write them. Read-only is a hint, not the boundary: the App can remount
	// them writable from its user namespace, so the bridge gives them an
	// owner the App can't map (inputbridge.LockDir).
	for _, ctrs := range [][]corev1.Container{app.Spec.InitContainers, app.Spec.Containers} {
		for i := range ctrs {
			for j := range ctrs[i].VolumeMounts {
				m := &ctrs[i].VolumeMounts[j]
				switch m.Name {
				case hotplugDevVolume:
					m.ReadOnly = true
					m.MountPropagation = new(corev1.MountPropagationHostToContainer)
				case hotplugUdevVolume:
					m.ReadOnly = true
				}
			}
		}
	}

	for _, pod := range []*corev1.Pod{wolf, app} {
		pod.Spec.Volumes = slices.DeleteFunc(pod.Spec.Volumes, func(v corev1.Volume) bool { return !podUsesVolume(&pod.Spec, v.Name) })
		shared[wolfRuntimeVolume] = wolfRuntimeVolumeSource()
		if pod == app {
			shared[wolfRuntimeVolume] = appRuntimeVolume(wolfUID)
		}
		for i := range pod.Spec.Volumes {
			if src, ok := shared[pod.Spec.Volumes[i].Name]; ok {
				pod.Spec.Volumes[i].VolumeSource = src
			}
		}
		pod.Spec.ResourceClaims = slices.DeleteFunc(pod.Spec.ResourceClaims, func(rc corev1.PodResourceClaim) bool { return !podUsesClaim(&pod.Spec, rc.Name) })
		pod.Labels = maps.Clone(pod.Labels)
		pod.Labels[sessionUIDLabel] = string(session.UID)
	}
	// A generated claim is per pod: Wolf would get a card of its own.
	for _, rc := range wolf.Spec.ResourceClaims {
		if rc.ResourceClaimTemplateName != nil && podUsesClaim(&app.Spec, rc.Name) {
			return nil, nil, invalidSpec(fmt.Errorf("resourceClaim %q comes from a template, which the App pod and Wolf pod can't share; use spec.gpu", rc.Name))
		}
	}

	wolf.Labels[podRoleLabel] = podRoleWolf
	wolf.Annotations[appPodAnnotation] = c.appPodName(session)

	app.Name = c.appPodName(session)
	app.Labels[podRoleLabel] = podRoleApp
	delete(app.Annotations, portBlockAnnotation)
	app.Spec.Containers = append(app.Spec.Containers, c.udevRelayContainer())
	app.Spec.Volumes = append(app.Spec.Volumes, corev1.Volume{Name: udevEventsVolume, VolumeSource: corev1.VolumeSource{
		HostPath: &corev1.HostPathVolumeSource{Path: c.sessionHostDir(session, "events"), Type: new(corev1.HostPathDirectoryOrCreate)},
	}})
	app.Spec.HostUsers = new(false)
	app.Spec.HostNetwork = false
	app.Spec.HostIPC = false
	if app.Spec.DNSPolicy == corev1.DNSClusterFirstWithHostNet {
		app.Spec.DNSPolicy = corev1.DNSClusterFirst
	}
	// Its volumes are on the Wolf pod's node; without a GPU claim nothing
	// else holds it there.
	if app.Spec.Affinity == nil {
		app.Spec.Affinity = &corev1.Affinity{}
	}
	if app.Spec.Affinity.PodAffinity == nil {
		app.Spec.Affinity.PodAffinity = &corev1.PodAffinity{}
	}
	pa := app.Spec.Affinity.PodAffinity
	pa.RequiredDuringSchedulingIgnoredDuringExecution = append(pa.RequiredDuringSchedulingIgnoredDuringExecution, corev1.PodAffinityTerm{
		LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
			sessionUIDLabel: string(session.UID),
			podRoleLabel:    podRoleWolf,
		}},
		TopologyKey: corev1.LabelHostname,
	})
	return wolf, app, nil
}

// wolfRuntimeVolumeSource is the Wolf pod's runtime directory: in memory and
// size-limited, as on the node's disk (a hostPath) the App could fill /var,
// whose writes kubelet counts toward no pod's ephemeral-storage, and get
// every pod on the node evicted (XERK-2177). Its pages are charged to the
// writer's memory cgroup. Kubelet unmounts it with the Wolf pod, however
// that pod ends.
func wolfRuntimeVolumeSource() corev1.VolumeSource {
	return corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
		Medium:    corev1.StorageMediumMemory,
		SizeLimit: new(resource.MustParse(runtimeSizeLimit)),
	}}
}

// appRuntimeVolume is the App pod's runtime directory: the Wolf pod's own,
// through kubelet's directory for that pod's emptyDir on the shared node.
// Directory, not DirectoryOrCreate, and mounted by runtimeSubPath: the App
// must never get a directory on the node's disk. Once the Wolf pod is gone,
// the runtime recreates a missing hostPath on the node's disk for each new
// container, but kubelet refuses a container whose subPath's volume is
// missing (both measured on talos04).
func appRuntimeVolume(wolfUID types.UID) corev1.VolumeSource {
	return corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
		Path: path.Join(kubeletPodsDir, string(wolfUID), "volumes/kubernetes.io~empty-dir", wolfRuntimeVolume),
		Type: new(corev1.HostPathDirectory),
	}}
}

// udevRelayContainer is the App pod's sidecar announcing hotplugged devices
// there (pkg/inputbridge events.go): the App pod's network namespace is its
// own, so the node's udevd isn't heard, and Steam's Big Picture missed every
// pad plugged after it started. NET_ADMIN, which sending to the udev group
// needs, is the pod user namespace's: it covers only the pod's own network.
func (c *SessionController) udevRelayContainer() corev1.Container {
	return corev1.Container{
		Name:            v1alpha1types.UdevRelayContainerName,
		Image:           c.WolfAgentImage,
		ImagePullPolicy: corev1.PullAlways,
		Args:            []string{"udev-relay", "--events-socket=" + path.Join(udevRelayEventsDir, udevEventsSocket)},
		Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("10m"),
			corev1.ResourceMemory: resource.MustParse("16Mi"),
		}},
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:                new(int64(0)),
			AllowPrivilegeEscalation: new(false),
			ReadOnlyRootFilesystem:   new(true),
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{allCapabilities},
				Add:  []corev1.Capability{"NET_ADMIN"},
			},
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		VolumeMounts: []corev1.VolumeMount{{Name: udevEventsVolume, MountPath: udevRelayEventsDir}},
	}
}

// inputBridgeContainer is the Wolf pod's privileged sidecar publishing device
// nodes into the App pod (pkg/inputbridge). Privileged for the Bidirectional
// mount and mount(2); it talks only to wolf-agent, over inputBridgeVolume.
//
// On start it also sweeps the directories of Sessions that are gone
// (inputbridge.Sweep, XERK-2178): those of no Session listed here, last
// changed sweepMargin before this one was created. A Session created later
// isn't listed, but its directory, made for a pod built after this one, is
// newer than that; the margin absorbs clock skew between the API server and
// the node.
func (c *SessionController) inputBridgeContainer(session *v1alpha1types.Session) (corev1.Container, error) {
	sessions, err := c.SessionInformer.List(labels.Everything())
	if err != nil {
		return corev1.Container{}, fmt.Errorf("failed to list sessions: %w", err)
	}
	live := []string{string(session.UID)}
	for _, s := range sessions {
		if s.UID != session.UID {
			live = append(live, string(s.UID))
		}
	}
	slices.Sort(live[1:])
	args := []string{
		"input-bridge",
		"--socket=" + inputBridgeSocket,
		"--session-dir=" + path.Join(inputBridgeSessionsPath, string(session.UID)),
		"--events-socket=" + path.Join(inputBridgeSessionsPath, string(session.UID), "events", udevEventsSocket),
		"--sweep-before=" + session.CreationTimestamp.Add(-sweepMargin).UTC().Format(time.RFC3339),
	}
	// One flag per UID: Linux caps a single argument at 128 KiB
	// (MAX_ARG_STRLEN), about 3,500 UIDs joined, past which the exec fails.
	// A new name, not repeated --live-sessions: an older bridge keeps only
	// the last of those and would sweep the others' live directories.
	for _, uid := range live {
		args = append(args, "--live-session="+uid)
	}
	return corev1.Container{
		Name:            v1alpha1types.InputBridgeContainerName,
		Image:           c.WolfAgentImage,
		ImagePullPolicy: corev1.PullAlways,
		Args:            args,
		Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("10m"),
			corev1.ResourceMemory: resource.MustParse("32Mi"),
		}},
		SecurityContext: &corev1.SecurityContext{Privileged: new(true)},
		VolumeMounts: []corev1.VolumeMount{
			{Name: inputBridgeVolume, MountPath: inputBridgeSocketDir},
			{
				Name:             appPodSessionsVolume,
				MountPath:        inputBridgeSessionsPath,
				MountPropagation: new(corev1.MountPropagationBidirectional),
			},
		},
	}, nil
}

// podUsesVolume reports whether a container of spec (init too) mounts or
// attaches the volume name.
func podUsesVolume(spec *corev1.PodSpec, name string) bool {
	for _, ctrs := range [][]corev1.Container{spec.InitContainers, spec.Containers} {
		for _, ctr := range ctrs {
			if slices.ContainsFunc(ctr.VolumeMounts, func(m corev1.VolumeMount) bool { return m.Name == name }) ||
				slices.ContainsFunc(ctr.VolumeDevices, func(d corev1.VolumeDevice) bool { return d.Name == name }) {
				return true
			}
		}
	}
	return false
}

// podUsesClaim reports whether a container of spec (init too) uses the pod
// resource claim name.
func podUsesClaim(spec *corev1.PodSpec, name string) bool {
	for _, ctrs := range [][]corev1.Container{spec.InitContainers, spec.Containers} {
		for _, ctr := range ctrs {
			if slices.ContainsFunc(ctr.Resources.Claims, func(rc corev1.ResourceClaim) bool { return rc.Name == name }) {
				return true
			}
		}
	}
	return false
}

// buildPods renders the session's pods: the one session pod, or with
// AppPodHostDir the Wolf pod and the App pod (see splitAppPod for wolfUID).
func (c *SessionController) buildPods(session *v1alpha1types.Session, wolfUID types.UID) (wolf, app *corev1.Pod, err error) {
	pod, err := c.buildPod(session)
	if err != nil || c.AppPodHostDir == "" {
		return pod, nil, err
	}
	wolf, app, err = c.splitAppPod(session, pod, wolfUID)
	if err != nil {
		return nil, nil, err
	}
	// The split adds volumes of its own, which an App's can collide with.
	for _, p := range []*corev1.Pod{wolf, app} {
		if err := validateBuiltPod(&p.Spec, p.Labels); err != nil {
			return nil, nil, invalidSpec(err)
		}
	}
	return wolf, app, nil
}

// reconcileAppPod creates or checks the App pod of a split Session, whose
// Wolf pod is wolfPod. Like the Wolf pod it is never recreated: one gone, or
// with an exited container, ends the Session.
func (c *SessionController) reconcileAppPod(ctx context.Context, session *v1alpha1types.Session, wolfPod *corev1.Pod) error {
	name := wolfPod.Annotations[appPodAnnotation]
	if name == "" {
		return nil
	}
	existing, err := c.podController.Informer().Namespaced(session.Namespace).Get(name)
	switch {
	case err == nil:
		_, err = c.checkPodContainers(ctx, session, existing)
		return err
	case !errors.IsNotFound(err):
		return fmt.Errorf("failed to get pod %s/%s: %w", session.Namespace, name, err)
	case meta.IsStatusConditionTrue(session.Status.Conditions, appPodCreatedCondition):
		live, getErr := c.K8sClient.CoreV1().Pods(session.Namespace).Get(ctx, name, metav1.GetOptions{})
		if errors.IsNotFound(getErr) {
			return c.endSessionErr(ctx, session, "its app pod was deleted")
		} else if getErr != nil {
			return fmt.Errorf("failed to get pod %s/%s: %w", session.Namespace, name, getErr)
		}
		_, err = c.checkPodContainers(ctx, session, live)
		return err
	}
	// Kubelet sets up the Wolf pod's runtime emptyDir before its init
	// containers; until then the App pod's mount of it would fail and back
	// off. The Wolf pod's status update requeues the Session.
	if !podConditionTrue(wolfPod, corev1.PodInitialized) {
		return nil
	}

	_, pod, err := c.buildPods(session, wolfPod.UID)
	switch {
	case stderrors.Is(err, errInvalidSpec):
		if staleErr := c.specCacheCurrent(ctx, session); staleErr != nil {
			return staleErr
		}
		c.warnApp(session, "InvalidSpec", err.Error())
		return c.endSessionErr(ctx, session, err.Error())
	case err != nil:
		return err
	case pod == nil || pod.Name != name:
		// AppPodHostDir was cleared (or the naming changed) since the
		// Wolf pod was made; this operator can no longer build its App pod.
		return c.endSessionErr(ctx, session, "its app pod can no longer be built (--app-pod-host-dir unset)")
	}
	_, err = c.K8sClient.CoreV1().Pods(session.Namespace).Create(ctx, pod, metav1.CreateOptions{
		FieldManager: "direwolf-session-controller-pod",
	})
	switch {
	case errors.IsAlreadyExists(err):
		live, getErr := c.K8sClient.CoreV1().Pods(session.Namespace).Get(ctx, name, metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("failed to get pod %s/%s: %w", session.Namespace, name, getErr)
		}
		if _, checkErr := c.checkPodContainers(ctx, session, live); checkErr != nil {
			return checkErr
		}
	case errors.IsInvalid(err):
		reason := fmt.Sprintf("its app pod is invalid (check its App, User and namespace LimitRanges): %v", err)
		c.warnApp(session, "InvalidPod", reason)
		return c.endSessionErr(ctx, session, reason)
	case err != nil:
		return fmt.Errorf("failed to create pod: %w", err)
	}
	meta.SetStatusCondition(&session.Status.Conditions, metav1.Condition{
		Type:   appPodCreatedCondition,
		Status: metav1.ConditionTrue,
		Reason: "Created",
	})
	return nil
}

// appPodReady reports, for a split Session, whether its App pod runs: the
// stream waits for it as it waits for the game in a single session pod.
func (c *SessionController) appPodReady(wolfPod *corev1.Pod) error {
	name := wolfPod.Annotations[appPodAnnotation]
	if name == "" {
		return nil
	}
	pod, err := c.podController.Informer().Namespaced(wolfPod.Namespace).Get(name)
	if err != nil {
		return fmt.Errorf("app pod %s/%s: %w", wolfPod.Namespace, name, err)
	}
	if pod.Status.Phase != corev1.PodRunning || !podReady(pod) {
		return fmt.Errorf("app pod %s/%s not ready (phase %s)", pod.Namespace, pod.Name, pod.Status.Phase)
	}
	return nil
}
