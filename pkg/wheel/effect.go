package wheel

import "encoding/binary"

// decodeEffect reads a struct ff_effect: a 14-byte header, then at offset 16
// the union member its type selects.
func decodeEffect(b []byte) Effect {
	u16 := func(off int) uint16 { return binary.NativeEndian.Uint16(b[off:]) }
	s16 := func(off int) int16 { return int16(u16(off)) } //nolint:gosec // bit pattern
	env := func(off int) Envelope {
		return Envelope{AttackLength: u16(off), AttackLevel: u16(off + 2), FadeLength: u16(off + 4), FadeLevel: u16(off + 6)}
	}
	e := Effect{
		Type: u16(0), ID: s16(2), Direction: u16(4),
		TriggerButton: u16(6), TriggerInterval: u16(8),
		ReplayLength: u16(10), ReplayDelay: u16(12),
	}
	const u = 16
	switch e.Type {
	case ffConstant:
		e.Constant = &Constant{Level: s16(u), Envelope: env(u + 2)}
	case ffRamp:
		e.Ramp = &Ramp{StartLevel: s16(u), EndLevel: s16(u + 2), Envelope: env(u + 4)}
	case ffPeriodic:
		e.Periodic = &Periodic{
			Waveform: u16(u), Period: u16(u + 2), Magnitude: s16(u + 4),
			Offset: s16(u + 6), Phase: u16(u + 8), Envelope: env(u + 10),
		}
	case ffSpring, ffFriction, ffDamper, ffInertia:
		for i := range 2 {
			o := u + 12*i
			e.Condition = append(e.Condition, Condition{
				RightSaturation: u16(o), LeftSaturation: u16(o + 2),
				RightCoeff: s16(o + 4), LeftCoeff: s16(o + 6),
				Deadband: u16(o + 8), Center: s16(o + 10),
			})
		}
	case ffRumble:
		e.Rumble = &Rumble{StrongMagnitude: u16(u), WeakMagnitude: u16(u + 2)}
	}
	return e
}

// ffEvent is the message for an EV_FF event the game wrote: gain,
// autocenter, or playing (value > 0, the repeat count) or stopping an effect.
func ffEvent(code uint16, value int32) Message {
	switch code {
	case ffGain:
		return Message{Kind: FFGain, Value: &value}
	case ffAutocenter:
		return Message{Kind: FFAutocenter, Value: &value}
	}
	id := int16(code) //nolint:gosec // effect ids are < ff_effects_max
	if value == 0 {
		return Message{Kind: FFStop, EffectID: &id}
	}
	return Message{Kind: FFPlay, EffectID: &id, Value: &value}
}
