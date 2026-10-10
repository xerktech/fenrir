package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	"games-on-whales.github.io/direwolf/pkg/conntrack"
)

// VideoPacketsPath is wolf-agent's count of the video packets Wolf has sent
// from the session's video port. Wolf keeps listing a stream whose pipeline
// froze (XERK-1741), and the client keeps pinging it, so this count is the
// only sign the operator gets that frames stopped.
const VideoPacketsPath = "/agent/v1/video-packets"

// streamStallTimeout is how long an attached stream may send no video before
// its Session ends. A live stream sends frames many times a second, even of a
// still picture; a pipeline hung on a freed CUDA context never resumes, and
// only a new pod streams again (XERK-1688), so the game is lost either way.
const streamStallTimeout = 30 * time.Second

// VideoPackets is VideoPacketsPath's response: the packets sent so far by
// each flow from the video port, keyed by peer. The node's table also keeps a
// previous attach's flow for minutes after it ended, so only a flow's count
// rising shows video. Counted is false where the node does not count packets
// per flow (net.netfilter.nf_conntrack_acct off) or the agent was not given
// the video port; Flows is then empty.
type VideoPackets struct {
	Counted bool              `json:"counted"`
	Flows   map[string]uint64 `json:"flows,omitempty"`
}

// ReadVideoPackets counts the packets sent from videoPort in the node's
// conntrack table. Callers must wrap the handler serving it in
// wolfapi.RequireBearerToken.
func ReadVideoPackets(videoPort int, acctPath, tablePath string) (VideoPackets, error) {
	if videoPort == 0 {
		return VideoPackets{}, nil
	}
	counted, err := conntrack.Counted(acctPath)
	if err != nil {
		return VideoPackets{}, fmt.Errorf("reading packet accounting setting: %w", err)
	}
	if !counted {
		return VideoPackets{}, nil
	}
	flows, err := conntrack.UDPFlowsFrom(tablePath, videoPort)
	if err != nil {
		return VideoPackets{}, fmt.Errorf("reading conntrack table: %w", err)
	}
	return VideoPackets{Counted: true, Flows: flows}, nil
}

// VideoPacketsHandler serves read's count as JSON.
func VideoPacketsHandler(read func() (VideoPackets, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		v, err := read()
		if err != nil {
			klog.ErrorS(err, "Counting video packets")
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	})
}

// fetchVideoPackets asks the session's wolf-agent at baseURL for its count.
func fetchVideoPackets(ctx context.Context, client *http.Client, baseURL string) (VideoPackets, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+VideoPacketsPath, http.NoBody)
	if err != nil {
		return VideoPackets{}, fmt.Errorf("building %s request: %w", VideoPacketsPath, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return VideoPackets{}, fmt.Errorf("calling wolf-agent %s: %w", VideoPacketsPath, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Pods from before the endpoint answer 404.
		return VideoPackets{}, fmt.Errorf("wolf-agent %s: %s", VideoPacketsPath, resp.Status)
	}
	var v VideoPackets
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return VideoPackets{}, fmt.Errorf("decoding %s: %w", VideoPacketsPath, err)
	}
	return v, nil
}

// streamProgress is what one attach's video flows last looked like.
type streamProgress struct {
	uid           types.UID
	wolfSessionID string
	generation    int64 // status.attachedGeneration
	flows         map[string]uint64
	// rose is whether any flow's count has risen since the attach was first
	// seen, and risenAt when one last did.
	rose    bool
	risenAt time.Time
}

// videoProgress tracks streamProgress per Session ("namespace/name"). It is
// in memory: an operator restart or leader change starts each attach afresh,
// which only delays a stall's detection.
type videoProgress struct {
	mu sync.Mutex
	m  map[string]streamProgress
}

// stalledFor records flows for session's current attach at now, and returns
// how long since a flow's count last rose, or 0 while none has risen since
// the attach was first seen (its client has not started video yet). The
// flows first seen are only a baseline: they may be a previous attach's,
// still counted at their final value. A flow that expires is no progress, and
// no stall either.
func (p *videoProgress) stalledFor(session *v1alpha1types.Session, flows map[string]uint64, now time.Time) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil {
		p.m = map[string]streamProgress{}
	}
	key := session.Namespace + "/" + session.Name
	prev, ok := p.m[key]
	if !ok || prev.uid != session.UID || prev.wolfSessionID != session.Status.WolfSessionID ||
		prev.generation != session.Status.AttachedGeneration {
		p.m[key] = streamProgress{
			uid: session.UID, wolfSessionID: session.Status.WolfSessionID,
			generation: session.Status.AttachedGeneration, flows: flows,
		}
		return 0
	}
	for peer, n := range flows {
		if n > prev.flows[peer] {
			prev.rose, prev.risenAt = true, now
			break
		}
	}
	prev.flows = flows
	p.m[key] = prev
	if !prev.rose {
		return 0
	}
	return now.Sub(prev.risenAt)
}

// forget drops a deleted Session's progress.
func (p *videoProgress) forget(namespace, name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.m, namespace+"/"+name)
}

// streamStalledReason asks wolf-agent for the attached stream's video packet
// counts and returns why the Session should end if, having risen during this
// attach, none has risen for streamStallTimeout, else "". Not knowing the
// counts (an old pod, a node
// without packet accounting, an agent error) never ends a Session.
func (c *SessionController) streamStalledReason(ctx context.Context, session *v1alpha1types.Session, client *http.Client, baseURL string) string {
	v, err := fetchVideoPackets(ctx, client, baseURL)
	if err != nil {
		klog.V(2).Infof("Session %s/%s: no video packet count: %v", session.Namespace, session.Name, err)
		return ""
	}
	if !v.Counted {
		klog.V(2).Infof("Session %s/%s: node does not count video packets (net.netfilter.nf_conntrack_acct); not checking for a frozen stream", session.Namespace, session.Name)
		return ""
	}
	if d := c.videoProgress.stalledFor(session, v.Flows, time.Now()); d >= streamStallTimeout {
		return fmt.Sprintf("its stream %s sent no video for %s while Wolf still lists it: the stream froze", session.Status.WolfSessionID, d.Round(time.Second))
	}
	return ""
}
