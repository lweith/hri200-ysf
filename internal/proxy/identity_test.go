package proxy

import (
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// W8HHF/R's HRI-200 and DR-1X, as they answered (tmra's cache, 2026-11-14),
// but for the HRI-200's serial number and the date it was made: not a unit's
// (not hex, and before the HRI-200 was made), as WIRES-X registers by them.
var capturedR6423 = "R0" + strings.ToUpper(hex.EncodeToString([]byte("00000,00000,NOTAUNIT,19990101000000")))

const (
	capturedVersion  = "D1V0030DR-1X           EXP A1 1.10Q201608233R0OR4_&0C00"
	capturedSettings = "D1M006BE000146.23500+000.000000210302310020146.83500+000.0000002103023100210W8HHF/R   0180002000012010000105020500"
)

func TestSerial(t *testing.T) {
	serial, made, ok := Serial(capturedR6423)
	if !ok || serial != "NOTAUNIT" || made != "1999-01-01 00:00:00" {
		t.Errorf("Serial: %q %q %v", serial, made, ok)
	}
	if _, _, ok := Serial("R0303030"); ok {
		t.Error("a short R6423 answer parsed")
	}
	c := &Cache{Replies: map[string]string{}}
	if got := c.Describe(); got != "no HRI-200 known yet" {
		t.Errorf("Describe, empty: %q", got)
	}
	c.Learn("R6423", capturedR6423)
	c.Learn("D1V0000", capturedVersion)
	if got, want := c.Describe(), "HRI-200 serial NOTAUNIT (made 1999-01-01 00:00:00), DR-1X EXP A1 1.10Q"; got != want {
		t.Errorf("Describe: %q", got)
	}
}

// Every settings write WIRES-X made and the DR-1X's echo of it (tmra's cache,
// 2026-11-14): its own four hang times, and the mode writes.
var settingsPairs = [][2]string{
	{"D1M006BE000146.23500+000.000000210302300020146.83500+000.0000002103023000200W8HHF/R   0180002000012010000105020500",
		"D1M006BE000146.23500+000.000000210302300020146.83500+000.0000002103023100200W8HHF/R   0180002000012010000105020500"},
	{"D1M006BE000146.23500+000.000000210302300020146.83500+000.0000002103023000210W8HHF/R   0180000000012010000105020500",
		"D1M006BE000146.23500+000.000000210302300020146.83500+000.0000002103023100210W8HHF/R   0180000000012010000105020500"},
	{"D1M006BE000146.23500+000.000000210302300020146.83500+000.0000002103023000210W8HHF/R   0180001000012010000105020500",
		"D1M006BE000146.23500+000.000000210302300020146.83500+000.0000002103023100210W8HHF/R   0180001000012010000105020500"},
	{"D1M006BE000146.23500+000.000000210302300020146.83500+000.0000002103023000210W8HHF/R   0180002000012010000105020500",
		"D1M006BE000146.23500+000.000000210302300020146.83500+000.0000002103023100210W8HHF/R   0180002000012010000105020500"},
	{"D1M006BE000146.23500+000.000000210302300020146.83500+000.0000002103023000210W8HHF/R   0600002000012010000105020500",
		"D1M006BE000146.23500+000.000000210302300020146.83500+000.0000002103023100210W8HHF/R   0600002000012010000105020500"},
	{SetMode(analogMode),
		"D1M006BE000146.23500+000.000000210302310020146.83500+000.0000002103023100200W8HHF/R   0180002000012010000105020500"},
	{SetMode(digitalMode), capturedSettings},
}

func TestSettingsEcho(t *testing.T) {
	for i, p := range settingsPairs {
		if got := settingsEcho(capturedSettings, p[0]); got != p[1] {
			t.Errorf("pair %d:\n write %s\n  got %s\n want %s", i, p[0], got, p[1])
		}
	}
	// A hang-time write goes over what the last write left.
	cur := settingsEcho(capturedSettings, settingsPairs[1][0])
	got := settingsEcho(cur, ModeWrite(digitalMode, "0600"))
	if want := settingsPairs[1][1][:sqltAt+7] + "0600" + settingsPairs[1][1][sqltAt+11:]; got != want {
		t.Errorf("hang write over a full one:\n  got %s\n want %s", got, want)
	}
}

// The cache keeps the radio's last full settings echo, not a write's F's.
func TestCacheKeepsSettings(t *testing.T) {
	c := &Cache{Replies: map[string]string{}}
	c.NoteMode(settingsPairs[0][1])
	c.NoteMode(SetMode(digitalMode)) // not an echo: ignored
	if got := c.LastSettings(); got != settingsPairs[0][1] {
		t.Errorf("LastSettings: %s", got)
	}
	old := &Cache{Replies: map[string]string{}, Mode: "1"} // from before Settings
	old.Learn(SetMode(digitalMode), capturedSettings)
	if got := old.LastSettings(); got != capturedSettings {
		t.Errorf("LastSettings, old cache: %s", got)
	}
}

// With the HRI-200 gone, WIRES-X's startup is answered from what the cache
// learned of it, and a settings write never seen is echoed as the radio
// would.
func TestFaceRunsWithoutHRI200(t *testing.T) {
	c := &Cache{Replies: map[string]string{}}
	c.Learn("M10", "M10")
	c.Learn("R6423", capturedR6423)
	c.Learn("D1V0000", capturedVersion)
	c.NoteMode(capturedSettings)
	f := &Face{Cache: c, Log: t.Logf}
	for _, x := range [][2]string{
		{"M10", "M10"},
		{"R6423", capturedR6423},
		{"D1V0000", capturedVersion},
		{SetMode(digitalMode), capturedSettings},
		{settingsPairs[4][0], settingsPairs[4][1]},
		{SetMode(analogMode), settingsPairs[4][1][:74] + "0" + settingsPairs[4][1][75:]},
	} {
		got := texts(f.Request(x[0], at(0)))
		if len(got) != 1 || got[0] != x[1] {
			t.Errorf("%.20s: %q, want %q", x[0], got, x[1])
		}
	}
}

func TestMasterChecksSerial(t *testing.T) {
	var log []string
	m, _ := newMaster(t)
	m.Log = func(format string, args ...any) { log = append(log, fmt.Sprintf(format, args...)) }
	m.Cache.Learn("R6423", capturedR6423)
	other := "R0" + strings.ToUpper(hex.EncodeToString([]byte("00000,00000,NOTUNIT2,19990202000000")))
	m.send("R6423", at(0))
	m.FromRadio(other, at(10))
	if len(log) == 0 || !strings.Contains(log[len(log)-1], "A DIFFERENT HRI-200: serial NOTUNIT2 (made 1999-02-02 00:00:00); WIRES-X has been told of NOTAUNIT") {
		t.Errorf("log: %q", log)
	}
	m.send("R6423", at(100))
	m.FromRadio(other, at(110))
	if !strings.Contains(log[len(log)-1], "serial NOTUNIT2 (made 1999-02-02 00:00:00), the one WIRES-X knows") {
		t.Errorf("log: %q", log)
	}
}

// A radio that stays sick through the unkey polls gets WIRES-X's startup
// again: what brought the wedged radio back at 06:53.
func TestSickRadioGetsStartupAgain(t *testing.T) {
	m, r := newMaster(t)
	for i := range 30 {
		m.Tick(at(1000 * i)) // it never answers
	}
	sent := r.take()
	i := slices.Index(sent, "M10")
	if i < 0 || !slices.Contains(sent[i:], "R6423") || !slices.Contains(sent[i:], "D1V0000") {
		t.Fatalf("startup not re-run: %q", sent)
	}
	d1m := false
	for _, s := range sent[i:] {
		d1m = d1m || strings.HasPrefix(s, "D1M")
	}
	if !d1m {
		t.Errorf("no mode write to the sick radio: %q", sent[i:])
	}
}
