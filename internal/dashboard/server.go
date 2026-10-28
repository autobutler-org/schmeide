package dashboard

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"mime"
	"net"
	"net/http"
)

// maxAskBody is the largest POST /api/ask body accepted, in bytes.
const maxAskBody = 64 << 10

//go:embed page.html
var page []byte

// Handler serves the dashboard page, GET /api/state, and POST /api/ask.
// When loopbackOnly is set, a request whose Host header is not localhost,
// 127.0.0.1, or ::1 is refused, so a web page on another site cannot reach
// the dashboard through DNS rebinding.
func Handler(view func() View, ask func(context.Context, Question) (string, error), loopbackOnly bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline' https://fonts.googleapis.com; font-src https://fonts.gstatic.com; connect-src 'self'")
		_, _ = w.Write(page)
	})
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, view())
	})
	mux.HandleFunc("POST /api/ask", func(w http.ResponseWriter, r *http.Request) {
		// A JSON content type makes a cross-site form post impossible
		// without a CORS preflight, which this server never approves.
		if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct != "application/json" {
			writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "send the question as application/json"})
			return
		}
		var q Question
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAskBody)).Decode(&q); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the request is not a question: " + err.Error()})
			return
		}
		answer, err := ask(r.Context(), q)
		switch {
		case errors.Is(err, ErrEmptyQuestion), errors.Is(err, ErrLongQuestion):
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		case errors.Is(err, ErrBusy):
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": err.Error()})
		case err != nil:
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		default:
			writeJSON(w, http.StatusOK, map[string]string{"answer": answer})
		}
	})
	if !loopbackOnly {
		return mux
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if host != "localhost" && !isLoopback(host) {
			http.Error(w, "the dashboard only answers on localhost; open it as http://localhost or http://127.0.0.1", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func isLoopback(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
