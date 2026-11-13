package controllers

import (
	"context"
	stderrors "errors"
	"maps"
	"path"
	"slices"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
)

// A VR App's wivrn-server asks the wheel helper (pkg/wheel) for a uinput wheel
// shaped like the one plugged into the headset (XERK-2852, epic XERK-2848).
// The helper runs in the Session's input-bridge, privileged in the host user
// namespace, and publishes each wheel's node and udev entry into the
// Session's input and udev directories as it does Wolf's pads; the App
// container gets only the socket, never a device or capability.
//
//   - Moonlight mode (split App pod): the Wolf pod's bridge serves it.
//   - Native VR: the pod is user-namespaced and has no Wolf pod, so a pod of
//     its own, <session>-wheel, runs the bridge beside it. Like the App pod
//     it is never recreated: gone, or with its container exited, it ends
//     the Session (a node left behind would open whatever device the kernel
//     next gives its minor). The VR pod waits for it (wheel-wait).
const (
	wheelVolume     = "direwolf-wheel"
	wheelSocketName = "wheel.sock"
	// WheelDir is where App containers find the socket; WheelSocketEnv
	// names it for wivrn-server.
	WheelDir       = "/run/direwolf-wheel"
	WheelSocketEnv = "DIREWOLF_WHEEL_SOCKET"
	// On a native VR pod: the name of its wheel helper pod.
	wheelPodAnnotation = "direwolf/wheel-pod"
	// wheelPodCreatedCondition is True once the wheel helper pod has been
	// created; it is never recreated.
	wheelPodCreatedCondition = "WheelPodCreated"
	podRoleVR                = "vr"
	podRoleWheel             = "wheel"
	// wheelWaitTimeout bounds the VR pod's wait for its helper; the pod then
	// fails and the Session ends.
	wheelWaitTimeout = "5m"
	appLabel         = "direwolf/app"
	userLabel        = "direwolf/user"
)

// wheelOn reports whether vrPort Apps get a wheel socket.
func (c *SessionController) wheelOn() bool {
	return c.VRWheel && c.AppPodHostDir != ""
}

func (c *SessionController) wheelPodName(session *v1alpha1types.Session) string {
	return c.podName(session) + "-wheel"
}

// sessionHostDirVolume is a hostPath volume on the Session's sub directory.
func (c *SessionController) sessionHostDirVolume(session *v1alpha1types.Session, name, sub string) corev1.Volume {
	return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
		Path: c.sessionHostDir(session, sub),
		Type: new(corev1.HostPathDirectoryOrCreate),
	}}}
}

// withWheelSocket gives the App's containers the socket's directory,
// read-only (connecting needs no write access to the mount), and its path.
// The bridge locks the directory (inputbridge.LockDir) before it listens, so
// the App can't swap the socket.
func (c *SessionController) withWheelSocket(session *v1alpha1types.Session, spec *corev1.PodSpec) {
	socket := path.Join(WheelDir, wheelSocketName)
	for i := range spec.Containers {
		ctr := &spec.Containers[i]
		if ctr.Name == v1alpha1types.UdevRelayContainerName || ctr.Name == v1alpha1types.VRAgentContainerName {
			continue
		}
		ctr.VolumeMounts = append(ctr.VolumeMounts, corev1.VolumeMount{Name: wheelVolume, MountPath: WheelDir, ReadOnly: true})
		ctr.Env = append(slices.DeleteFunc(ctr.Env, func(e corev1.EnvVar) bool { return e.Name == WheelSocketEnv }),
			corev1.EnvVar{Name: WheelSocketEnv, Value: socket})
	}
	spec.Volumes = append(spec.Volumes, c.sessionHostDirVolume(session, wheelVolume, "wheel"))
}

// wheelBridgeArgs turn on the bridge's wheel server, its socket handed to the
// App's desktop user (wivrn-server's).
func wheelBridgeArgs(session *v1alpha1types.Session, uid, gid int64) []string {
	return []string{
		"--wheel-socket=" + path.Join(inputBridgeSessionsPath, string(session.UID), "wheel", wheelSocketName),
		"--wheel-owner=" + strconv.FormatInt(uid, 10) + ":" + strconv.FormatInt(gid, 10),
	}
}

// withSplitWheel adds the wheel to a split Session's pods: the Wolf pod's
// bridge serves it to the App pod. Call it once the App pod's volumes are
// pruned.
func (c *SessionController) withSplitWheel(session *v1alpha1types.Session, wolf, app *corev1.Pod) {
	uid, gid, err := vrAgentIDs(&app.Spec)
	if err != nil {
		// As for vr-agent: the stream still runs, without a wheel.
		klog.Warningf("Session %s/%s: no wheel socket, the App's user is unclear: %v", session.Namespace, session.Name, err)
		return
	}
	for i := range wolf.Spec.Containers {
		if ctr := &wolf.Spec.Containers[i]; ctr.Name == v1alpha1types.InputBridgeContainerName {
			ctr.Args = append(ctr.Args, wheelBridgeArgs(session, uid, gid)...)
		}
	}
	c.withWheelSocket(session, &app.Spec)
}

// withNativeWheel adds the wheel to a native VR pod: the input and udev
// directories become the Session's host directories, which its wheel helper
// pod's bridge writes, mounted as in a split App pod (read-only; input with
// HostToContainer propagation, so each node arrives as a mount of its own,
// without the pod's nodev); udev-relay announces the wheel in the pod's own
// network namespace; wheel-wait holds the App until the bridge has reset the
// udev directory. Call it before the pod's volumes are pruned, and after
// vrAgentIDs.
func (c *SessionController) withNativeWheel(session *v1alpha1types.Session, pod *corev1.Pod, uid, gid int64) {
	spec := &pod.Spec
	for _, ctrs := range [][]corev1.Container{spec.InitContainers, spec.Containers} {
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
	for i := range spec.Volumes {
		switch spec.Volumes[i].Name {
		case hotplugDevVolume:
			spec.Volumes[i] = c.sessionHostDirVolume(session, hotplugDevVolume, "input")
		case hotplugUdevVolume:
			spec.Volumes[i] = c.sessionHostDirVolume(session, hotplugUdevVolume, "udev")
		}
	}
	c.withWheelSocket(session, spec)
	spec.Containers = append(spec.Containers, c.udevRelayContainer())
	spec.Volumes = append(spec.Volumes, c.sessionHostDirVolume(session, udevEventsVolume, "events"))
	spec.InitContainers = append([]corev1.Container{c.wheelWaitContainer(uid, gid)}, spec.InitContainers...)

	pod.Annotations[wheelPodAnnotation] = c.wheelPodName(session)
	pod.Labels[sessionUIDLabel] = string(session.UID)
	pod.Labels[podRoleLabel] = podRoleVR
}

// wheelWaitContainer is the native VR pod's first init container: it waits
// for the udev control file the bridge creates once it has locked and reset
// the directory. libudev checks for it once, when a monitor is made, so a
// game started earlier would never hear of a wheel.
func (c *SessionController) wheelWaitContainer(uid, gid int64) corev1.Container {
	return corev1.Container{
		Name:            v1alpha1types.WheelWaitContainerName,
		Image:           c.WolfAgentImage,
		ImagePullPolicy: corev1.PullAlways,
		Args:            []string{"wheel-wait", "--file=" + udevControlPath, "--timeout=" + wheelWaitTimeout},
		Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("10m"),
			corev1.ResourceMemory: resource.MustParse("16Mi"),
		}},
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:                new(uid),
			RunAsGroup:               new(gid),
			RunAsNonRoot:             new(true),
			AllowPrivilegeEscalation: new(false),
			ReadOnlyRootFilesystem:   new(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{allCapabilities}},
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		VolumeMounts: []corev1.VolumeMount{{Name: hotplugUdevVolume, MountPath: path.Dir(udevControlPath), ReadOnly: true}},
	}
}

// buildWheelPod renders a native VR Session's wheel helper pod for its VR
// pod vrPod: the input-bridge alone, in the host user namespace, on the VR
// pod's node.
func (c *SessionController) buildWheelPod(session *v1alpha1types.Session, vrPod *corev1.Pod) (*corev1.Pod, error) {
	uid, gid, err := vrAgentIDs(&vrPod.Spec)
	if err != nil {
		return nil, invalidSpec(err)
	}
	bridge, err := c.inputBridgeContainer(session)
	if err != nil {
		return nil, err
	}
	bridge.Args = append(bridge.Args, wheelBridgeArgs(session, uid, gid)...)
	podLabels := map[string]string{}
	// What the pod watch maps back to the Session (reconcileDependant), and
	// what tells Moonlight's pod lists to skip it.
	for _, k := range []string{"app", appLabel, userLabel, v1alpha1types.SessionPodLabel, v1alpha1types.SessionModeLabel} {
		if v, ok := vrPod.Labels[k]; ok {
			podLabels[k] = v
		}
	}
	podLabels[sessionUIDLabel] = string(session.UID)
	podLabels[podRoleLabel] = podRoleWheel
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            c.wheelPodName(session),
			Namespace:       session.Namespace,
			Labels:          podLabels,
			OwnerReferences: slices.Clone(vrPod.OwnerReferences),
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: new(false),
			EnableServiceLinks:           new(false),
			PriorityClassName:            vrPod.Spec.PriorityClassName,
			NodeSelector:                 maps.Clone(c.SessionNodeSelector),
			Tolerations:                  slices.Clone(c.SessionTolerations),
			// Its directories are the VR pod's node's.
			Affinity: &corev1.Affinity{PodAffinity: &corev1.PodAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
					LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
						sessionUIDLabel: string(session.UID),
						podRoleLabel:    podRoleVR,
					}},
					TopologyKey: corev1.LabelHostname,
				}},
			}},
			Containers: []corev1.Container{bridge},
			Volumes: []corev1.Volume{
				// No wolf-agent: nothing else mounts the bridge's request
				// socket.
				{Name: inputBridgeVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				{Name: appPodSessionsVolume, VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
					Path: c.AppPodHostDir,
					Type: new(corev1.HostPathDirectoryOrCreate),
				}}},
			},
		},
	}, nil
}

// reconcileWheelPod creates or checks the wheel helper pod of a native VR
// Session whose pod is vrPod. Like the VR pod it is never recreated: one gone,
// or with its container exited, ends the Session.
func (c *SessionController) reconcileWheelPod(ctx context.Context, session *v1alpha1types.Session, vrPod *corev1.Pod) error {
	name := vrPod.Annotations[wheelPodAnnotation]
	if name == "" {
		return nil
	}
	if found, err := c.checkCompanionPod(ctx, session, name, wheelPodCreatedCondition, "wheel helper pod"); found {
		return err
	}
	pod, err := c.buildWheelPod(session, vrPod)
	switch {
	case stderrors.Is(err, errInvalidSpec):
		c.warnApp(session, "InvalidSpec", err.Error())
		return c.endSessionErr(ctx, session, err.Error())
	case err != nil:
		return err
	}
	return c.createCompanionPod(ctx, session, pod, wheelPodCreatedCondition, "wheel helper pod")
}
