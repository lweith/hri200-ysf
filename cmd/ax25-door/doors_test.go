package main

import "testing"

func TestParseDoorsSingle(t *testing.T) {
	ds, err := parseDoors("kn6fln-11", []string{"/usr/games/wump"}, nil, []string{"W8HHF/R wumpus door."})
	if err != nil {
		t.Fatal(err)
	}
	d := ds["KN6FLN-11"]
	if d == nil || d.argv[0] != "/usr/games/wump" || d.banner != "W8HHF/R wumpus door." {
		t.Errorf("door: %+v", d)
	}
}

func TestParseDoorsSeveral(t *testing.T) {
	ds, err := parseDoors("", nil,
		[]string{"KN6FLN-11 /usr/games/wump", "KN6FLN-12 /usr/games/trek"},
		[]string{"KN6FLN-12 Welcome aboard.", "kn6fln-11 Wumpus."})
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 2 || ds["KN6FLN-12"].banner != "Welcome aboard." || ds["KN6FLN-11"].banner != "Wumpus." {
		t.Errorf("doors: %+v %+v", ds["KN6FLN-11"], ds["KN6FLN-12"])
	}
	if _, err := parseDoors("", nil, []string{"KN6FLN-11 /a", "KN6FLN-12 /b"}, []string{"Nobody's."}); err == nil {
		t.Error("a banner for no door was taken")
	}
	if _, err := parseDoors("", nil, []string{"KN6FLN-11 /a", "kn6fln-11 /b"}, nil); err == nil {
		t.Error("two doors on one callsign were taken")
	}
}
