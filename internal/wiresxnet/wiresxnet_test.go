package wiresxnet

import (
	"bytes"
	"strings"
	"testing"
)

// A real node poll from captures/2026-11-13/wx-login.pcapng (8310), as wire
// bytes: STX, length 0026, body, sum 88, ETX. Ground truth for the envelope and
// the 8-bit body sum.
var realPoll = append(append([]byte{0x02}, []byte("0026&L0020@p0000000A0700304010309W8HHF-RPT88")...), 0x03)

func TestReadRealPoll(t *testing.T) {
	f, err := NewReader(bytes.NewReader(realPoll)).ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	want := "&L0020@p0000000A0700304010309W8HHF-RPT"
	if string(f.Body) != want {
		t.Fatalf("body = %q, want %q", f.Body, want)
	}
	// Re-encoding must reproduce the exact wire bytes (checks sum formatting).
	if got := f.Encode(); !bytes.Equal(got, realPoll) {
		t.Fatalf("Encode round-trip:\n got %q\nwant %q", got, realPoll)
	}
}

func TestReadBadSum(t *testing.T) {
	bad := append(append([]byte{0x02}, []byte("0026&L0020@p0000000A0700304010309W8HHF-RPT00")...), 0x03)
	_, err := NewReader(bytes.NewReader(bad)).ReadFrame()
	if err == nil {
		t.Fatal("expected ErrBadFrame on wrong sum")
	}
}

func TestParseBody(t *testing.T) {
	m, err := ParseBody([]byte("&L0020@p0000000A0700304010309W8HHF-RPT"))
	if err != nil {
		t.Fatalf("ParseBody: %v", err)
	}
	if m.Type != TypeNodePoll {
		t.Errorf("type = %c, want L", m.Type)
	}
	if m.Verb != "@p" {
		t.Errorf("verb = %q, want @p", m.Verb)
	}
}

func TestFieldsTLV(t *testing.T) {
	// node id (0A,07), long call (03,09), short call (04,05)
	in := []byte("0A0700304010309W8HHF-RPT0405W8HHF")
	var got []Field
	if err := Fields(in, func(f Field) bool { got = append(got, f); return true }); err != nil {
		t.Fatalf("Fields: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d fields, want 3", len(got))
	}
	if got[0].Tag != TagNodeID || string(got[0].Value) != "0030401" {
		t.Errorf("field 0 = %+v", got[0])
	}
	if got[1].Tag != TagName || string(got[1].Value) != "W8HHF-RPT" {
		t.Errorf("field 1 = %+v", got[1])
	}
	if got[2].Tag != TagShort || string(got[2].Value) != "W8HHF" {
		t.Errorf("field 2 = %+v", got[2])
	}
}

// A real Shift-JIS room description from the capture must decode to UTF-8 when
// the record's country is Japan, and CP1252 bytes must survive for others.
func TestDecodeText(t *testing.T) {
	sjis := []byte{0x83, 0x8d, 0x81, 0x5b, 0x83, 0x4a, 0x83, 0x8b} // "ローカル"
	if got := decodeText(sjis, "Japan"); got != "ローカル" {
		t.Errorf("Shift-JIS decode = %q, want ローカル", got)
	}
	// ASCII is valid UTF-8 and passes through untouched.
	if got := decodeText([]byte("K5COW Net"), "USA"); got != "K5COW Net" {
		t.Errorf("ASCII = %q", got)
	}
	// CP1252 0xF3 is ó; no country hint, so CP1252 is used.
	if got := decodeText([]byte("Le\xf3n"), "Mexico"); got != "León" {
		t.Errorf("CP1252 decode = %q, want León", got)
	}
	// Valid UTF-8 from a modern client is kept as-is even for Japan.
	if got := decodeText([]byte("東京"), "Japan"); got != "東京" {
		t.Errorf("UTF-8 passthrough = %q", got)
	}
}

// makeRR builds an RR record payload with the captured header shape (room 28054,
// node 30401, token), a subtype byte, and a tail. The header up to the token is
// ASCII, matching real frames; HandleRR locates the token and reads the subtype.
func makeRR(sub byte, tail string) []byte {
	p := []byte("RR000000A0070028054B7070030401BE04B42A")
	p = append(p, sub)
	return append(p, tail...)
}

// TestVoiceTalker drives the talker metrics with record shapes taken from
// captures/2026-11-13/wx-voice.pcapng (room AMERICA-KC-WIDE, talker NA9PL-RPT).
func TestVoiceTalker(t *testing.T) {
	m := NewMetrics()
	v := NewVoiceState(m, "")

	v.HandleRR(makeRR('Q', "< AMERICA-KC-WIDE #28054 by KCWIDE #18054 >"))
	if v.Room() != "AMERICA-KC-WIDE" {
		t.Fatalf("room = %q, want AMERICA-KC-WIDE", v.Room())
	}
	// Repeater NA9PL-RPT keys up carrying operator WK9W; credit WK9W, not the
	// node. Then five voice frames, then a keydown (t~).
	v.HandleRR(makeRR(0x10, "\x03\x01\x05\x18\x00v#NA9PL-RPT $WK9W ~N5TND-ND"))
	for i := 0; i < 5; i++ {
		v.HandleRR(makeRR(0x00, "\x01\x00\x98 ambe"))
	}
	v.HandleRR(makeRR(0x10, "\x03\x01\x05\x10\x02\x00t~N5TND-ND  ~W3AOK-ND")) // keydown
	// The link US-KCWIDE relays operator KD3EAC from a bridged network: credit
	// KD3EAC, not US-KCWIDE. One frame, then keydown.
	v.HandleRR(makeRR(0x10, "\x03\x01\x05\x18\x00v#US-KCWIDE $KD3EAC ~N5TND-ND"))
	v.HandleRR(makeRR(0x00, "ambe"))
	v.HandleRR(makeRR(0x10, "\x03\x01\x05\x10\x02\x00t~N5TND-ND")) // keydown

	var buf bytes.Buffer
	m.WriteProm(&buf)
	s := buf.String()
	for _, want := range []string{
		`wiresx_talker_frames_total{room="AMERICA-KC-WIDE",callsign="WK9W"} 5`,
		`wiresx_talker_frames_total{room="AMERICA-KC-WIDE",callsign="KD3EAC"} 1`,
		`wiresx_talker_overs_total{room="AMERICA-KC-WIDE",callsign="WK9W"} 1`,
		`wiresx_talker_overs_total{room="AMERICA-KC-WIDE",callsign="KD3EAC"} 1`,
		`wiresx_conversation_total{room="AMERICA-KC-WIDE",from="WK9W",to="KD3EAC"} 1`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing metric:\n  %s\nin:\n%s", want, s)
		}
	}
	// The relaying nodes must never appear as talkers.
	for _, bad := range []string{`callsign="NA9PL-RPT"`, `callsign="US-KCWIDE"`} {
		if strings.Contains(s, "wiresx_talker_frames_total{room=\"AMERICA-KC-WIDE\","+bad) {
			t.Errorf("relay node credited as talker: %s\n%s", bad, s)
		}
	}
	// After the final keydown no one is keyed.
	if strings.Contains(s, "wiresx_current_talker{") {
		t.Errorf("current_talker set after keydown:\n%s", s)
	}
}

// A directory page built from two faithful records (a room and a plain node),
// behind a verb+sequence header that ingest must skip.
func TestDirectoryIngest(t *testing.T) {
	header := "@E00010001"
	room := "FE0200" +
		"0A070004480" + // node id 0004480 (tag 0A, len 07)
		"040ABloomer-RM" + // short name, len 0x0A
		"0611Bloomer,Wisconsin" + // city, len 0x11
		"0703USA" +
		"2C040003" // members = 3
	node := "FE0200" +
		"0A070030401" + // node id 0030401 (tag 0A, len 07)
		"0409W8HHF-RPT" + // name, len 0x09
		"0606TOLEDO" // city, len 0x06
	page := []byte(header + room + node)

	m := NewMetrics()
	d := NewDirectory(m)
	n, err := d.IngestPage(page)
	if err != nil {
		t.Fatalf("IngestPage: %v", err)
	}
	if n != 2 {
		t.Fatalf("ingested %d records, want 2", n)
	}

	r, ok := d.Lookup("0004480")
	if !ok || r.Name != "Bloomer-RM" || r.City != "Bloomer,Wisconsin" || r.Members != 3 || !r.IsRoom {
		t.Fatalf("room lookup = %+v ok=%v", r, ok)
	}
	nd, ok := d.Lookup("0030401")
	if !ok || nd.Name != "W8HHF-RPT" || nd.IsRoom {
		t.Fatalf("node lookup = %+v ok=%v", nd, ok)
	}
	if len(d.Rooms()) != 1 || len(d.Nodes()) != 1 {
		t.Fatalf("rooms=%d nodes=%d, want 1 and 1", len(d.Rooms()), len(d.Nodes()))
	}

	// The room occupancy gauge must have been exported.
	var buf bytes.Buffer
	if err := m.WriteProm(&buf); err != nil {
		t.Fatalf("WriteProm: %v", err)
	}
	if !strings.Contains(buf.String(), `wiresx_room_members{room="0004480",name="Bloomer-RM"} 3`) {
		t.Fatalf("missing room gauge in:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "wiresx_directory_entries 2") {
		t.Fatalf("missing directory size in:\n%s", buf.String())
	}
}
