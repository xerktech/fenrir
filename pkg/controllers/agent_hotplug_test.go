package controllers

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/r3labs/sse/v2"

	"games-on-whales.github.io/direwolf/pkg/inputbridge"
	"games-on-whales.github.io/direwolf/pkg/wolfapi"
)

// stoppingClient serves events and records StopSession calls.
type stoppingClient struct {
	fakeEventsClient
	stopped chan string
	err     error
}

func (f *stoppingClient) StopSession(_ context.Context, id string) error {
	f.stopped <- id
	return f.err
}

func hotplugAgent(t *testing.T, client wolfapi.Client) *Agent {
	t.Helper()
	a := NewAgent(client)
	a.inputDevPath, a.udevDataPath = t.TempDir(), t.TempDir()
	a.sysDevChar = "" // the test's devices aren't the kernel's
	return a
}

// Wolf destroys a stopped stream's devices without unplug events, so a
// pause must drop their nodes and udev entries itself.
func TestAgentPauseClearsDevices(t *testing.T) {
	testAgentPause(t, nil)
}

// A stream Wolf did not stop still has its devices.
func TestAgentFailedPauseKeepsDevices(t *testing.T) {
	testAgentPause(t, errors.New("wolf refused"))
}

// A superseded stream's end names the resumed stream's ID: it must neither
// stop that stream nor drop its devices (XERK-1964).
func TestAgentIgnoresSupersededStreamEnd(t *testing.T) {
	client := &stoppingClient{
		fakeEventsClient: fakeEventsClient{events: make(chan *sse.Event)},
		stopped:          make(chan string, 1),
	}
	a := hotplugAgent(t, client)
	node := filepath.Join(a.inputDevPath, "event5")
	if err := os.WriteFile(node, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go a.Run(ctx)
	client.events <- &sse.Event{Event: []byte(wolfapi.EndSupersededStreamEventType), Data: []byte(`{"session_id":"42","rtp_secret_payload":"x"}`)}
	// The loop is unbuffered: once this is taken, the event is handled.
	client.events <- &sse.Event{Event: []byte("Done")}
	select {
	case id := <-client.stopped:
		t.Errorf("stopped Wolf session %q on a superseded stream's end", id)
	default:
	}
	if _, err := os.Stat(node); err != nil {
		t.Errorf("device node dropped: %v", err)
	}
}

func testAgentPause(t *testing.T, stopErr error) {
	t.Helper()
	client := &stoppingClient{
		fakeEventsClient: fakeEventsClient{events: make(chan *sse.Event)},
		stopped:          make(chan string, 1),
		err:              stopErr,
	}
	a := hotplugAgent(t, client)
	for _, f := range []string{filepath.Join(a.inputDevPath, "event5"), filepath.Join(a.udevDataPath, "c13:69")} {
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go a.Run(ctx)
	client.events <- &sse.Event{Event: []byte(wolfapi.PauseStreamEventType), Data: []byte(`{"session_id":"42"}`)}
	// The loop is unbuffered: once this is taken, the pause is handled.
	client.events <- &sse.Event{Event: []byte("Done")}
	select {
	case id := <-client.stopped:
		if id != "42" {
			t.Errorf("stopped %q, want 42", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pause did not stop the Wolf session")
	}
	want := 0
	if stopErr != nil {
		want = 1
	}
	for _, dir := range []string{a.inputDevPath, a.udevDataPath} {
		if entries, _ := os.ReadDir(dir); len(entries) != want {
			t.Errorf("%s has %d entries, want %d", dir, len(entries), want)
		}
	}
}

func TestAgentPlugPublishesDevice(t *testing.T) {
	a := hotplugAgent(t, nil)
	ev := wolfapi.PlugDeviceEvent{
		SessionID: "42",
		UdevEvents: []map[string]string{
			{"ACTION": "add", "DEVPATH": "/devices/virtual/input/input9"},
			{"ACTION": "add", "DEVNAME": "/dev/input/event69", "MAJOR": "13", "MINOR": "69", "DEVPATH": "/devices/virtual/input/input9/event69"},
		},
		UdevHwDbEntries: []wolfapi.UdevHwDbEntry{{Filename: "c13:69", Content: []string{"E:ID_INPUT_JOYSTICK=1"}}},
	}
	a.handleDevicePlug(ev)
	if _, err := os.Stat(filepath.Join(a.udevDataPath, "c13:69")); err != nil {
		t.Errorf("udev entry: %v", err)
	}
	node := filepath.Join(a.inputDevPath, "event69")
	st, err := os.Stat(node)
	switch {
	case errors.Is(err, os.ErrNotExist) && !canMknod(t):
		t.Skip("node creation needs CAP_MKNOD")
	case err != nil:
		t.Fatalf("node: %v", err)
	case st.Mode()&os.ModeCharDevice == 0:
		t.Errorf("%s is %v, want a char device", node, st.Mode())
	}

	a.handleDeviceUnplug(wolfapi.UnplugDeviceEvent(ev))
	if _, err := os.Stat(node); !os.IsNotExist(err) {
		t.Errorf("node after unplug: %v", err)
	}
}

type recordingNodes struct{ calls []string }

func (r *recordingNodes) Create(p map[string]string) error {
	r.calls = append(r.calls, "create "+p["DEVNAME"])
	return nil
}

func (r *recordingNodes) Remove(p map[string]string) error {
	r.calls = append(r.calls, "remove "+p["DEVNAME"])
	return nil
}

func (r *recordingNodes) WriteHwDB(name string, _ []string) error {
	r.calls = append(r.calls, "hwdb "+name)
	return nil
}

func (r *recordingNodes) RemoveHwDB(name string) error {
	r.calls = append(r.calls, "rm hwdb "+name)
	return nil
}

func (r *recordingNodes) Announce(p map[string]string) error {
	r.calls = append(r.calls, "announce "+p["ACTION"]+" "+p["DEVPATH"])
	return nil
}

func (r *recordingNodes) Clear() error {
	r.calls = append(r.calls, "clear")
	return nil
}

// With the App in its own pod, nodes and udev entries go through the
// input-bridge, and the agent writes neither directory itself (XERK-1920).
func TestAgentPublishesNodesThroughBridge(t *testing.T) {
	a := hotplugAgent(t, nil)
	bridge := &recordingNodes{}
	a.bridge = bridge
	ev := wolfapi.PlugDeviceEvent{
		SessionID: "42",
		UdevEvents: []map[string]string{
			{"ACTION": "add", "DEVPATH": "/devices/virtual/input/input9"},
			{"ACTION": "add", "DEVNAME": "/dev/input/event69", "MAJOR": "13", "MINOR": "69", "DEVPATH": "/devices/virtual/input/input9/event69"},
		},
		UdevHwDbEntries: []wolfapi.UdevHwDbEntry{{Filename: "c13:69", Content: []string{"E:ID_INPUT_JOYSTICK=1"}}},
	}
	a.handleDevicePlug(ev)
	a.handleDeviceUnplug(wolfapi.UnplugDeviceEvent(ev))
	a.clearDevices()
	// Announced after the node and entry exist, parent device first, as
	// Wolf sends them: libudev consumers open DEVNAME as soon as they hear.
	want := []string{
		"create /dev/input/event69", "hwdb c13:69",
		"announce add /devices/virtual/input/input9", "announce add /devices/virtual/input/input9/event69",
		"remove /dev/input/event69", "rm hwdb c13:69",
		"announce remove /devices/virtual/input/input9", "announce remove /devices/virtual/input/input9/event69",
		"clear",
	}
	if !slices.Equal(bridge.calls, want) {
		t.Errorf("bridge calls = %v, want %v", bridge.calls, want)
	}
	for _, dir := range []string{a.inputDevPath, a.udevDataPath} {
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("wrote into %s: %v", dir, entries)
		}
	}
}

// Through UseInputBridge the agent's announcements reach the bridge's event
// stream, which the App pod's udev-relay broadcasts (XERK-1920): without
// them Big Picture misses pads plugged after Steam started.
func TestAgentAnnouncesThroughBridge(t *testing.T) {
	dir, err := os.MkdirTemp("", "ab") //nolint:usetesting // t.TempDir's path is too long for a socket
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	srv := inputbridge.NewServer(filepath.Join(dir, "in"), filepath.Join(dir, "udev"), dir)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	lc := net.ListenConfig{}
	reqs, err := lc.Listen(ctx, "unix", filepath.Join(dir, "r"))
	if err != nil {
		t.Fatal(err)
	}
	evs, err := lc.Listen(ctx, "unix", filepath.Join(dir, "e"))
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ctx, reqs) }()
	go func() { _ = srv.ServeEvents(ctx, evs) }()

	a := hotplugAgent(t, nil)
	a.UseInputBridge(filepath.Join(dir, "r"))
	a.announce([]map[string]string{{"DEVPATH": "/devices/virtual/input/input9", "SUBSYSTEM": "input"}}, "add")

	got := make(chan map[string]string, 1)
	go func() {
		_ = inputbridge.Subscribe(ctx, filepath.Join(dir, "e"), func(ev map[string]string) error {
			got <- ev
			return nil
		})
	}()
	select {
	case ev := <-got:
		if ev["ACTION"] != "add" || ev["DEVPATH"] != "/devices/virtual/input/input9" {
			t.Errorf("relayed %v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event reached the bridge's stream")
	}
}

// A published node whose device the kernel no longer has under its numbers
// (gone, or the minor reused by another device) is dropped with its entry,
// and its removal announced; one whose device is there stays (XERK-2365).
func TestAgentPrunesNodesOfGoneDevices(t *testing.T) {
	a := hotplugAgent(t, nil)
	nodes := &recordingNodes{}
	a.bridge = nodes
	a.sysDevChar = t.TempDir()
	plug := func(input string, minor string) {
		a.handleDevicePlug(wolfapi.PlugDeviceEvent{
			SessionID: "42",
			UdevEvents: []map[string]string{
				{"ACTION": "add", "DEVPATH": "/devices/virtual/input/" + input},
				{"ACTION": "add", "DEVNAME": "/dev/input/event" + minor, "MAJOR": "13", "MINOR": minor, "DEVPATH": "/devices/virtual/input/" + input + "/event" + minor},
			},
			UdevHwDbEntries: []wolfapi.UdevHwDbEntry{{Filename: "c13:" + minor, Content: []string{"E:ID_INPUT_JOYSTICK=1"}}},
		})
	}
	link := func(minor, target string) {
		if err := os.Symlink("../../devices/virtual/input/"+target, filepath.Join(a.sysDevChar, "13:"+minor)); err != nil {
			t.Fatal(err)
		}
	}
	plug("input9", "72")
	plug("input10", "73")
	plug("input11", "74")
	link("72", "input9/event72")  // still there
	link("73", "input12/event73") // its minor now another device's
	// 74: gone
	nodes.calls = nil
	a.pruneGoneDevices()
	for _, want := range []string{
		"remove /dev/input/event73", "rm hwdb c13:73",
		"announce remove /devices/virtual/input/input10", "announce remove /devices/virtual/input/input10/event73",
		"remove /dev/input/event74", "rm hwdb c13:74",
		"announce remove /devices/virtual/input/input11/event74",
	} {
		if !slices.Contains(nodes.calls, want) {
			t.Errorf("no %q in %v", want, nodes.calls)
		}
	}
	if slices.ContainsFunc(nodes.calls, func(c string) bool { return strings.Contains(c, "72") || strings.Contains(c, "input9") }) {
		t.Errorf("pruned the device still there: %v", nodes.calls)
	}
	nodes.calls = nil
	a.pruneGoneDevices()
	if len(nodes.calls) != 0 {
		t.Errorf("pruned again: %v", nodes.calls)
	}
}

// A device Wolf destroyed without an unplug leaves its record; the next
// device on its minor replaces it, so pruning never drops the new node. An
// unplugged or cleared device is no longer pruned either.
func TestAgentPruneSparesNodeReusedByNewDevice(t *testing.T) {
	a := hotplugAgent(t, nil)
	nodes := &recordingNodes{}
	a.bridge = nodes
	a.sysDevChar = t.TempDir()
	plug := func(input, minor string) wolfapi.PlugDeviceEvent {
		ev := wolfapi.PlugDeviceEvent{
			SessionID: "42",
			UdevEvents: []map[string]string{
				{"ACTION": "add", "DEVNAME": "/dev/input/event" + minor, "MAJOR": "13", "MINOR": minor, "DEVPATH": "/devices/virtual/input/" + input + "/event" + minor},
			},
		}
		a.handleDevicePlug(ev)
		return ev
	}
	plug("input9", "72")
	plug("input10", "72") // input9 destroyed, its minor reused
	if err := os.Symlink("../../devices/virtual/input/input10/event72", filepath.Join(a.sysDevChar, "13:72")); err != nil {
		t.Fatal(err)
	}
	nodes.calls = nil
	a.pruneGoneDevices()
	if len(nodes.calls) != 0 {
		t.Errorf("pruned the device now on the node: %v", nodes.calls)
	}

	// Gone, but already unplugged or cleared: nothing left to prune
	gone := plug("input11", "73")
	a.handleDeviceUnplug(wolfapi.UnplugDeviceEvent(gone))
	nodes.calls = nil
	a.pruneGoneDevices()
	if len(nodes.calls) != 0 {
		t.Errorf("pruned a device already unplugged: %v", nodes.calls)
	}
	plug("input12", "74")
	a.clearDevices()
	nodes.calls = nil
	a.pruneGoneDevices()
	if len(nodes.calls) != 0 {
		t.Errorf("pruned a device already cleared: %v", nodes.calls)
	}
}

// unreachableClient fails every subscribe, as when Wolf's API is down.
type unreachableClient struct{ *lobbyClient }

func (unreachableClient) SubscribeToEvents(context.Context) (<-chan *sse.Event, error) {
	return nil, errors.New("wolf unreachable")
}

// The prune runs for the agent's life, also while Wolf's event stream is
// down (here no subscribe ever succeeds), and keeps running: Wolf destroys
// devices then too.
func TestAgentPrunesWhileEventStreamIsDown(t *testing.T) {
	client := unreachableClient{newLobbyClient()}
	a := hotplugAgent(t, client)
	a.subscribeFailed = func(error) {}
	nodes := &recordingNodes{}
	a.bridge = nodes
	a.sysDevChar = t.TempDir() // no links: every device is gone
	a.pruneInterval = 10 * time.Millisecond
	a.minResubscribeDelay, a.maxResubscribeDelay = time.Hour, time.Hour // one failed subscribe, then a long wait
	go a.Run(t.Context())
	// Each device plugged after the last prune is pruned in turn
	for _, minor := range []string{"72", "73", "74"} {
		a.handleDevicePlug(wolfapi.PlugDeviceEvent{SessionID: "42", UdevEvents: []map[string]string{
			{"ACTION": "add", "DEVNAME": "/dev/input/event" + minor, "MAJOR": "13", "MINOR": minor, "DEVPATH": "/devices/virtual/input/input9/event" + minor},
		}})
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			a.devicesMu.Lock()
			pruned := slices.Contains(nodes.calls, "remove /dev/input/event"+minor)
			a.devicesMu.Unlock()
			if pruned {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the gone device's node event%s was never pruned", minor)
			}
		}
	}
}

// numberedNodes records what Remove is given, and fails it while refuse is set.
type numberedNodes struct {
	recordingNodes
	refuse bool
}

func (n *numberedNodes) Remove(p map[string]string) error {
	n.calls = append(n.calls, "remove "+p["DEVNAME"]+" "+p["MAJOR"]+":"+p["MINOR"])
	if n.refuse {
		return errors.New("refused")
	}
	return nil
}

// Wolf destroys a pad before the agent hears of its unplug (XERK-2412 joins
// inputtino's threads in the destructor), so sysfs can't resolve the unplug's
// zeroed numbers: the node and its udev entry go by what the plug recorded
// (XERK-2402). A refused removal keeps the record for pruneGoneDevices.
func TestAgentUnplugUsesThePlugsNumbers(t *testing.T) {
	a := hotplugAgent(t, nil)
	nodes := &numberedNodes{}
	a.bridge = nodes
	events := func(major, minor string) []map[string]string {
		return []map[string]string{
			{"ACTION": "add", "DEVPATH": "/devices/virtual/input/input9"},
			{"ACTION": "add", "DEVNAME": "/dev/input/event69", "MAJOR": major, "MINOR": minor, "DEVPATH": "/devices/virtual/input/input9/event69"},
		}
	}
	a.handleDevicePlug(wolfapi.PlugDeviceEvent{
		SessionID:       "42",
		UdevEvents:      events("13", "69"), // resolved from sysfs while the device lived
		UdevHwDbEntries: []wolfapi.UdevHwDbEntry{{Filename: "c13:69", Content: []string{"E:ID_INPUT_JOYSTICK=1"}}},
	})
	unplug := wolfapi.UnplugDeviceEvent{
		SessionID:       "42",
		UdevEvents:      events("0", "0"),
		UdevHwDbEntries: []wolfapi.UdevHwDbEntry{{Filename: "c0:0", Content: []string{"E:ID_INPUT_JOYSTICK=1"}}},
	}

	nodes.refuse = true
	nodes.calls = nil
	a.handleDeviceUnplug(unplug)
	if len(a.published) != 1 {
		t.Errorf("a refused removal dropped the record: %v", a.published)
	}

	nodes.refuse = false
	nodes.calls = nil
	unplug.UdevEvents = events("0", "0")
	a.handleDeviceUnplug(unplug)
	want := []string{
		"remove /dev/input/event69 13:69", "rm hwdb c13:69",
		"announce remove /devices/virtual/input/input9", "announce remove /devices/virtual/input/input9/event69",
	}
	if !slices.Equal(nodes.calls, want) {
		t.Errorf("calls = %v, want %v", nodes.calls, want)
	}
	if len(a.published) != 0 {
		t.Errorf("records after unplug: %v", a.published)
	}
}

// Wolf destroyed pad A, the kernel gave its eventN to pad B (a new inputN:
// those are never reused), and only then did A's unplug arrive. It must not
// remove or un-announce B's node (XERK-2450).
func TestAgentLateUnplugSparesNodeReusedByNewDevice(t *testing.T) {
	a := hotplugAgent(t, nil)
	nodes := &numberedNodes{}
	a.bridge = nodes
	events := func(input, major, minor string) []map[string]string {
		return []map[string]string{
			{"ACTION": "add", "DEVPATH": "/devices/virtual/input/" + input},
			{"ACTION": "add", "DEVNAME": "/dev/input/event69", "MAJOR": major, "MINOR": minor, "DEVPATH": "/devices/virtual/input/" + input + "/event69"},
		}
	}
	a.handleDevicePlug(wolfapi.PlugDeviceEvent{SessionID: "42", UdevEvents: events("input9", "13", "69")})
	a.handleDevicePlug(wolfapi.PlugDeviceEvent{
		SessionID:       "42",
		UdevEvents:      events("input10", "13", "69"),
		UdevHwDbEntries: []wolfapi.UdevHwDbEntry{{Filename: "c13:69", Content: []string{"E:ID_INPUT_JOYSTICK=1"}}},
	})

	nodes.calls = nil
	a.handleDeviceUnplug(wolfapi.UnplugDeviceEvent{
		SessionID:       "42",
		UdevEvents:      events("input9", "0", "0"),
		UdevHwDbEntries: []wolfapi.UdevHwDbEntry{{Filename: "c0:0", Content: []string{"E:ID_INPUT_JOYSTICK=1"}}},
	})
	for _, c := range nodes.calls {
		if strings.Contains(c, "event69") || c == "rm hwdb c13:69" || strings.HasPrefix(c, "remove ") {
			t.Errorf("A's late unplug touched B's node: %v", nodes.calls)
			break
		}
	}
	if _, ok := a.published["/devices/virtual/input/input10/event69"]; !ok || len(a.published) != 1 {
		t.Errorf("records after A's unplug = %v, want B's only", a.published)
	}

	// B's own unplug still removes it.
	nodes.calls = nil
	a.handleDeviceUnplug(wolfapi.UnplugDeviceEvent{SessionID: "42", UdevEvents: events("input10", "0", "0")})
	want := []string{
		"remove /dev/input/event69 13:69", "rm hwdb c13:69",
		"announce remove /devices/virtual/input/input10", "announce remove /devices/virtual/input/input10/event69",
	}
	if !slices.Equal(nodes.calls, want) {
		t.Errorf("calls = %v, want %v", nodes.calls, want)
	}
}

// A pad with several nodes (PS5: pad, touchpad, motion) replaced by one that
// takes only its first: the old pad's late unplug removes its other nodes and
// entries, but not the entry the new pad rewrote on the taken node (XERK-2450).
func TestAgentLateUnplugSparesUdevEntryOfPartlyReusedDevice(t *testing.T) {
	a := hotplugAgent(t, nil)
	nodes := &numberedNodes{}
	a.bridge = nodes
	node := func(input, event, minor string) map[string]string {
		return map[string]string{"ACTION": "add", "DEVNAME": "/dev/input/" + event, "MAJOR": "13", "MINOR": minor, "DEVPATH": "/devices/virtual/input/" + input + "/" + event}
	}
	ps5 := func(major string) []map[string]string {
		evs := []map[string]string{node("input9", "event69", "69"), node("input10", "event70", "70"), node("input12", "event71", "71")}
		for _, e := range evs {
			e["MAJOR"] = major
		}
		return evs
	}
	a.handleDevicePlug(wolfapi.PlugDeviceEvent{
		SessionID:       "42",
		UdevEvents:      ps5("13"),
		UdevHwDbEntries: []wolfapi.UdevHwDbEntry{{Filename: "c13:69"}, {Filename: "c13:70"}, {Filename: "c13:71"}},
	})
	a.handleDevicePlug(wolfapi.PlugDeviceEvent{
		SessionID:       "42",
		UdevEvents:      []map[string]string{node("input11", "event69", "69")},
		UdevHwDbEntries: []wolfapi.UdevHwDbEntry{{Filename: "c13:69"}},
	})

	nodes.calls = nil
	a.handleDeviceUnplug(wolfapi.UnplugDeviceEvent{SessionID: "42", UdevEvents: ps5("0")})
	// Once each, though both remaining records list them
	want := []string{
		"remove /dev/input/event70 13:70", "remove /dev/input/event71 13:71", "rm hwdb c13:70", "rm hwdb c13:71",
		"announce remove /devices/virtual/input/input10/event70", "announce remove /devices/virtual/input/input12/event71",
	}
	if !slices.Equal(nodes.calls, want) {
		t.Errorf("calls = %v, want %v", nodes.calls, want)
	}
}

// A node with no record and no other device on it (plugged before the agent
// started) is still removed by what sysfs gives, and announced.
func TestAgentUnplugOfUnrecordedNodeFallsBackToSysfs(t *testing.T) {
	a := hotplugAgent(t, nil)
	nodes := &numberedNodes{}
	a.bridge = nodes
	a.handleDeviceUnplug(wolfapi.UnplugDeviceEvent{SessionID: "42", UdevEvents: []map[string]string{
		{"ACTION": "remove", "DEVNAME": "/dev/input/event69", "MAJOR": "0", "MINOR": "0", "DEVPATH": "/devices/virtual/input/input9/event69"},
	}})
	want := []string{"remove /dev/input/event69 0:0", "announce remove /devices/virtual/input/input9/event69"}
	if !slices.Equal(nodes.calls, want) {
		t.Errorf("calls = %v, want %v", nodes.calls, want)
	}
}

// The prune of a gone device whose pad node a new device took (PS5 replaced
// by Xbox, no unplug) drops its other nodes, but neither the new device's
// udev entry nor an announce on its node (XERK-2450).
func TestAgentPruneSparesUdevEntryOfPartlyReusedDevice(t *testing.T) {
	a := hotplugAgent(t, nil)
	nodes := &numberedNodes{}
	a.bridge = nodes
	a.sysDevChar = t.TempDir()
	node := func(input, event, minor string) map[string]string {
		return map[string]string{"ACTION": "add", "DEVNAME": "/dev/input/" + event, "MAJOR": "13", "MINOR": minor, "DEVPATH": "/devices/virtual/input/" + input + "/" + event}
	}
	a.handleDevicePlug(wolfapi.PlugDeviceEvent{
		SessionID:       "42",
		UdevEvents:      []map[string]string{node("input9", "event69", "69"), node("input10", "event70", "70"), node("input12", "event71", "71")},
		UdevHwDbEntries: []wolfapi.UdevHwDbEntry{{Filename: "c13:69"}, {Filename: "c13:70"}, {Filename: "c13:71"}},
	})
	a.handleDevicePlug(wolfapi.PlugDeviceEvent{
		SessionID:       "42",
		UdevEvents:      []map[string]string{node("input11", "event69", "69")},
		UdevHwDbEntries: []wolfapi.UdevHwDbEntry{{Filename: "c13:69"}},
	})
	if err := os.Symlink("../../devices/virtual/input/input11/event69", filepath.Join(a.sysDevChar, "13:69")); err != nil {
		t.Fatal(err)
	}

	nodes.calls = nil
	a.pruneGoneDevices()
	// Once each, though both gone records list them; map order varies
	want := []string{
		"announce remove /devices/virtual/input/input10/event70", "announce remove /devices/virtual/input/input12/event71",
		"remove /dev/input/event70 13:70", "remove /dev/input/event71 13:71", "rm hwdb c13:70", "rm hwdb c13:71",
	}
	slices.Sort(nodes.calls)
	if !slices.Equal(nodes.calls, want) {
		t.Errorf("calls = %v, want %v", nodes.calls, want)
	}
}
