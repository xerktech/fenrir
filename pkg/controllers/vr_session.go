package controllers

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path"
	"reflect"
	"slices"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/klog/v2"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	"games-on-whales.github.io/direwolf/pkg/wolfapi"
)

// A native VR Session (spec.mode VR, XERK-2667; docs/vr-native-session.md)
// runs one pod: the App's containers and the vr-agent sidecar. No Wolf,
// wolf-agent, pulseaudio or port block. Like the split App pod it is
// user-namespaced and off the host network (Steam's bwrap; CUDA's abstract
// socket, XERK-2692), with the VR port as its only hostPort.
//
// Its liveness is the headset: vr-agent reads WiVRn's HeadsetConnected and
// SessionRunning over the App's session bus, which the image puts on
// vrBusVolume in native mode, and serves them to the operator on the pod IP
// behind the Session's token and pinned cert (agent_token.go).
const (
	// VRStatusPath is vr-agent's status endpoint.
	VRStatusPath = "/agent/v1/vr-status"
	// VRAgentPort is vr-agent's port, in the pod's own network namespace
	// (never a hostPort): the operator dials the pod IP.
	VRAgentPort int32 = 9443
	// vrBusVolume holds the App's D-Bus session bus socket, shared with
	// vr-agent only. Tiny and in memory: it holds one socket.
	vrBusVolume = "direwolf-vr-bus"
	// VRBusDir is where vrBusVolume is mounted, in the App and vr-agent;
	// the vr image's native bus config listens at VRBusAddress.
	VRBusDir     = "/run/direwolf-vr-bus"
	VRBusAddress = "unix:path=" + VRBusDir + "/bus"
	// vrModeEnv tells the vr image to run without Wolf: its own headless
	// sway, and its session bus on VRBusAddress.
	vrModeEnv    = "DIREWOLF_VR_MODE"
	vrModeNative = "native"
	// vrAgentUID is the desktop user (GoW's PUID default): D-Bus's EXTERNAL
	// auth lets only the bus owner's uid connect.
	vrAgentUID int64 = 1000
	// vrUnstartedTTL reaps a VR Session whose pod never became ready. Longer
	// than unstartedSessionTTL: no Moonlight launch is waiting, and the vr
	// image is several GB to pull.
	vrUnstartedTTL = 15 * time.Minute
	// HeadsetConnected reasons.
	vrWaitingForHeadset = "WaitingForHeadset"
	vrHeadsetConnected  = "Connected"
	vrHeadsetGone       = "Disconnected"
	vrAgentLost         = "AgentUnreachable"
	vrConditionSuccess  = "Success"
	// vrStatusMaxBytes caps vr-agent's reply: room for a few dozen keys.
	vrStatusMaxBytes = 64 << 10
	// vrAgentTokenVolume is the Session's token Secret, as buildPod mounts it.
	vrAgentTokenVolume = "wolf-agent-token"
	// vrAgentOwnTokenVolume is a split App pod's vr-agent token Secret
	// (vrAgentTokenSecretName).
	vrAgentOwnTokenVolume = "vr-agent-token"
)

// VRStatus is vr-agent's reply on VRStatusPath: WiVRn's properties.
type VRStatus struct {
	HeadsetConnected bool `json:"headsetConnected"`
	SessionRunning   bool `json:"sessionRunning"`
	// KnownKeys are the keys WiVRn lets connect: those it was seeded with,
	// plus any a PIN pairing (or the game) added (trackPairedHeadsets).
	KnownKeys []VRKnownKey `json:"knownKeys,omitempty"`
}

// VRKnownKey is one entry of WiVRn's KnownKeys: the name it shows and the
// key in its stored form (HeadsetSpec.PublicKey).
type VRKnownKey struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

// vrUnstartedReason is why a VR Session never got a ready pod, or "".
// HeadsetConnected is first set when its pod goes ready.
func vrUnstartedReason(session *v1alpha1types.Session, now time.Time) string {
	if meta.FindStatusCondition(session.Status.Conditions, v1alpha1types.SessionHeadsetConnectedCondition) != nil ||
		!session.CreationTimestamp.Add(vrUnstartedTTL).Before(now) {
		return ""
	}
	return fmt.Sprintf("older than %s and its VR pod never became ready", vrUnstartedTTL)
}

// buildVRPod renders a VR Session's pod from buildPod's: the App's
// containers keep everything buildPod gave them and every check it ran;
// Wolf's containers and volumes go.
func (c *SessionController) buildVRPod(session *v1alpha1types.Session) (*corev1.Pod, error) {
	pod, err := c.buildPod(session)
	if err != nil {
		return nil, err
	}
	spec := &pod.Spec
	spec.InitContainers = slices.DeleteFunc(spec.InitContainers, func(c corev1.Container) bool { return isWolfPodContainer(c.Name) })
	spec.Containers = slices.DeleteFunc(spec.Containers, func(c corev1.Container) bool { return isWolfPodContainer(c.Name) })
	for i := range spec.Containers {
		ctr := &spec.Containers[i]
		// No wolf-agent publishes input nodes: Quest controllers reach the
		// game through OpenXR.
		ctr.VolumeMounts = slices.DeleteFunc(ctr.VolumeMounts, func(m corev1.VolumeMount) bool {
			return m.Name == hotplugDevVolume || m.Name == hotplugUdevVolume
		})
		ctr.VolumeMounts = append(ctr.VolumeMounts, corev1.VolumeMount{Name: vrBusVolume, MountPath: VRBusDir})
		ctr.Env = append(slices.DeleteFunc(ctr.Env, func(e corev1.EnvVar) bool { return e.Name == vrModeEnv }),
			corev1.EnvVar{Name: vrModeEnv, Value: vrModeNative})
	}
	spec.Containers = append(spec.Containers, c.vrAgentContainer(true, vrAgentTokenVolume))
	spec.Volumes = slices.DeleteFunc(spec.Volumes, func(v corev1.Volume) bool { return !podUsesVolume(spec, v.Name) })
	for i := range spec.Volumes {
		if spec.Volumes[i].Name == wolfRuntimeVolume {
			// XDG_RUNTIME_DIR, the pod's own (no Wolf pod to share).
			spec.Volumes[i].VolumeSource = wolfRuntimeVolumeSource()
		}
	}
	spec.Volumes = append(spec.Volumes, vrBusVolumeSpec())
	spec.ResourceClaims = slices.DeleteFunc(spec.ResourceClaims, func(rc corev1.PodResourceClaim) bool { return !podUsesClaim(spec, rc.Name) })
	spec.HostUsers = new(false)
	spec.HostNetwork = false
	spec.HostIPC = false
	if spec.DNSPolicy == corev1.DNSClusterFirstWithHostNet {
		spec.DNSPolicy = corev1.DNSClusterFirst
	}
	delete(pod.Annotations, portBlockAnnotation)
	pod.Labels[v1alpha1types.SessionModeLabel] = v1alpha1types.SessionModeVR
	if err := validateBuiltPod(spec, pod.Labels); err != nil {
		return nil, invalidSpec(err)
	}
	return pod, nil
}

// vrBusVolumeSpec is vrBusVolume: tiny and in memory, it holds one socket.
func vrBusVolumeSpec() corev1.Volume {
	return corev1.Volume{Name: vrBusVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
		Medium:    corev1.StorageMediumMemory,
		SizeLimit: new(resource.MustParse("1Mi")),
	}}}
}

// withVRAgent adds vr-agent to a Moonlight Session's split App pod of a VR
// App (XERK-2740), so a PIN pairing there is offered for confirmation too:
// the App's containers get the bus directory (the image's session bus then
// listens there) and vr-agent its own token Secret, never wolf-agent's (that
// one drives Wolf). Call it after the App pod's volumes are pruned.
func (c *SessionController) withVRAgent(session *v1alpha1types.Session, spec *corev1.PodSpec) {
	for i := range spec.Containers {
		if ctr := &spec.Containers[i]; ctr.Name != v1alpha1types.UdevRelayContainerName {
			ctr.VolumeMounts = append(ctr.VolumeMounts, corev1.VolumeMount{Name: vrBusVolume, MountPath: VRBusDir})
		}
	}
	spec.Containers = append(spec.Containers, c.vrAgentContainer(false, vrAgentOwnTokenVolume))
	spec.Volumes = append(spec.Volumes, vrBusVolumeSpec(), corev1.Volume{
		Name: vrAgentOwnTokenVolume,
		// Created by reconcileAppPod before the pod.
		VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: vrAgentTokenSecretName(session.Name)}},
	})
}

// vrAgentContainer reads WiVRn's state from the App's session bus. It runs as
// the desktop user (the bus refuses other uids) with nothing else: the token
// and key are its alone, and the App never mounts them.
//
// gateReady makes it the pod's readiness (native mode: the headset wait
// starts once WiVRn answers). Never in a split App pod: the stream waits for
// that pod to be ready, and the App waits for the stream's display before
// it starts WiVRn.
func (c *SessionController) vrAgentContainer(gateReady bool, tokenVolume string) corev1.Container {
	probe := func(p string) *corev1.Probe {
		return &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Path: p, Port: intstr.FromInt32(VRAgentPort), Scheme: corev1.URISchemeHTTPS,
		}}}
	}
	var readiness *corev1.Probe
	if gateReady {
		readiness = probe("/readyz")
	}
	return corev1.Container{
		Name:            v1alpha1types.VRAgentContainerName,
		Image:           c.WolfAgentImage,
		ImagePullPolicy: corev1.PullAlways,
		Args: []string{
			"vr-agent",
			fmt.Sprintf("--port=%d", VRAgentPort),
			"--bus=" + VRBusAddress,
			"--token-file=" + path.Join(wolfAgentTokenMountPath, wolfAgentTokenKey),
			"--tls-cert=" + path.Join(wolfAgentTokenMountPath, corev1.TLSCertKey),
			"--tls-key=" + path.Join(wolfAgentTokenMountPath, corev1.TLSPrivateKeyKey),
		},
		Ports: []corev1.ContainerPort{{Name: "vr-agent", ContainerPort: VRAgentPort, Protocol: corev1.ProtocolTCP}},
		// Ready once WiVRn answers on the bus: the headset wait (and its
		// grace period) starts then.
		ReadinessProbe: readiness,
		LivenessProbe:  probe("/livez"),
		Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("10m"),
			corev1.ResourceMemory: resource.MustParse("32Mi"),
		}},
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:                new(vrAgentUID),
			RunAsGroup:               new(vrAgentUID),
			RunAsNonRoot:             new(true),
			AllowPrivilegeEscalation: new(false),
			ReadOnlyRootFilesystem:   new(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{allCapabilities}},
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: tokenVolume, MountPath: wolfAgentTokenMountPath, ReadOnly: true},
			{Name: vrBusVolume, MountPath: VRBusDir},
		},
	}
}

// finishVRReconcile is the rest of Reconcile for a VR Session once its pod is
// reconciled: track the headset, write status, and poll again.
func (c *SessionController) finishVRReconcile(ctx context.Context, session *v1alpha1types.Session, oldStatus *v1alpha1types.SessionStatus, pod *corev1.Pod, podError error) error {
	switch {
	case stderrors.Is(podError, errSessionEnded):
		return nil
	case stderrors.Is(podError, errEndSessionFailed):
		return podError
	case podError != nil:
		klog.Errorf("Failed to reconcile pod: %s", podError)
		meta.SetStatusCondition(&session.Status.Conditions, metav1.Condition{
			Type: podCreatedCondition, Status: metav1.ConditionFalse, Reason: "PodCreationFailed", Message: podError.Error(),
		})
	default:
		meta.SetStatusCondition(&session.Status.Conditions, metav1.Condition{
			Type: podCreatedCondition, Status: metav1.ConditionTrue, Reason: vrConditionSuccess,
		})
	}

	// Polled from the first time the pod is ready; after that also while it
	// is not, since vr-agent's readiness is WiVRn answering.
	started := meta.FindStatusCondition(session.Status.Conditions, v1alpha1types.SessionHeadsetConnectedCondition) != nil
	ready := podError == nil && pod.Status.Phase == corev1.PodRunning && pod.Status.PodIP != "" && (started || podReady(pod))
	if ready {
		pollErr := c.reconcileVRHeadset(ctx, session, pod)
		switch {
		case stderrors.Is(pollErr, errSessionEnded):
			return nil
		case stderrors.Is(pollErr, errEndSessionFailed):
			return pollErr
		case pollErr != nil:
			// A headset we can't see is gone: the grace period runs, and an
			// answer showing it connected stops it. Nothing else would end a
			// Session whose bus or WiVRn hung (the game can move the bus).
			klog.Warningf("Session %s/%s: %v", session.Namespace, session.Name, pollErr)
			lostVRAgent(session, pollErr)
		}
	}

	if !reflect.DeepEqual(&session.Status, oldStatus) {
		if err := c.writeStatus(ctx, session, oldStatus); err != nil && !errors.IsNotFound(err) {
			return err
		}
	}
	switch {
	case podError != nil:
		return podError // retried with backoff, as in Moonlight mode
	case !ready:
		c.controller.EnqueueAfter(session.Namespace, session.Name, podNotReadyRecheck)
	default:
		next := streamPollInterval
		if d := session.Status.DisconnectedAt; d != nil {
			next = min(next, max(time.Until(d.Add(c.DisconnectGracePeriod)), 0))
		}
		c.controller.EnqueueAfter(session.Namespace, session.Name, next)
	}
	return nil
}

// reconcileVRHeadset records the headset's state from vr-agent in session's
// status. The first time the pod is ready the Session starts waiting for a
// headset: disconnectedAt is set, so one that never comes is ended after the
// grace period like one that left.
func (c *SessionController) reconcileVRHeadset(ctx context.Context, session *v1alpha1types.Session, pod *corev1.Pod) error {
	status := &session.Status
	if meta.FindStatusCondition(status.Conditions, v1alpha1types.SessionHeadsetConnectedCondition) == nil {
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{
			Type: v1alpha1types.SessionHeadsetConnectedCondition, Status: metav1.ConditionFalse, Reason: vrWaitingForHeadset,
			Message: "waiting for a headset to connect",
		})
		if status.DisconnectedAt == nil {
			status.DisconnectedAt = new(metav1.Now())
		}
	}

	vr, err := c.pollVRAgent(ctx, session, pod.Status.PodIP, agentTokenSecretName(session.Name))
	if err != nil {
		return err
	}
	// A failure costs only a PIN next session; it never ends this one.
	if err := c.trackPairedHeadsets(ctx, session, pod, vr.KnownKeys); err != nil {
		klog.Warningf("Session %s/%s: %v", session.Namespace, session.Name, err)
	}
	headset := meta.FindStatusCondition(status.Conditions, v1alpha1types.SessionHeadsetConnectedCondition)
	switch {
	case vr.HeadsetConnected:
		if headset.Status != metav1.ConditionTrue {
			klog.Infof("Session %s/%s: headset connected", session.Namespace, session.Name)
		}
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{
			Type: v1alpha1types.SessionHeadsetConnectedCondition, Status: metav1.ConditionTrue, Reason: vrHeadsetConnected,
		})
		status.DisconnectedAt = nil
	case headset.Status == metav1.ConditionTrue:
		klog.Infof("Session %s/%s: headset disconnected, keeping the pod for %s", session.Namespace, session.Name, c.DisconnectGracePeriod)
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{
			Type: v1alpha1types.SessionHeadsetConnectedCondition, Status: metav1.ConditionFalse, Reason: vrHeadsetGone,
			Message: "the headset disconnected",
		})
		if status.DisconnectedAt == nil {
			status.DisconnectedAt = new(metav1.Now())
		}
	}
	stream := metav1.Condition{Type: v1alpha1types.SessionStreamStartedCondition, Status: metav1.ConditionFalse, Reason: "NotStreaming"}
	if vr.SessionRunning {
		stream.Status, stream.Reason = metav1.ConditionTrue, "Streaming"
	}
	meta.SetStatusCondition(&status.Conditions, stream)
	return nil
}

// lostVRAgent records that vr-agent could not say whether a headset is
// connected: as a disconnect, so the grace period ends a Session that stays
// unreadable.
func lostVRAgent(session *v1alpha1types.Session, err error) {
	status := &session.Status
	if meta.IsStatusConditionTrue(status.Conditions, v1alpha1types.SessionHeadsetConnectedCondition) {
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{
			Type: v1alpha1types.SessionHeadsetConnectedCondition, Status: metav1.ConditionFalse, Reason: vrAgentLost,
			Message: "vr-agent did not answer: " + err.Error(),
		})
	}
	if status.DisconnectedAt == nil {
		status.DisconnectedAt = new(metav1.Now())
	}
}

// pollVRAgent asks the pod's vr-agent for the headset's state, pinned to the
// Session's cert: a stranger in the pod's network namespace (the App can bind
// the port first) fails the handshake before it sees the token.
func (c *SessionController) pollVRAgent(ctx context.Context, session *v1alpha1types.Session, podIP, secret string) (VRStatus, error) {
	token, tlsConfig, err := c.tokenCredentials(ctx, session, secret)
	if stderrors.Is(err, errAgentCertMissing) {
		return VRStatus{}, c.endSessionErr(ctx, session, err.Error())
	} else if err != nil {
		return VRStatus{}, err
	}
	transport := &http.Transport{TLSClientConfig: tlsConfig}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Timeout:   wolfAgentTimeout,
		Transport: &wolfapi.BearerTokenTransport{Token: wolfapi.StaticToken(token), Base: transport},
	}
	url := "https://" + net.JoinHostPort(podIP, strconv.Itoa(int(c.vrAgentPort))) + VRStatusPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return VRStatus{}, fmt.Errorf("polling vr-agent: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return VRStatus{}, fmt.Errorf("polling vr-agent: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return VRStatus{}, fmt.Errorf("polling vr-agent: %s", resp.Status)
	}
	var vr VRStatus
	if err := json.NewDecoder(io.LimitReader(resp.Body, vrStatusMaxBytes)).Decode(&vr); err != nil {
		return VRStatus{}, fmt.Errorf("polling vr-agent: %w", err)
	}
	return vr, nil
}

// trackAppPodPairing follows a PIN pairing in a Moonlight Session's split
// App pod (XERK-2740), as reconcileVRHeadset does in native mode. Polled only
// while that pod paired by PIN; a failure is logged, never the stream's
// concern.
func (c *SessionController) trackAppPodPairing(ctx context.Context, session *v1alpha1types.Session, wolfPod *corev1.Pod) {
	name := wolfPod.Annotations[appPodAnnotation]
	if name == "" || c.HeadsetClient == nil {
		return
	}
	pod, err := c.podController.Informer().Namespaced(wolfPod.Namespace).Get(name)
	if err != nil || !pairingWasOpen(pod) || pod.Status.Phase != corev1.PodRunning || pod.Status.PodIP == "" ||
		!slices.ContainsFunc(pod.Spec.Containers, func(c corev1.Container) bool { return c.Name == v1alpha1types.VRAgentContainerName }) {
		return // an App pod from before XERK-2740 has no vr-agent
	}
	vr, err := c.pollVRAgent(ctx, session, pod.Status.PodIP, vrAgentTokenSecretName(session.Name))
	if err == nil {
		err = c.trackPairedHeadsets(ctx, session, pod, vr.KnownKeys)
	}
	if err != nil {
		klog.Warningf("Session %s/%s: recording a paired headset: %v", session.Namespace, session.Name, err)
	}
}
