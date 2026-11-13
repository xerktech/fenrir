// Package wheel serves DIREWOLF_WHEEL_SOCKET to a VR Session's App
// (XERK-2852, epic XERK-2848): wivrn-server asks it for a uinput wheel shaped
// like the one plugged into the headset, feeds it the wheel's state, and gets
// back the force-feedback requests the game makes of it.
//
// The App container gets no device or capability (a hostUsers: false pod
// can't open /dev/uinput anyway): the helper, in the input-bridge process in
// the host user namespace, owns uinput and publishes each wheel's event node
// and udev entry into the Session's input directory (pkg/inputbridge).
//
// The socket is SOCK_SEQPACKET, one JSON object per packet. Anything in the
// App can connect (every App process is the same uid), so every request is
// checked against limits: a few wheels per connection, gamepad buttons and
// absolute axes only, a bounded event rate. A closed connection destroys its
// wheels.
package wheel

import (
	"errors"
	"fmt"
	"slices"
	"unicode"
)

// Request ops, client to helper.
const (
	OpCreate  = "create"
	OpEvents  = "events"
	OpDestroy = "destroy"
)

// Message ops, helper to client.
const (
	OpCreated = "created"
	OpError   = "error"
	OpFF      = "ff"
)

// FF message kinds.
const (
	FFUpload     = "upload"
	FFErase      = "erase"
	FFPlay       = "play"
	FFStop       = "stop"
	FFGain       = "gain"
	FFAutocenter = "autocenter"
)

// AbsAxis is one absolute axis and its uinput_abs_setup range.
type AbsAxis struct {
	Code uint16 `json:"code"`
	Min  int32  `json:"min"`
	Max  int32  `json:"max"`
	Fuzz int32  `json:"fuzz"`
	Flat int32  `json:"flat"`
}

// Event is one input_event the client sends for a wheel.
type Event struct {
	Type  uint16 `json:"type"`
	Code  uint16 `json:"code"`
	Value int32  `json:"value"`
}

// Request is one packet from the client.
type Request struct {
	Op string `json:"op"`
	// The client's name for the wheel, unique per connection.
	ID uint32 `json:"id"`

	// Create only: uinput_setup, absinfo, key and FF bits.
	Name         string    `json:"name,omitempty"`
	Vendor       uint16    `json:"vendor,omitempty"`
	Product      uint16    `json:"product,omitempty"`
	Version      uint16    `json:"version,omitempty"`
	Abs          []AbsAxis `json:"abs,omitempty"`
	Keys         []uint16  `json:"keys,omitempty"`
	FF           []uint16  `json:"ff,omitempty"`
	FFEffectsMax uint32    `json:"ff_effects_max,omitempty"`

	// Events only.
	Events []Event `json:"events,omitempty"`
}

// Message is one packet to the client.
type Message struct {
	Op  string `json:"op"`
	ID  uint32 `json:"id"`
	Msg string `json:"msg,omitempty"`

	// FF only.
	Kind     string  `json:"kind,omitempty"`
	Effect   *Effect `json:"effect,omitempty"`
	EffectID *int16  `json:"effect_id,omitempty"`
	Value    *int32  `json:"value,omitempty"`
}

// Effect is a struct ff_effect the game uploaded, with only its type's part
// of the union set.
type Effect struct {
	Type            uint16      `json:"type"`
	ID              int16       `json:"id"`
	Direction       uint16      `json:"direction"`
	TriggerButton   uint16      `json:"trigger_button"`
	TriggerInterval uint16      `json:"trigger_interval"`
	ReplayLength    uint16      `json:"replay_length"`
	ReplayDelay     uint16      `json:"replay_delay"`
	Constant        *Constant   `json:"constant,omitempty"`
	Ramp            *Ramp       `json:"ramp,omitempty"`
	Periodic        *Periodic   `json:"periodic,omitempty"`
	Condition       []Condition `json:"condition,omitempty"`
	Rumble          *Rumble     `json:"rumble,omitempty"`
}

// Envelope is struct ff_envelope.
type Envelope struct {
	AttackLength uint16 `json:"attack_length"`
	AttackLevel  uint16 `json:"attack_level"`
	FadeLength   uint16 `json:"fade_length"`
	FadeLevel    uint16 `json:"fade_level"`
}

// Constant is struct ff_constant_effect.
type Constant struct {
	Level    int16    `json:"level"`
	Envelope Envelope `json:"envelope"`
}

// Ramp is struct ff_ramp_effect.
type Ramp struct {
	StartLevel int16    `json:"start_level"`
	EndLevel   int16    `json:"end_level"`
	Envelope   Envelope `json:"envelope"`
}

// Periodic is struct ff_periodic_effect, without custom data (FF_CUSTOM is
// never offered: its samples live in the uploading process's memory).
type Periodic struct {
	Waveform  uint16   `json:"waveform"`
	Period    uint16   `json:"period"`
	Magnitude int16    `json:"magnitude"`
	Offset    int16    `json:"offset"`
	Phase     uint16   `json:"phase"`
	Envelope  Envelope `json:"envelope"`
}

// Condition is struct ff_condition_effect, one per axis.
type Condition struct {
	RightSaturation uint16 `json:"right_saturation"`
	LeftSaturation  uint16 `json:"left_saturation"`
	RightCoeff      int16  `json:"right_coeff"`
	LeftCoeff       int16  `json:"left_coeff"`
	Deadband        uint16 `json:"deadband"`
	Center          int16  `json:"center"`
}

// Rumble is struct ff_rumble_effect.
type Rumble struct {
	StrongMagnitude uint16 `json:"strong_magnitude"`
	WeakMagnitude   uint16 `json:"weak_magnitude"`
}

// Linux input codes (linux/input-event-codes.h).
const (
	evSyn    = 0x00
	evKey    = 0x01
	evAbs    = 0x03
	evFF     = 0x15
	evUinput = 0x0101

	synReport = 0

	ffRumble     = 0x50
	ffPeriodic   = 0x51
	ffConstant   = 0x52
	ffSpring     = 0x53
	ffFriction   = 0x54
	ffDamper     = 0x55
	ffInertia    = 0x56
	ffRamp       = 0x57
	ffSquare     = 0x58
	ffTriangle   = 0x59
	ffSine       = 0x5a
	ffSawUp      = 0x5b
	ffSawDown    = 0x5c
	ffGain       = 0x60
	ffAutocenter = 0x61
)

// Limits on what a client may ask for.
const (
	// MaxWheelsPerConn bounds the wheels one connection holds: a wheel and
	// pedals, with room for a shifter and a handbrake.
	MaxWheelsPerConn = 4
	// MaxFFEffects bounds ff_effects_max (the G920 has 16 slots).
	MaxFFEffects = 64
	// maxNameBytes is uinput_setup's name, less its NUL.
	maxNameBytes = 79
	// maxAbsCode is ABS_MISC: every axis below the multitouch ones.
	maxAbsCode = 0x28
	// maxEventsPerPacket bounds one events packet.
	maxEventsPerPacket = 256
)

// allowedKey reports whether code is a gamepad, joystick or wheel button:
// BTN_MISC through BTN_GEAR_UP (no mouse buttons, BTN_MOUSE..BTN_TASK,
// 0x110-0x117, and no digitizer tools, 0x140-0x14f), or BTN_TRIGGER_HAPPY*.
// No keyboard key: a wheel must never type into anything that hears it.
func allowedKey(code uint16) bool {
	switch {
	case code >= 0x110 && code <= 0x117, code >= 0x140 && code <= 0x14f:
		return false
	case code >= 0x100 && code <= 0x151, code >= 0x2c0 && code <= 0x2e7:
		return true
	}
	return false
}

// allowedFF are the FF bits a wheel may offer: every effect but FF_CUSTOM.
var allowedFF = []uint16{
	ffRumble, ffPeriodic, ffConstant, ffSpring, ffFriction, ffDamper, ffInertia, ffRamp,
	ffSquare, ffTriangle, ffSine, ffSawUp, ffSawDown, ffGain, ffAutocenter,
}

// validateCreate checks a create request against the limits.
func validateCreate(req *Request) error {
	if req.Name == "" || len(req.Name) > maxNameBytes {
		return fmt.Errorf("name must be 1-%d bytes", maxNameBytes)
	}
	for _, r := range req.Name {
		if r > unicode.MaxASCII || !unicode.IsPrint(r) {
			return errors.New("name must be printable ASCII")
		}
	}
	if len(req.Abs) == 0 && len(req.Keys) == 0 {
		return errors.New("a wheel needs an axis or a button")
	}
	seen := map[uint16]bool{}
	for _, a := range req.Abs {
		if a.Code > maxAbsCode {
			return fmt.Errorf("abs code %#x not allowed (max %#x)", a.Code, maxAbsCode)
		}
		if seen[a.Code] {
			return fmt.Errorf("abs code %#x repeated", a.Code)
		}
		seen[a.Code] = true
		if a.Min >= a.Max {
			return fmt.Errorf("abs code %#x: min %d not below max %d", a.Code, a.Min, a.Max)
		}
	}
	if len(req.Keys) > 0x2e7 {
		return errors.New("too many keys")
	}
	for _, k := range req.Keys {
		if !allowedKey(k) {
			return fmt.Errorf("key code %#x not allowed (gamepad buttons only)", k)
		}
	}
	for _, f := range req.FF {
		if !slices.Contains(allowedFF, f) {
			return fmt.Errorf("ff code %#x not allowed", f)
		}
	}
	if len(req.FF) > 0 && (req.FFEffectsMax == 0 || req.FFEffectsMax > MaxFFEffects) {
		return fmt.Errorf("ff_effects_max must be 1-%d with ff bits", MaxFFEffects)
	}
	if len(req.FF) == 0 && req.FFEffectsMax != 0 {
		return errors.New("ff_effects_max without ff bits")
	}
	return nil
}

// validateEvents checks an events packet: only the types a wheel has. The
// kernel drops codes the device didn't declare.
func validateEvents(evs []Event) error {
	if len(evs) == 0 || len(evs) > maxEventsPerPacket {
		return fmt.Errorf("an events packet carries 1-%d events", maxEventsPerPacket)
	}
	for _, e := range evs {
		if e.Type != evSyn && e.Type != evKey && e.Type != evAbs {
			return fmt.Errorf("event type %#x not allowed", e.Type)
		}
	}
	return nil
}

// withReport ends evs with a SYN_REPORT, so a batch is one frame.
func withReport(evs []Event) []Event {
	if last := evs[len(evs)-1]; last.Type == evSyn && last.Code == synReport {
		return evs
	}
	return append(evs, Event{Type: evSyn, Code: synReport})
}
