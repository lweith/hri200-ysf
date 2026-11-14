package wiresxnet

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	m := NewMetrics()
	d := NewDirectory(m)
	page := "@E00010001" +
		"FE0200" + "0A070004480" + "040ABloomer-RM" + "0611Bloomer,Wisconsin" + "0703USA" + "2C040003" +
		"FE0200" + "0A070030401" + "0409W8HHF-RPT" + "0606TOLEDO"
	if _, err := d.IngestPage([]byte(page)); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	m.CountOver("AMERICA-KC-WIDE", "", "NA9PL-RPT")
	for i := 0; i < 68; i++ {
		m.CountTalkerFrame("AMERICA-KC-WIDE", "NA9PL-RPT")
	}
	return &Server{Dir: d, Metrics: m, Broker: NewBroker()}
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec
}

func TestServerJSON(t *testing.T) {
	h := testServer(t).Handler()

	// /rooms/{number}
	rec := get(t, h, "/rooms/0004480")
	var e Entry
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("unmarshal room: %v (%s)", err, rec.Body)
	}
	if e.Name != "Bloomer-RM" || e.Members != 3 {
		t.Errorf("room = %+v", e)
	}

	// unknown number -> 404
	if rec := get(t, h, "/rooms/9999999"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown room code = %d, want 404", rec.Code)
	}

	// /search
	rec = get(t, h, "/search?q=bloomer")
	var hits []Entry
	if err := json.Unmarshal(rec.Body.Bytes(), &hits); err != nil || len(hits) != 1 {
		t.Fatalf("search = %v (%s)", err, rec.Body)
	}

	// /talkers
	rec = get(t, h, "/talkers")
	var ts []TalkerStat
	if err := json.Unmarshal(rec.Body.Bytes(), &ts); err != nil || len(ts) != 1 {
		t.Fatalf("talkers = %v (%s)", err, rec.Body)
	}
	if ts[0].Call != "NA9PL-RPT" || ts[0].Frames != 68 || ts[0].Overs != 1 {
		t.Errorf("talker = %+v", ts[0])
	}

	// /metrics is the Prometheus exposition
	if rec := get(t, h, "/metrics"); !strings.Contains(rec.Body.String(), "wiresx_directory_entries") {
		t.Errorf("metrics missing series:\n%s", rec.Body)
	}
}

func TestServerSSE(t *testing.T) {
	s := testServer(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}
	defer resp.Body.Close()

	// Give the subscription a moment to register, then publish.
	time.Sleep(50 * time.Millisecond)
	s.Broker.Publish(Event{Type: EventTalker, Room: "AMERICA-KC-WIDE", Call: "NA9PL-RPT"})

	sc := bufio.NewScanner(resp.Body)
	got := ""
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data: ") {
			got = strings.TrimPrefix(line, "data: ")
			break
		}
	}
	if !strings.Contains(got, `"call":"NA9PL-RPT"`) {
		t.Fatalf("SSE event = %q", got)
	}
}
