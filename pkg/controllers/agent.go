package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/r3labs/sse/v2"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/klog/v2"

	"games-on-whales.github.io/direwolf/pkg/fakeudev"
	"games-on-whales.github.io/direwolf/pkg/inputbridge"
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
	// Publishes the /dev/input nodes: the input-bridge sidecar when the App
	// runs in its own pod (UseInputBridge), else nil: mknod into inputDevPath.
	bridge deviceNodes

	// Bounds of the delay before resubscribing after Wolf's event stream
	// ends or a subscribe fails. A stream that stayed up for maxDelay resets
	// it to minDelay.
	minResubscribeDelay, maxResubscribeDelay time.Duration
	// Reports a failed subscribe; utilruntime.HandleError outside tests.
	// Its rate limiter keeps real time, which a synctest bubble's fake
	// clock reads as a sleep of decades.
	subscribeFailed func(error)

	lobby *lobbyJoiner

	// Serializes publishing and removing devices, including
	// pruneGoneDevices, which runs on its own goroutine: a prune between a
	// plug's node and its record would drop the new node.
	devicesMu sync.Mutex
	// The devices whose nodes are published, by node DEVPATH, for
	// pruneGoneDevices; sysDevChar is /sys/dev/char outside tests, "" to
	// prune nothing, every pruneInterval.
	published     map[string]publishedDevice
	sysDevChar    string
	pruneInterval time.Duration
}

// publishedDevice is what a plug published for one /dev/input node: Wolf's
// udev events for its device (to announce its removal) and its udev entries.
type publishedDevice struct {
	node     map[string]string
	events   []map[string]string
	hwdbFile []string
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
		subscribeFailed:     utilruntime.HandleError,
		sysDevChar:          "/sys/dev/char",
		pruneInterval:       time.Second,
	}
	res.lobby = &lobbyJoiner{client: wolfClient, settle: lobbyJoinSettle, unplug: res.handleDeviceUnplug}

	return res
}

// deviceNodes publishes the /dev/input nodes of hotplugged devices.
// and their udev database entries.
type deviceNodes interface {
	Create(props map[string]string) error
	Remove(props map[string]string) error
	WriteHwDB(filename string, content []string) error
	RemoveHwDB(filename string) error
	// Announce broadcasts a udev event where the App hears it, once its node
	// and entry are published. A no-op in one session pod: the App shares the
	// node's network namespace, where its udevd announces Wolf's devices.
	Announce(props map[string]string) error
	// Clear drops every node and entry.
	Clear() error
}

// dirNodes writes into volumes the App's containers share: mknod into dev,
// udev entries into udev.
type dirNodes struct{ dev, udev string }

func (d dirNodes) Create(props map[string]string) error {
	return wrapNodeErr(fakeudev.CreateDeviceNode(d.dev, props))
}

func (d dirNodes) Remove(props map[string]string) error {
	return wrapNodeErr(fakeudev.RemoveDeviceNode(d.dev, props))
}

func (d dirNodes) WriteHwDB(filename string, content []string) error {
	return wrapNodeErr(fakeudev.WriteHwDbEntry(d.udev, filename, content))
}

func (d dirNodes) RemoveHwDB(filename string) error {
	return wrapNodeErr(fakeudev.RemoveHwDbEntry(d.udev, filename))
}

func (d dirNodes) Announce(map[string]string) error { return nil }

func (d dirNodes) Clear() error {
	return wrapNodeErr(errors.Join(fakeudev.ClearDir(d.dev), fakeudev.ClearDir(d.udev)))
}

func wrapNodeErr(err error) error {
	if err != nil {
		return fmt.Errorf("device node: %w", err)
	}
	return nil
}

// UseInputBridge publishes device nodes and udev entries through the
// input-bridge sidecar listening on socket: the App runs in a user-namespaced
// pod of its own, where a node in a shared volume would not open, and whose
// root could write any directory it can map (pkg/inputbridge).
func (a *Agent) UseInputBridge(socket string) {
	a.bridge = &bridgeNodes{inputbridge.Client{Socket: socket}}
}

// bridgeNodes is the input-bridge client as deviceNodes.
type bridgeNodes struct{ inputbridge.Client }

func (b *bridgeNodes) Announce(props map[string]string) error { return wrapNodeErr(b.Uevent(props)) }

func (a *Agent) nodes() deviceNodes {
	if a.bridge != nil {
		return a.bridge
	}
	return dirNodes{dev: a.inputDevPath, udev: a.udevDataPath}
}

// Run handles Wolf's events until ctx ends. Whenever the stream closes or a
// subscribe fails it resubscribes, with backoff: without the stream, pause
// events would no longer stop Wolf's session or clear the devices.
func (a *Agent) Run(ctx context.Context) {
	klog.Infof("Starting Agent")
	// For the agent's whole life: Wolf destroys devices while its event
	// stream is down too.
	go a.pruneLoop(ctx)
	delay := a.minResubscribeDelay
	for {
		// Timed from the request: a proxy may hold the response headers
		// back until the stream's first event.
		start := time.Now()
		ch, err := a.WolfClient.SubscribeToEvents(ctx)
		if err != nil {
			a.subscribeFailed(fmt.Errorf("failed to subscribe to Wolf events: %w", err))
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
	// Wolf keeps the joined stream's joypads plugged into the game when it
	// ends (WOLF_LOBBY_KEEPS_JOYPADS): their nodes stay, as on a pause event.
	joined, _ := a.lobby.joinedStream()
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
		if s.ClientID != joined {
			a.clearDevices()
		}
	}

	if stream, key := a.lobby.joinedStream(); stream != "" && !slices.ContainsFunc(live, func(s wolfapi.Session) bool {
		return s.ClientID == stream && (key == "" || s.AESKey == key)
	}) {
		klog.Infof("Session %s was stopped while the event stream was down", stream)
		a.lobby.ended(stream)
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

				// Wolf keeps a lobby stream's joypads plugged into the game
				// for the next stream (WOLF_LOBBY_KEEPS_JOYPADS): their
				// nodes stay, or the game would see them unplugged.
				joined, _ := a.lobby.joinedStream()
				a.lobby.ended(pauseEvent.SessionID)
				if err := a.WolfClient.StopSession(ctx, pauseEvent.SessionID); err != nil {
					utilruntime.HandleError(err)
					continue
				}
				if joined != pauseEvent.SessionID {
					a.clearDevices()
				}
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
	a.devicesMu.Lock()
	defer a.devicesMu.Unlock()
	a.published = nil
	if err := a.nodes().Clear(); err != nil {
		utilruntime.HandleError(fmt.Errorf("failed to clear devices: %w", err))
	}
}

func (a *Agent) handleDevicePlug(ev wolfapi.PlugDeviceEvent) {
	a.devicesMu.Lock()
	defer a.devicesMu.Unlock()
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
		if err := a.nodes().Create(udevEvent); err != nil {
			utilruntime.HandleError(fmt.Errorf("failed to create device node: %w", err))
		} else {
			klog.InfoS("Created device node", "devname", udevEvent["DEVNAME"], "session", ev.SessionID)
		}
	}

	var hwdbFiles []string
	for _, entry := range ev.UdevHwDbEntries {
		filename := entry.Filename
		// Recompute "cMAJOR:MINOR" filenames that were built from the
		// zeroed-out device numbers.
		if filename == "c0:0" && len(ev.UdevEvents) > 0 {
			if maj := ev.UdevEvents[0]["MAJOR"]; maj != "" && maj != "0" {
				filename = "c" + maj + ":" + ev.UdevEvents[0]["MINOR"]
			}
		}
		if err := a.nodes().WriteHwDB(filename, entry.Content); err != nil {
			utilruntime.HandleError(fmt.Errorf("failed to write udev hwdb entry %q: %w", filename, err))
		} else {
			klog.InfoS("Wrote udev hwdb entry", "file", filename, "session", ev.SessionID)
			hwdbFiles = append(hwdbFiles, filename)
		}
	}
	a.announce(ev.UdevEvents, "add")
	for _, udevEvent := range ev.UdevEvents {
		if !fakeudev.IsInputDeviceNode(udevEvent) {
			continue
		}
		// A device Wolf destroyed without an unplug may have left its record
		// on this node: the node is this device's now, and pruning the old
		// record would drop it.
		maps.DeleteFunc(a.published, func(_ string, d publishedDevice) bool {
			return d.node["DEVNAME"] == udevEvent["DEVNAME"] ||
				d.node["MAJOR"]+":"+d.node["MINOR"] == udevEvent["MAJOR"]+":"+udevEvent["MINOR"]
		})
		if a.published == nil {
			a.published = map[string]publishedDevice{}
		}
		a.published[udevEvent["DEVPATH"]] = publishedDevice{node: udevEvent, events: ev.UdevEvents, hwdbFile: hwdbFiles}
	}
}

// pruneLoop runs pruneGoneDevices every pruneInterval until ctx ends.
func (a *Agent) pruneLoop(ctx context.Context) {
	if a.sysDevChar == "" {
		return
	}
	t := time.NewTicker(a.pruneInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.pruneGoneDevices()
		}
	}
}

// pruneGoneDevices drops the published nodes whose device the kernel no
// longer has under their numbers. Wolf destroys a device without an unplug
// event in places the agent can't see, e.g. a joined stream that left the
// lobby while the event stream was down and then ended (catchUp keeps a joined
// stream's nodes, as Wolf keeps its joypads in the game). A 0666 node left
// behind would open whichever device the kernel gives its minor next.
func (a *Agent) pruneGoneDevices() {
	if a.sysDevChar == "" {
		return
	}
	a.devicesMu.Lock()
	defer a.devicesMu.Unlock()
	for devpath, d := range a.published {
		target, err := os.Readlink(filepath.Join(a.sysDevChar, d.node["MAJOR"]+":"+d.node["MINOR"]))
		if err == nil && strings.HasSuffix(target, devpath) {
			continue
		}
		klog.InfoS("Dropping the node of a device that is gone", "devname", d.node["DEVNAME"], "devpath", devpath)
		if err := a.nodes().Remove(d.node); err != nil {
			utilruntime.HandleError(fmt.Errorf("failed to remove device node: %w", err))
		}
		for _, f := range d.hwdbFile {
			if err := a.nodes().RemoveHwDB(f); err != nil {
				utilruntime.HandleError(fmt.Errorf("failed to remove udev hwdb entry %q: %w", f, err))
			}
		}
		a.announce(d.events, "remove")
		delete(a.published, devpath)
	}
}

// announce passes Wolf's udev events for one device on, in Wolf's order (the
// parent input device, then its nodes), as action.
func (a *Agent) announce(events []map[string]string, action string) {
	for _, udevEvent := range events {
		props := maps.Clone(udevEvent)
		props["ACTION"] = action
		if err := a.nodes().Announce(props); err != nil {
			utilruntime.HandleError(fmt.Errorf("failed to announce udev event %q: %w", props["DEVPATH"], err))
		}
	}
}

func (a *Agent) handleDeviceUnplug(ev wolfapi.UnplugDeviceEvent) {
	a.devicesMu.Lock()
	defer a.devicesMu.Unlock()
	// Wolf usually destroys the device before the agent hears of the unplug,
	// so sysfs no longer has its numbers (Wolf sends MAJOR "0"): a published
	// node takes them, and its udev entries, from what its plug recorded.
	var hwdbFiles []string
	recorded := false
	// The nodes another device holds now (by DEVPATH): the kernel reuses
	// eventN, never inputN, so a device destroyed before its unplug arrived
	// may have had its node taken by a new one. Neither removed nor announced.
	taken := map[string]bool{}
	for _, udevEvent := range ev.UdevEvents {
		if d, ok := a.published[udevEvent["DEVPATH"]]; ok {
			udevEvent["MAJOR"], udevEvent["MINOR"] = d.node["MAJOR"], d.node["MINOR"]
			hwdbFiles = append(hwdbFiles, d.hwdbFile...)
			recorded = true
		} else if a.publishedOn(udevEvent["DEVNAME"]) {
			klog.InfoS("Keeping a device node a new device took over", "devname", udevEvent["DEVNAME"], "devpath", udevEvent["DEVPATH"])
			taken[udevEvent["DEVPATH"]] = true
			continue
		} else {
			fakeudev.ResolveDevNumbers(udevEvent)
		}
		if !fakeudev.IsInputDeviceNode(udevEvent) {
			continue
		}
		if err := a.nodes().Remove(udevEvent); err != nil {
			// Kept: pruneGoneDevices retries once the device is gone
			utilruntime.HandleError(fmt.Errorf("failed to remove device node: %w", err))
			continue
		}
		delete(a.published, udevEvent["DEVPATH"])
	}

	if !recorded {
		for _, entry := range ev.UdevHwDbEntries {
			filename := entry.Filename
			if filename == "c0:0" && len(ev.UdevEvents) > 0 {
				if maj := ev.UdevEvents[0]["MAJOR"]; maj != "" && maj != "0" {
					filename = "c" + maj + ":" + ev.UdevEvents[0]["MINOR"]
				}
			}
			hwdbFiles = append(hwdbFiles, filename)
		}
	}
	// Each of the device's records lists all its entries, and a taken node's
	// entry is its new device's now.
	keep := map[string]bool{}
	for devpath, d := range a.published {
		if !slices.ContainsFunc(ev.UdevEvents, func(e map[string]string) bool { return e["DEVPATH"] == devpath }) {
			keep["c"+d.node["MAJOR"]+":"+d.node["MINOR"]] = true
		}
	}
	for _, filename := range hwdbFiles {
		if keep[filename] {
			continue
		}
		keep[filename] = true
		if err := a.nodes().RemoveHwDB(filename); err != nil {
			utilruntime.HandleError(fmt.Errorf("failed to remove udev hwdb entry %q: %w", filename, err))
		}
	}
	a.announce(slices.DeleteFunc(slices.Clone(ev.UdevEvents), func(e map[string]string) bool {
		return taken[e["DEVPATH"]]
	}), "remove")
}

// publishedOn reports whether a published record holds the node devname.
func (a *Agent) publishedOn(devname string) bool {
	if devname == "" {
		return false
	}
	for _, d := range a.published {
		if d.node["DEVNAME"] == devname {
			return true
		}
	}
	return false
}
