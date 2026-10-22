package main

import (
	"bytes"
	"testing"
)

// The stream hands a late-joining viewer the encoder's SPS/PPS plus the first
// IDR it sees. This is the unit that decides which frames those are, and getting
// it wrong is invisible except as a black picture on the client.
func TestInspectAccessUnit(t *testing.T) {
	sps := []byte{0, 0, 1, 0x67, 0x64, 0x00, 0x32}
	pps := []byte{0, 0, 1, 0x68, 0xeb, 0xec}
	sei := []byte{0, 0, 1, 0x06, 0x05, 0x01}
	idr := []byte{0, 0, 1, 0x65, 0x88, 0x84}
	delta := []byte{0, 0, 1, 0x41, 0x9a, 0x02}
	join := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

	// A key access unit: SPS + PPS + SEI + IDR slice.
	s, p, isIDR := inspectAccessUnit(join(sps, pps, sei, idr))
	if !isIDR {
		t.Error("IDR slice not detected")
	}
	if !bytes.Equal(s, sps) || !bytes.Equal(p, pps) {
		t.Errorf("parameter sets not captured: sps=%v pps=%v", s, p)
	}

	// A delta access unit must report no IDR so the gate can skip it.
	if _, _, isIDR := inspectAccessUnit(join(delta)); isIDR {
		t.Error("delta frame reported as a key frame")
	}

	// An access unit with no parameter sets must not invent any.
	s2, p2, _ := inspectAccessUnit(join(delta, idr))
	if s2 != nil || p2 != nil {
		t.Errorf("invented parameter sets: sps=%v pps=%v", s2, p2)
	}

	if _, _, isIDR := inspectAccessUnit(nil); isIDR {
		t.Error("empty input reported a key frame")
	}
}
