package controllers

import (
	"fmt"
	"slices"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/klog/v2"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
)

// allocateVRPort records a VR port in the session's status for an App with
// spec.vrPort, or re-claims the one it has (see vrPortEnv).
func (c *SessionController) allocateVRPort(session *v1alpha1types.Session) error {
	owner := c.portOwner(session)
	if vr := session.Status.Ports.VR; vr != 0 {
		if c.vrPorts == nil {
			// The operator lost --vr-port-range; a pod not yet built is
			// refused by checkVRPort, a running one keeps its port.
			return nil
		}
		err := c.vrPorts.ClaimBase(owner, vr)
		if err == nil {
			return nil
		}
		// A pod built with the old port is ended by vrPortMoved.
		klog.Warningf("Session %s/%s: re-allocating VR port: %v", session.Namespace, session.Name, err)
	} else {
		app, err := c.AppInformer.Namespaced(session.Namespace).Get(session.Spec.GameReference.Name)
		switch {
		case errors.IsNotFound(err):
			return nil // appGoneReason or buildPod deal with it
		case err != nil:
			return fmt.Errorf("failed to get app: %w", err)
		case !app.Spec.VRPort || c.vrPorts == nil:
			return nil // checkVRPort refuses the latter
		case meta.IsStatusConditionTrue(session.Status.Conditions, podCreatedCondition):
			// The App asked for a port after the pod was built: holding one
			// for a pod that never serves it would only drain the range.
			return nil
		}
	}
	vr, err := c.vrPorts.AllocateBase(owner)
	if err != nil {
		return fmt.Errorf("VR port: %w", err)
	}
	session.Status.Ports.VR = vr
	return nil
}

// checkVRPort refuses an App's spec.vrPort this operator can't serve.
func (c *SessionController) checkVRPort(app *v1alpha1types.App, session *v1alpha1types.Session) error {
	switch {
	case !app.Spec.VRPort:
		return nil
	case !app.Spec.RequireAppPod:
		// The CRD refuses this too; a VR server must not sit on the
		// host-networked session pod.
		return fmt.Errorf("app %s sets vrPort without requireAppPod", app.Name)
	case c.vrPorts == nil:
		return fmt.Errorf("app %s sets vrPort, but the operator runs without --vr-port-range", app.Name)
	case session.Status.Ports.VR == 0:
		return fmt.Errorf("app %s asked for a VR port after this Session's pod was created; start a new Session", app.Name)
	}
	return nil
}

// withVRPort hands port (0: none) to the App's containers in spec: as
// DIREWOLF_VR_PORT in each, and as TCP and UDP hostPorts on the first, so the
// scheduler and the node's portmap reserve it. spec must hold only the App's
// containers.
func withVRPort(spec *corev1.PodSpec, port int32) {
	if port == 0 {
		return
	}
	env := corev1.EnvVar{Name: vrPortEnv, Value: strconv.Itoa(int(port))}
	for _, ctrs := range [][]corev1.Container{spec.InitContainers, spec.Containers} {
		for i := range ctrs {
			// First, so the App's own variables can refer to it as
			// $(DIREWOLF_VR_PORT); any value of its own is replaced.
			ctrs[i].Env = append([]corev1.EnvVar{env}, slices.DeleteFunc(slices.Clone(ctrs[i].Env), func(e corev1.EnvVar) bool {
				return e.Name == vrPortEnv
			})...)
		}
	}
	if len(spec.Containers) == 0 {
		return
	}
	spec.Containers[0].Ports = append(slices.Clone(spec.Containers[0].Ports),
		corev1.ContainerPort{Name: "vr-tcp", ContainerPort: port, HostPort: port, Protocol: corev1.ProtocolTCP},
		corev1.ContainerPort{Name: "vr-udp", ContainerPort: port, HostPort: port, Protocol: corev1.ProtocolUDP},
	)
}

// vrPortMoved reports whether pod was built with a VR port other than the
// session's: its port was re-allocated (e.g. lost across an operator restart
// with a changed range), so the pod no longer serves the one advertised.
func vrPortMoved(pod *corev1.Pod, session *v1alpha1types.Session) bool {
	built, ok := pod.Annotations[vrPortAnnotation]
	return ok && built != strconv.Itoa(int(session.Status.Ports.VR))
}
