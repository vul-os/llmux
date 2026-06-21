package server

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"time"
)

type reqIDKey int

const requestIDKey reqIDKey = 0

// requestIDFrom returns the request id from context, or "".
func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// newLogger builds a slog logger at the configured level.
func newLogger(level string) *slog.Logger {
	var lv slog.Level
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lv}))
}

// observeMW assigns/propagates a request id (X-Request-ID) and emits a
// structured access-log line per request. It wraps the response to capture
// status. Health and metrics scrapes are not logged to keep noise down.
func (s *Server) observeMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = genID("req_")
		}
		w.Header().Set("X-Request-ID", id)
		r = r.WithContext(context.WithValue(r.Context(), requestIDKey, id))

		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)

		if r.URL.Path == "/health" || r.URL.Path == "/metrics" {
			return
		}
		s.log.Info("request",
			"request_id", id,
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"dur_ms", time.Since(start).Milliseconds(),
		)
	})
}
