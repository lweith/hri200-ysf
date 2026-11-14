package hri

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// Wrap frames message text with SOH and EOT, ready to write.
func Wrap(text string) []byte {
	b := make([]byte, 0, len(text)+2)
	b = append(b, soh)
	b = append(b, text...)
	return append(b, eot)
}

// D1 builds a "D1" message: command letter, four hex digits of length, payload.
func D1(cmd byte, payload string) string {
	return fmt.Sprintf("D1%c%04X%s", cmd, len(payload), payload)
}

// Ack is the reply to a D1E or D1F from WIRES-X, and WIRES-X's reply to a D1G
// from the radio: the command with a one-character payload "0".
func Ack(cmd byte) string { return D1(cmd, "0") }

// IsAck reports whether a message is an acknowledgement (one-character payload).
func IsAck(text string) bool {
	m, ok := Parse(text)
	return ok && len(m.Payload) == 1
}

// State is the five-character radio state in D1P and D1C payloads, such as
// "04000" idle, "0A038" receiving, "00004" transmitting.
type State string

// Radio states seen in captures, in the order the HRI-200 reports them.
var (
	// RxStart is reported, one after another, when the radio starts receiving.
	RxStart = []State{"0A010", "2A010", "0A010", "0A018", "0A038"}
	// RxEnd is reported when it stops receiving, ending idle.
	RxEnd = []State{"06008", "23008", "03008", "04000"}
	// TxStart is reported when WIRES-X starts a transmission (after the D1F header).
	TxStart = []State{"20000", "00000", "00004"}
	// TxEnd is reported when the transmission's frames stop, ending idle.
	TxEnd = []State{"00002", "24002", "04000"}
)

// stateSuffix follows the state in every D1P and D1C payload.
const stateSuffix = "R0OR40"

// StatePayload is a D1P or D1C payload for a state.
func StatePayload(s State) string { return string(s) + stateSuffix }

// ParseState returns the state of a D1P or D1C message.
func ParseState(m Message) (State, bool) {
	if (m.Cmd != 'P' && m.Cmd != 'C') || len(m.Payload) != 11 {
		return "", false
	}
	return State(m.Payload[:5]), true
}

// Receiving reports whether a state is one of the receive states ("?A???").
func (s State) Receiving() bool { return len(s) == 5 && s[1] == 'A' }

// Idle reports whether a state looks idle: not receiving, not transmitting.
func (s State) Idle() bool {
	return len(s) == 5 && s[0] == '0' && s[1] != 'A' && s[1] != '0' && s[2:] == "000"
}

// VoicePayload is a D1R or D1E payload: "2", the counter, "00", the voice blocks as hex.
func VoicePayload(counter uint16, blocks [5][13]byte) string {
	var raw [65]byte
	for i := range 5 {
		copy(raw[13*i:], blocks[i][:])
	}
	return fmt.Sprintf("2%04X00%s", counter, strings.ToUpper(hex.EncodeToString(raw[:])))
}

// RadioPrefix starts the D1H and D1G payloads of a V/D Mode 2 signal the radio
// receives off the air.
const RadioPrefix = "21002000"

// CallsignPayload is a D1H payload: prefix, block (1-3), repeat, 20 characters.
func CallsignPayload(prefix string, block, repeat int, text [20]byte) string {
	return fmt.Sprintf("%s%d%d%s", prefix, block, repeat, text[:])
}

// DataPayload is a D1G payload: prefix, CSD1, CSD2, CSD3 and the 20 bytes of
// data channel from frames 6 and 7 (DT1, DT2) as hex.
func DataPayload(prefix string, csd1, csd2, csd3 [20]byte, dt [20]byte) string {
	return prefix + string(csd1[:]) + string(csd2[:]) + string(csd3[:]) + strings.ToUpper(hex.EncodeToString(dt[:]))
}

// RadioDataPayload is a D1G payload as the HRI-200 sends it for a station its
// radio receives: as DataPayload, but with the DT data only through its end,
// an 03 and the 8-bit sum of every byte before the sum (see DTLen). A radio
// without a position sends a short DT ("22 61 5F 33", 7 bytes with the
// counter, end and sum) and the HRI-200 a D1G of 0x52 characters; with one,
// 20 bytes and 0x6C. Captured 2026-11-14 from an FT-5D: WIRES-X shows a
// reception whose short DT is padded out to 20 bytes as local, but does not
// send it to the room.
func RadioDataPayload(prefix string, csd1, csd2, csd3 [20]byte, dt [20]byte) string {
	return prefix + string(csd1[:]) + string(csd2[:]) + string(csd3[:]) +
		strings.ToUpper(hex.EncodeToString(dt[:DTLen(dt)]))
}

// DTLen is the length of the DT data in dt: through the last 03 whose next
// byte is the 8-bit sum of every byte up to and including that 03, or all 20
// bytes if there is none.
func DTLen(dt [20]byte) int {
	for i := len(dt) - 2; i >= 1; i-- {
		if dt[i] != 0x03 {
			continue
		}
		var sum byte
		for _, c := range dt[:i+1] {
			sum += c
		}
		if dt[i+1] == sum {
			return i + 2
		}
	}
	return len(dt)
}

// Data is a D1G message: the callsign blocks and the DT data of frames 6 and 7.
type Data struct {
	CSD1, CSD2, CSD3 [20]byte
	DT               [20]byte // zero after the data, if it was short
}

// ParseData parses a D1G from either side, or a D1F from WIRES-X, which uses
// this form instead of the 0x52 header for some transmissions and repeats it
// every few seconds while they last: 0x6C characters, or fewer when the DT
// data is short (a D1G from the radio, see RadioDataPayload).
func ParseData(m Message) (Data, bool) {
	var d Data
	n := len(m.Payload)
	if (m.Cmd != 'G' && m.Cmd != 'F') || n > 0x6C || n < 70 || n%2 != 0 {
		return d, false
	}
	copy(d.CSD1[:], m.Payload[8:28])
	copy(d.CSD2[:], m.Payload[28:48])
	copy(d.CSD3[:], m.Payload[48:68])
	dt, err := hex.DecodeString(m.Payload[68:])
	if err != nil {
		return d, false
	}
	copy(d.DT[:], dt)
	return d, true
}

// CommandPayload is the D1G payload of a WIRES-X command the radio received
// off the air: "210010" and the command's first byte as hex, the three
// callsign blocks, and the command's bytes as hex. (As captured: "2100107F"
// for a node info request starting 7F, "21001000" for a room list starting 00.)
func CommandPayload(csd1, csd2, csd3 [20]byte, data []byte) string {
	var first byte
	if len(data) > 0 {
		first = data[0]
	}
	return fmt.Sprintf("210010%02X", first) + string(csd1[:]) + string(csd2[:]) + string(csd3[:]) +
		strings.ToUpper(hex.EncodeToString(data))
}

// Command is a D1G or D1F carrying WIRES-X data: an eight-character prefix,
// the three callsign blocks, and the data as hex, of any length.
type Command struct {
	Prefix           string
	CSD1, CSD2, CSD3 [20]byte
	Data             []byte
}

// ParseCommand parses a D1G or D1F carrying WIRES-X data, such as WIRES-X's
// reply to a command ("20211000", node callsign blocks, the reply).
func ParseCommand(m Message) (Command, bool) {
	var c Command
	p := m.Payload
	if (m.Cmd != 'G' && m.Cmd != 'F') || len(p) < 70 || (len(p)-68)%2 != 0 {
		return c, false
	}
	data, err := hex.DecodeString(p[68:])
	if err != nil {
		return c, false
	}
	c.Prefix = p[:8]
	copy(c.CSD1[:], p[8:28])
	copy(c.CSD2[:], p[28:48])
	copy(c.CSD3[:], p[48:68])
	c.Data = data
	return c, true
}

// Key is what pairs a request from WIRES-X with the HRI-200's reply: "D1" and
// the command letter for D1 messages, otherwise the first character.
func Key(text string) string {
	if len(text) >= 3 && text[0] == 'D' && text[1] == '1' {
		return text[:3]
	}
	if text == "" {
		return ""
	}
	return text[:1]
}

// ReplyKey is the key of the reply a request expects: polls ("P") are answered
// with "B", everything else with the same key.
func ReplyKey(request string) string {
	k := Key(request)
	if k == "P" {
		return "B"
	}
	return k
}
