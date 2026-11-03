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

// Data is a D1G message: the callsign blocks and the DT data of frames 6 and 7.
type Data struct {
	CSD1, CSD2, CSD3 [20]byte
	DT               [20]byte
}

// ParseData parses a payload of 0x6C characters: a D1G from either side, or a
// D1F from WIRES-X, which uses this form instead of the 0x52 header for some
// transmissions and repeats it every few seconds while they last.
func ParseData(m Message) (Data, bool) {
	var d Data
	if (m.Cmd != 'G' && m.Cmd != 'F') || len(m.Payload) != 0x6C {
		return d, false
	}
	copy(d.CSD1[:], m.Payload[8:28])
	copy(d.CSD2[:], m.Payload[28:48])
	copy(d.CSD3[:], m.Payload[48:68])
	dt, err := hex.DecodeString(m.Payload[68:108])
	if err != nil {
		return d, false
	}
	copy(d.DT[:], dt)
	return d, true
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
