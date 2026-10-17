package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/r3labs/sse/v2"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/klog/v2"

	"games-on-whales.github.io/direwolf/pkg/fakeudev"
	"games-on-whales.github.io/direwolf/pkg/wolfapi"
)

// UdevDataPath is where udev database entries are written for hotplugged
// devices. It must be a volume shared with the app container (mounted at
// /run/udev there) for libudev consumers (SDL/Steam) to see them.
const UdevDataPath = "/run/udev/data"

// InputDevPath is where the agent creates the /dev/input nodes of hotplugged
// devices. It must be a volume shared with the app container, mounted at
// /dev/input in both (see inputDevVolume).
const InputDevPath = "/dev/input"

// Represents the controller that runs inside the Pod itself
type Agent struct {
	WolfClient wolfapi.Client

	// Where hotplugged devices are published; InputDevPath and
	// UdevDataPath outside tests.
	inputDevPath, udevDataPath string

	// Bounds of the delay before resubscribing after Wolf's event stream
	// ends or a subscribe fails. A stream that stayed up for maxDelay resets
	// it to minDelay.
	minResubscribeDelay, maxResubscribeDelay time.Duration

	lobby *lobbyJoiner
}

func NewAgent(
	wolfClient wolfapi.Client,
) *Agent {
	res := &Agent{
		WolfClient:   wolfClient,
		inputDevPath: InputDevPath,
		udevDataPath: UdevDataPath,

		minResubscribeDelay: 200 * time.Millisecond,
		maxResubscribeDelay: 10 * time.Second,
	}
	res.lobby = &lobbyJoiner{client: wolfClient, settle: lobbyJoinSettle, unplug: res.handleDeviceUnplug}

	return res
}

// Run handles Wolf's events until ctx ends. Whenever the stream closes or a
// subscribe fails it resubscribes, with backoff: without the stream, pause
// events would no longer stop Wolf's session or clear the devices.
func (a *Agent) Run(ctx context.Context) {
	klog.Infof("Starting Agent")
	delay := a.minResubscribeDelay
	for {
		// Timed from the request: a proxy may hold the response headers
		// back until the stream's first event.
		start := time.Now()
		ch, err := a.WolfClient.SubscribeToEvents(ctx)
		if err != nil {
			utilruntime.HandleError(fmt.Errorf("failed to subscribe to Wolf events: %w", err))
		} else {
			klog.Infof("Subscribed to Wolf events")
			a.catchUp(ctx)
			a.handleEvents(ctx, ch)
			if time.Since(start) >= a.maxResubscribeDelay {
				delay = a.minResubscribeDelay
			}
		}
		if ctx.Err() != nil {
			return
		}
		klog.Infof("Resubscribing to Wolf events in %s", delay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(2*delay, a.maxResubscribeDelay)
	}
}

// catchUp acts on the stream events Wolf sent while no event stream was
// connected: Wolf replays none, but lists each stream's state (XERK-1947).
// Runs before the new stream's events are handled, so the joiner's state is
// still that of before the outage.
//
// A paused stream is stopped, as its pause event would have (XERK-1378):
// missed, Wolf's session would stay listed, and the operator would never
// start the disconnect grace period. A joined stream Wolf no longer lists
// was stopped meanwhile. A live stream the joiner doesn't know started
// meanwhile (XERK-1797): its setup events are lost, and without them the
// joiner ignores its pings, so it is seeded as if they were seen.
func (a *Agent) catchUp(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, lobbyCallTimeout)
	defer cancel()
	sessions, err := a.WolfClient.ListSessions(ctx)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("listing Wolf's streams for events missed while the event stream was down: %w", err))
		return
	}

	// Wolf lists a stream's session ID as client_id. IDs repeat across
	// streams (wolf_clients.go), so streams are told apart by AES key.
	var live, paused []wolfapi.Session
	for _, s := range sessions {
		if s.State == wolfapi.SessionPaused {
			paused = append(paused, s)
		} else {
			live = append(live, s)
		}
	}
	for _, s := range paused {
		// Wolf stops by ID, which would end a live stream sharing it too.
		if slices.ContainsFunc(live, func(l wolfapi.Session) bool { return l.ClientID == s.ClientID }) {
			klog.Warningf("Session %s paused while the event stream was down, but a live stream shares its ID: not stopping it", s.ClientID)
			continue
		}
		klog.Infof("Session %s paused while the event stream was down: stopping it", s.ClientID)
		a.lobby.ended(s.ClientID)
		// Failed, Wolf keeps listing it paused, so the next subscribe tries
		// again.
		if err = a.WolfClient.StopSession(ctx, s.ClientID); err != nil {
			utilruntime.HandleError(err)
			continue
		}
		a.clearDevices()
	}

	if stream, key := a.lobby.joinedStream(); stream != "" && !slices.ContainsFunc(live, func(s wolfapi.Session) bool {
		return s.ClientID == stream && (key == "" || s.AESKey == key)
	}) {
		klog.Infof("Session %s was stopped while the event stream was down", stream)
		a.lobby.ended(stream)
		a.clearDevices()
	}

	var missed []wolfapi.Session
	for _, s := range live {
		if !a.lobby.knows(s.ClientID, s.AESKey) {
			missed = append(missed, s)
		}
	}
	if len(missed) == 0 {
		return
	}
	// The pod streams to one client at a time, so a stream in a lobby is the
	// live one (e.g. the agent restarted after joining it): nothing to seed.
	lobbies, err := a.WolfClient.ListLobbies(ctx)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("listing lobbies for a stream started while the event stream was down: %w", err))
		return
	}
	for _, l := range lobbies {
		if len(l.ConnectedSessions) > 0 {
			return
		}
	}
	if len(missed) > 1 {
		klog.Warningf("%d live streams started while the event stream was down; joining none to the lobby", len(missed))
		return
	}
	klog.Infof("Session %s (%s) started while the event stream was down: joining it to the lobby on its next pings", missed[0].ClientID, missed[0].State)
	a.lobby.seed(missed[0].ClientID, missed[0].AESKey)
}

// handleEvents handles ch's events until it is closed or ctx ends.
func (a *Agent) handleEvents(ctx context.Context, ch <-chan *sse.Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				klog.Infof("Wolf event stream closed")
				return
			}
			if ev == nil {
				continue
			}

			logEvent(ev)

			switch wolfapi.WolfEventType(ev.Event) {
			// Wolf handles a moonlight disconnect as a "Pause" (and takes
			// the stream out of the lobby itself). The stream can't be
			// resumed with the client's new keys, so stop it; the game
			// keeps running on the lobby's display.
			case wolfapi.PauseStreamEventType:
				var pauseEvent wolfapi.PauseStreamEvent
				if err := json.Unmarshal(ev.Data, &pauseEvent); err != nil {
					utilruntime.HandleError(fmt.Errorf("failed to unmarshal pause stream event: %w", err))
					continue
				}

				a.lobby.ended(pauseEvent.SessionID)
				if err := a.WolfClient.StopSession(ctx, pauseEvent.SessionID); err != nil {
					utilruntime.HandleError(err)
					continue
				}
				a.clearDevices()
			// Wolf hotplugged a virtual input device. Play the role of
			// fake-udev: publish its node and udev db entry. The app
			// container hears of it from the node's udevd, whose
			// events reach it through the host network namespace.
			case wolfapi.PlugDeviceEventType:
				var plugEvent wolfapi.PlugDeviceEvent
				if err := json.Unmarshal(ev.Data, &plugEvent); err != nil {
					utilruntime.HandleError(fmt.Errorf("failed to unmarshal plug device event: %w", err))
					continue
				}
				a.handleDevicePlug(plugEvent)
			case wolfapi.UnplugDeviceEventType:
				var unplugEvent wolfapi.UnplugDeviceEvent
				if err := json.Unmarshal(ev.Data, &unplugEvent); err != nil {
					utilruntime.HandleError(fmt.Errorf("failed to unmarshal unplug device event: %w", err))
					continue
				}
				if a.lobby.holdsUnplug(unplugEvent) {
					continue
				}
				a.handleDeviceUnplug(unplugEvent)
			case wolfapi.StopStreamEventType:
				var stopEvent wolfapi.PauseStreamEvent
				if err := json.Unmarshal(ev.Data, &stopEvent); err != nil {
					utilruntime.HandleError(fmt.Errorf("failed to unmarshal stop stream event: %w", err))
					continue
				}
				a.lobby.ended(stopEvent.SessionID)
			// A stream a /resume replaced under the same session ID lost its
			// client; Wolf ends that stream alone. The ID now names the
			// resumed stream, so neither stop it nor end its lobby join.
			case wolfapi.EndSupersededStreamEventType:
				continue
			case wolfapi.VideoSessionEventType, wolfapi.AudioSessionEventType:
				var setup wolfapi.StreamSetupEvent
				if err := json.Unmarshal(ev.Data, &setup); err != nil {
					utilruntime.HandleError(fmt.Errorf("failed to unmarshal %s: %w", ev.Event, err))
					continue
				}
				a.lobby.streamSetup(setup.SessionID, setup.AESKey, wolfapi.WolfEventType(ev.Event) == wolfapi.VideoSessionEventType)
			case wolfapi.RTPVideoPingEventType, wolfapi.RTPAudioPingEventType:
				a.lobby.ping(ctx, wolfapi.WolfEventType(ev.Event) == wolfapi.RTPVideoPingEventType)
			default:
				continue
			}
		}
	}
}

// logEvent logs ev without its data: Wolf's session events carry the
// stream's AES key and IV.
func logEvent(ev *sse.Event) {
	if t := wolfapi.WolfEventType(ev.Event); t == wolfapi.RTPVideoPingEventType || t == wolfapi.RTPAudioPingEventType {
		// Several a second for the whole stream.
		klog.V(5).Infof("Received event: %s", ev.Event)
		return
	}
	klog.Infof("Received event: %s", ev.Event)
	klog.Infof("Event ID: %s", ev.ID)
	klog.Infof("Event Data: %d bytes", len(ev.Data))
	klog.Infof("Event Retry: %d", ev.Retry)
	klog.Infof("Event Comment: %v", ev.Comment)
}

// clearDevices drops every published device node and udev entry. Wolf
// destroys a stopped session's devices without unplug events; left behind,
// a 0666 node would open whichever device the kernel gives its minor next,
// possibly another session's keyboard. A resumed stream plugs anew.
func (a *Agent) clearDevices() {
	for _, dir := range []string{a.inputDevPath, a.udevDataPath} {
		if err := fakeudev.ClearDir(dir); err != nil {
			utilruntime.HandleError(fmt.Errorf("failed to clear %s: %w", dir, err))
		}
	}
}

func (a *Agent) handleDevicePlug(ev wolfapi.PlugDeviceEvent) {
	// Wolf reports MAJOR/MINOR as "0" when it cannot stat the device node
	// in its own container; resolve the real numbers from sysfs so both
	// the node and the hwdb filename are correct.
	for _, udevEvent := range ev.UdevEvents {
		fakeudev.ResolveDevNumbers(udevEvent)
		// Promptly: the node's udevd announces the device ~40ms after it
		// appears (measured on talos04), and SDL opens DEVNAME as soon as
		// it hears of it. Events without a /dev/input node (the parent
		// input device, hidraw) have nothing to create here.
		if !fakeudev.IsInputDeviceNode(udevEvent) {
			continue
		}
		if err := fakeudev.CreateDeviceNode(a.inputDevPath, udevEvent); err != nil {
			utilruntime.HandleError(fmt.Errorf("failed to create device node: %w", err))
		} else {
			klog.InfoS("Created device node", "devname", udevEvent["DEVNAME"], "session", ev.SessionID)
		}
	}

	for _, entry := range ev.UdevHwDbEntries {
		filename := entry.Filename
		// Recompute "cMAJOR:MINOR" filenames that were built from the
		// zeroed-out device numbers.
		if filename == "c0:0" && len(ev.UdevEvents) > 0 {
			if maj := ev.UdevEvents[0]["MAJOR"]; maj != "" && maj != "0" {
				filename = "c" + maj + ":" + ev.UdevEvents[0]["MINOR"]
			}
		}
		if err := fakeudev.WriteHwDbEntry(a.udevDataPath, filename, entry.Content); err != nil {
			utilruntime.HandleError(fmt.Errorf("failed to write udev hwdb entry %q: %w", filename, err))
		} else {
			klog.InfoS("Wrote udev hwdb entry", "file", filename, "session", ev.SessionID)
		}
	}
}

func (a *Agent) handleDeviceUnplug(ev wolfapi.UnplugDeviceEvent) {
	for _, udevEvent := range ev.UdevEvents {
		fakeudev.ResolveDevNumbers(udevEvent)
		if !fakeudev.IsInputDeviceNode(udevEvent) {
			continue
		}
		if err := fakeudev.RemoveDeviceNode(a.inputDevPath, udevEvent); err != nil {
			utilruntime.HandleError(fmt.Errorf("failed to remove device node: %w", err))
		}
	}

	for _, entry := range ev.UdevHwDbEntries {
		filename := entry.Filename
		if filename == "c0:0" && len(ev.UdevEvents) > 0 {
			if maj := ev.UdevEvents[0]["MAJOR"]; maj != "" && maj != "0" {
				filename = "c" + maj + ":" + ev.UdevEvents[0]["MINOR"]
			}
		}
		if err := fakeudev.RemoveHwDbEntry(a.udevDataPath, filename); err != nil {
			utilruntime.HandleError(fmt.Errorf("failed to remove udev hwdb entry %q: %w", filename, err))
		}
	}
}
