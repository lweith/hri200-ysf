package wiresxnet

import (
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/transform"
)

// WIRES-X carries directory text (room names, cities, descriptions) as raw bytes
// in the sending operator's legacy Windows codepage, with no charset marker:
// Shift-JIS from Japan, CP1252/Latin-1 from the Americas and Europe, plain ASCII
// from the US/UK, and UTF-8 from newer clients. Yaesu's GUI renders everyone
// through one fixed codepage, so anything else turns to mojibake. We can do
// better because each record carries a country field the renderer ignores.
//
// decodeText returns raw as UTF-8. Valid UTF-8 (which includes all ASCII) is
// kept as-is. Otherwise the country picks the legacy codepage: Japan is
// Shift-JIS; everything else is CP1252, which maps all 256 byte values and so
// never fails. A Shift-JIS decode that hits an invalid sequence falls back to
// CP1252 rather than returning an error.
func decodeText(raw []byte, country string) string {
	if utf8.Valid(raw) {
		return string(raw)
	}
	if strings.EqualFold(strings.TrimSpace(country), "Japan") {
		if s, _, err := transform.Bytes(japanese.ShiftJIS.NewDecoder(), raw); err == nil {
			return string(s)
		}
	}
	s, _, err := transform.Bytes(charmap.Windows1252.NewDecoder(), raw)
	if err != nil {
		return string(raw) // unreachable for CP1252, but never lose the bytes
	}
	return string(s)
}
