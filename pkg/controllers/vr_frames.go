package controllers

import (
	"fmt"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
)

// vrFrameStallTimeout is how long a native VR Session's game may deliver no
// frame while the headset is focused before the Session ends (XERK-2844).
// When the OpenVR layer crashes it opens a dialog nobody can see
// (OpenComposite's MessageBox, xrizer's X11 panic window) or waits forever on
// a fence, so the game never exits, and WiVRn keeps streaming its last, black
// picture. Generous: a game may stop submitting frames while it loads.
const vrFrameStallTimeout = 2 * time.Minute

// vrFrames is what one native VR Session's frame count last looked like.
type vrFrames struct {
	uid    types.UID
	frames uint64
	// delivered is whether the game has delivered a frame (a count above
	// zero, or one changing) since the Session was first seen.
	delivered bool
	// since is when the count last changed or the headset was last
	// unfocused: only time spent looking at a frozen game counts.
	since time.Time
}

// vrFrameProgress tracks vrFrames per Session ("namespace/name"). In memory:
// an operator restart starts each Session afresh, which only delays a stall's
// detection.
type vrFrameProgress struct {
	mu sync.Mutex
	m  map[string]vrFrames
}

// stalledFor records vr at now and returns how long the focused headset has
// seen no new frame from a game that has delivered some, or 0. WiVRn's count
// restarts at zero with its server, so any change is progress. An image whose
// WiVRn reports no count (AppFramesDelivered nil) is never judged.
func (p *vrFrameProgress) stalledFor(session *v1alpha1types.Session, vr VRStatus, now time.Time) time.Duration {
	if vr.AppFramesDelivered == nil {
		return 0
	}
	frames := *vr.AppFramesDelivered
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil {
		p.m = map[string]vrFrames{}
	}
	key := session.Namespace + "/" + session.Name
	prev, ok := p.m[key]
	if !ok || prev.uid != session.UID {
		p.m[key] = vrFrames{uid: session.UID, frames: frames, delivered: frames != 0, since: now}
		return 0
	}
	if frames != prev.frames {
		prev.delivered, prev.since = true, now
	}
	prev.frames = frames
	if !vr.HeadsetConnected || !vr.HeadsetFocused {
		prev.since = now
	}
	p.m[key] = prev
	if !prev.delivered {
		return 0
	}
	return now.Sub(prev.since)
}

// forget drops a deleted Session's progress.
func (p *vrFrameProgress) forget(namespace, name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.m, namespace+"/"+name)
}

// vrFramesStalledReason is why session should end because its game stopped
// delivering frames while the headset looked at it, or "".
func (c *SessionController) vrFramesStalledReason(session *v1alpha1types.Session, vr VRStatus, now time.Time) string {
	if d := c.vrFrameProgress.stalledFor(session, vr, now); d >= vrFrameStallTimeout {
		return fmt.Sprintf("its game delivered no VR frame for %s while the headset was focused: the game hung", d.Round(time.Second))
	}
	return ""
}
