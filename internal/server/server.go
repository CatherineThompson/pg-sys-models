// Package server serves the static frontend and streams live frames to the
// browser over Server-Sent Events (spec §3 transport; SSE chosen for a
// stdlib-only implementation). Teaching mode needs no transport — it runs
// entirely in the browser — so this stream carries only live-mode frames.
package server

import (
	"encoding/json"
	"net/http"
	"sync"

	"github.com/catherinethompson/pg-sys-models/internal/frame"
)

// Server fans out frames to any number of connected SSE clients and serves the
// frontend assets from webDir.
type Server struct {
	webDir string

	mu   sync.Mutex
	subs map[chan frame.Frame]struct{}
	last *frame.Frame // most recent frame, replayed to new subscribers
}

// New builds a server that serves static files from webDir.
func New(webDir string) *Server {
	return &Server{webDir: webDir, subs: map[chan frame.Frame]struct{}{}}
}

// Broadcast delivers a frame to all connected clients. Non-blocking per client:
// a slow client drops frames rather than stalling the poller.
func (s *Server) Broadcast(f frame.Frame) {
	s.mu.Lock()
	s.last = &f
	for ch := range s.subs {
		select {
		case ch <- f:
		default:
		}
	}
	s.mu.Unlock()
}

// Pump forwards every frame from the poller's channel to connected clients.
func (s *Server) Pump(frames <-chan frame.Frame) {
	for f := range frames {
		s.Broadcast(f)
	}
}

// Handler wires the routes: SSE stream, a capabilities/health probe, and the
// static frontend.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/stream", s.handleStream)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.Handle("/", http.FileServer(http.Dir(s.webDir)))
	return mux
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := make(chan frame.Frame, 8)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	last := s.last
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}()

	// Replay the latest frame immediately so a fresh client renders without
	// waiting a full slow-loop interval.
	if last != nil {
		writeEvent(w, flusher, *last)
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case f := <-ch:
			writeEvent(w, flusher, f)
		}
	}
}

func writeEvent(w http.ResponseWriter, flusher http.Flusher, f frame.Frame) {
	b, err := json.Marshal(f)
	if err != nil {
		return
	}
	_, _ = w.Write([]byte("data: "))
	_, _ = w.Write(b)
	_, _ = w.Write([]byte("\n\n"))
	flusher.Flush()
}
