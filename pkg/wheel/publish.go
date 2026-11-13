package wheel

import (
	"fmt"
	"strconv"
	"time"
)

// Bridge is what Publish needs of the input-bridge (inputbridge.Server).
type Bridge interface {
	Publish(props map[string]string, udevFile string, content []string) error
	Withdraw(props map[string]string, udevFile string) error
}

// BridgePublisher publishes wheels through the Session's input-bridge, as
// udev on a desktop would: the node, a udev entry that marks it a joystick
// (libudev consumers such as SDL and Proton's winebus skip input devices
// without one), and an add event for the App pod's udev-relay.
type BridgePublisher struct{ Bridge Bridge }

// Publish implements Publisher.
func (p BridgePublisher) Publish(n Node, req *Request) error {
	if err := p.Bridge.Publish(nodeProps(n, req), udevFile(n), udevEntry(req)); err != nil {
		return fmt.Errorf("publish %s: %w", n.Event, err)
	}
	return nil
}

// Withdraw implements Publisher.
func (p BridgePublisher) Withdraw(n Node) error {
	if n.Event == "" {
		return nil
	}
	if err := p.Bridge.Withdraw(nodeProps(n, nil), udevFile(n)); err != nil {
		return fmt.Errorf("withdraw %s: %w", n.Event, err)
	}
	return nil
}

func udevFile(n Node) string { return fmt.Sprintf("c%d:%d", n.Major, n.Minor) }

// nodeProps are the event node's udev properties, as udevd would announce
// them for a USB joystick.
func nodeProps(n Node, req *Request) map[string]string {
	p := map[string]string{
		"DEVNAME":           "/dev/input/" + n.Event,
		"DEVPATH":           "/devices/virtual/input/" + n.Sysname + "/" + n.Event,
		"SUBSYSTEM":         "input",
		"MAJOR":             strconv.FormatUint(uint64(n.Major), 10),
		"MINOR":             strconv.FormatUint(uint64(n.Minor), 10),
		"ID_INPUT":          "1",
		"ID_INPUT_JOYSTICK": "1",
	}
	if req != nil {
		p["ID_VENDOR_ID"] = fmt.Sprintf("%04x", req.Vendor)
		p["ID_MODEL_ID"] = fmt.Sprintf("%04x", req.Product)
		p["ID_BUS"] = "usb"
	}
	return p
}

// udevEntry is the node's /run/udev/data entry: libudev reads a device with
// one as initialized.
func udevEntry(req *Request) []string {
	return []string{
		"I:" + strconv.FormatInt(time.Now().UnixMicro(), 10),
		"E:ID_INPUT=1",
		"E:ID_INPUT_JOYSTICK=1",
		"E:ID_BUS=usb",
		fmt.Sprintf("E:ID_VENDOR_ID=%04x", req.Vendor),
		fmt.Sprintf("E:ID_MODEL_ID=%04x", req.Product),
		"G:seat",
		"G:uaccess",
		"Q:seat",
		"Q:uaccess",
		"V:1",
	}
}
