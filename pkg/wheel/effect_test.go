package wheel

import (
	"encoding/binary"
	"reflect"
	"testing"
)

func put16(b []byte, off int, v int) { binary.NativeEndian.PutUint16(b[off:], uint16(v)) } //nolint:gosec // test bytes

// decodeEffect reads struct ff_effect's layout: header fields at their
// offsets, the union at 16.
func TestDecodeEffect(t *testing.T) {
	b := make([]byte, 48)
	put16(b, 0, ffSpring)
	put16(b, 2, 3)
	put16(b, 4, 0x4000)
	put16(b, 10, 500)
	put16(b, 12, 20)
	vals := []int{0x7fff, 0x7ffe, 1000, -1000, 5, -7, 1, 2, 3, 4, 5, 6}
	for i, v := range vals {
		put16(b, 16+2*i, v)
	}
	got := decodeEffect(b)
	want := Effect{
		Type: ffSpring, ID: 3, Direction: 0x4000, ReplayLength: 500, ReplayDelay: 20,
		Condition: []Condition{
			{RightSaturation: 0x7fff, LeftSaturation: 0x7ffe, RightCoeff: 1000, LeftCoeff: -1000, Deadband: 5, Center: -7},
			{RightSaturation: 1, LeftSaturation: 2, RightCoeff: 3, LeftCoeff: 4, Deadband: 5, Center: 6},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}

	b = make([]byte, 48)
	put16(b, 0, ffConstant)
	put16(b, 16, -9000)
	put16(b, 18, 100)
	put16(b, 24, 7)
	got = decodeEffect(b)
	if got.Constant == nil || *got.Constant != (Constant{Level: -9000, Envelope: Envelope{AttackLength: 100, FadeLevel: 7}}) {
		t.Errorf("constant: got %+v", got.Constant)
	}

	b = make([]byte, 48)
	put16(b, 0, ffPeriodic)
	put16(b, 16, ffSine)
	put16(b, 18, 50)
	put16(b, 20, -300)
	put16(b, 26, 11)
	got = decodeEffect(b)
	if got.Periodic == nil || *got.Periodic != (Periodic{Waveform: ffSine, Period: 50, Magnitude: -300, Envelope: Envelope{AttackLength: 11}}) {
		t.Errorf("periodic: got %+v", got.Periodic)
	}
}

func TestFFEvent(t *testing.T) {
	for _, tc := range []struct {
		code  uint16
		value int32
		kind  string
	}{
		{ffGain, 0xc000, FFGain},
		{ffAutocenter, 0, FFAutocenter},
		{2, 1, FFPlay},
		{2, 0, FFStop},
	} {
		m := ffEvent(tc.code, tc.value)
		if m.Kind != tc.kind {
			t.Errorf("%#x=%d: kind %s, want %s", tc.code, tc.value, m.Kind, tc.kind)
		}
		if tc.kind == FFStop && m.Value != nil {
			t.Errorf("stop carries a value")
		}
	}
}
