package wiresxnet

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
)

// A read API over the daemon's live state. The daemon already holds the
// authoritative directory and metrics in memory, so clients query it directly
// rather than through a separate datastore: JSON for point and list queries,
// Server-Sent Events for a live feed, and the Prometheus exposition for
// dashboards. Nothing here mutates node state.
//
// Routes (all GET):
//
//	/rooms            all rooms (JSON array of Entry)
//	/nodes            all non-room nodes
//	/rooms/{number}   one entry by number (404 if unknown)
//	/search?q=&limit= entries matching q in number/name/call/city/country
//	/talkers          per-talker totals and who is keyed now
//	/events           live events as text/event-stream (SSE)
//	/metrics          Prometheus exposition
type Server struct {
	Dir     *Directory
	Metrics *Metrics
	Broker  *Broker
}

// Handler returns the router. Mount it on an http.Server, or wrap it for auth,
// CORS or logging.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /rooms", s.handleRooms)
	mux.HandleFunc("GET /nodes", s.handleNodes)
	mux.HandleFunc("GET /rooms/{number}", s.handleRoom)
	mux.HandleFunc("GET /search", s.handleSearch)
	mux.HandleFunc("GET /talkers", s.handleTalkers)
	mux.HandleFunc("GET /events", s.handleEvents)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	return mux
}

func (s *Server) handleRooms(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.Dir.Rooms())
}

func (s *Server) handleNodes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.Dir.Nodes())
}

func (s *Server) handleRoom(w http.ResponseWriter, r *http.Request) {
	e, ok := s.Dir.Lookup(r.PathValue("number"))
	if !ok {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	writeJSON(w, e)
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	writeJSON(w, s.Dir.Search(r.URL.Query().Get("q"), limit))
}

func (s *Server) handleTalkers(w http.ResponseWriter, r *http.Request) {
	if s.Metrics == nil {
		writeJSON(w, []TalkerStat{})
		return
	}
	writeJSON(w, s.Metrics.Talkers())
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	if s.Metrics != nil {
		s.Metrics.WriteProm(w)
	}
}

// handleEvents streams live events as SSE until the client disconnects.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if s.Broker == nil {
		http.Error(w, `{"error":"events not available"}`, http.StatusServiceUnavailable)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush() // send headers now so the client's request returns

	ch, cancel := s.Broker.Subscribe()
	defer cancel()
	bw := bufio.NewWriter(w)

	for {
		select {
		case <-r.Context().Done():
			return
		case e, ok := <-ch:
			if !ok {
				return
			}
			b, err := json.Marshal(e)
			if err != nil {
				continue
			}
			fmt.Fprintf(bw, "event: %s\ndata: %s\n\n", e.Type, b)
			if bw.Flush() != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// writeJSON writes v as an indented JSON response.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
