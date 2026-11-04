package usbmon

import (
	"encoding/binary"
	"slices"
	"testing"
)

func isoDesc(status int32, offset, length uint32) []byte {
	d := make([]byte, 16)
	binary.LittleEndian.PutUint32(d, uint32(status))
	binary.LittleEndian.PutUint32(d[4:], offset)
	binary.LittleEndian.PutUint32(d[8:], length)
	return d
}

func TestIsoData(t *testing.T) {
	var raw []byte
	raw = append(raw, isoDesc(0, 0, 2)...)
	raw = append(raw, isoDesc(-18, 4, 2)...) // failed: skipped
	raw = append(raw, isoDesc(0, 8, 3)...)
	raw = append(raw, isoDesc(0, 10, 9)...) // runs past the data: skipped
	raw = append(raw, []byte{1, 2, 0, 0, 3, 4, 0, 0, 5, 6, 7}...)
	e := Event{Type: 'C', XferType: Isochronous, Endpoint: 0x81}
	e.splitIso(raw, 4)
	if got := e.IsoData(); !slices.Equal(got, []byte{1, 2, 5, 6, 7}) {
		t.Errorf("IsoData = %v", got)
	}
	e.Endpoint = 0x01
	if e.IsoData() != nil {
		t.Error("returned data for an OUT endpoint")
	}
	var bad Event
	bad.splitIso(raw[:20], 4)
	if bad.Data != nil || bad.IsoData() != nil {
		t.Error("descriptors past the end not rejected")
	}
}
