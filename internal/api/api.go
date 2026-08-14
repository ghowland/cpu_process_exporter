// Package api serves the metrics endpoint and a small set of read
// endpoints for debugging.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/example/process_exporter/internal/aggregate"
	"github.com/example/process_exporter/internal/config"
	"github.com/example/process_exporter/internal/metrics"
)

// Deps holds the live components the handlers read.
type Deps struct {
	Config    func() *config.Config
	Metrics   *metrics.Registry
	Version   string
	StartedAt time.Time
}

// Server serves the HTTP surface.
type Server struct {
	cfg  config.ServerConfig
	deps Deps
	srv  *http.Server
}

// New creates a Server.
func New(cfg config.ServerConfig, d Deps) *Server {
	s := &Server{cfg: cfg, deps: d}
	s.srv = &http.Server{
		Addr:              cfg.Listen,
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s
}

// Run serves until ctx is cancelled, then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		err := s.srv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(sctx)
		return nil
	}
}

func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle(s.cfg.MetricsPath, s.deps.Metrics.Handler())
	mux.HandleFunc("/groups", s.handleGroups)
	mux.HandleFunc("/config", s.handleConfig)
	mux.HandleFunc("/stats", s.handleStats)
	mux.HandleFunc("/livez", s.handleLivez)
	mux.HandleFunc("/readyz", s.handleReadyz)
	mux.HandleFunc("/", s.handleIndex)
	return mux
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"version":    s.deps.Version,
		"started_at": s.deps.StartedAt,
		"uptime":     time.Since(s.deps.StartedAt).Truncate(time.Second).String(),
		"endpoints": []string{
			s.cfg.MetricsPath, "/groups", "/config", "/stats", "/livez", "/readyz",
		},
	})
}

// handleGroups returns the current snapshot as JSON, sorted by CPU.
//
// It is the debugging equivalent of top, and answers "what is this
// exporter actually seeing" without going through Prometheus.
func (s *Server) handleGroups(w http.ResponseWriter, r *http.Request) {
	snap := s.deps.Metrics.Snapshot()
	if snap == nil {
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]string{"error": "no scan has completed yet"})
		return
	}

	list := make([]*aggregate.Sample, len(snap.List))
	copy(list, snap.List)

	switch r.URL.Query().Get("sort") {
	case "rss", "memory":
		sort.Slice(list, func(i, j int) bool { return list[i].RSSBytes > list[j].RSSBytes })
	case "procs":
		sort.Slice(list, func(i, j int) bool { return list[i].NumProcs > list[j].NumProcs })
	case "fds":
		sort.Slice(list, func(i, j int) bool { return list[i].OpenFDs > list[j].OpenFDs })
	case "name":
		sort.Slice(list, func(i, j int) bool { return list[i].Key.Name < list[j].Key.Name })
	default:
		sort.Slice(list, func(i, j int) bool {
			a := list[i].Accum.UTimeSeconds + list[i].Accum.STimeSeconds
			b := list[j].Accum.UTimeSeconds + list[j].Accum.STimeSeconds
			return a > b
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"scan_at":  snap.ScanAt,
		"duration": snap.Duration.String(),
		"groups":   list,
	})
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.deps.Config())
}

// handleStats returns the scan counters: processes seen, ignored,
// vanished, denied, and the exporter's own CPU use.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	snap := s.deps.Metrics.Snapshot()
	if snap == nil {
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]string{"error": "no scan has completed yet"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"scan_at":        snap.ScanAt,
		"scan_number":    snap.ScanNumber,
		"duration":       snap.Duration.String(),
		"self_cpu_secs":  snap.SelfCPU,
		"procs_total":    snap.ProcsTotal,
		"procs_scanned":  snap.ProcsScanned,
		"procs_ignored":  snap.ProcsIgnored,
		"procs_vanished": snap.ProcsVanished,
		"procs_denied":   snap.ProcsDenied,
		"read_errors":    snap.ReadErrs,
		"groups":         len(snap.Groups),
		"names_seen":     snap.NamesSeen,
		"state_entries":  snap.StateEntries,
	})
}

func (s *Server) handleLivez(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReadyz reports ready once at least two scans have completed,
// because counter values do not exist until the second scan produces
// the first deltas.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	snap := s.deps.Metrics.Snapshot()
	ready := snap != nil && snap.ScanNumber >= 2
	code := http.StatusOK
	if !ready {
		code = http.StatusServiceUnavailable
	}
	n := uint64(0)
	if snap != nil {
		n = snap.ScanNumber
	}
	writeJSON(w, code, map[string]any{
		"ready":  ready,
		"scans":  n,
		"reason": "counter values require at least two scans",
	})
}

// writeJSON encodes a value with the correct content type.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

