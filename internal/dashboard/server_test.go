package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandler(t *testing.T) {
	view := func() View { return View{Steward: "alice", Jobs: []Job{{Issue: 7}}} }
	ask := func(_ context.Context, q Question) (string, error) {
		switch q.Question {
		case "":
			return "", ErrEmptyQuestion
		case "busy":
			return "", ErrBusy
		case "broken":
			return "", errors.New("claude failed")
		}
		return "answer to " + q.Question, nil
	}
	tests := []struct {
		name     string
		method   string
		path     string
		host     string
		ctype    string
		body     string
		loopback bool
		wantCode int
		wantBody string
	}{
		{name: "page", method: "GET", path: "/", wantCode: 200, wantBody: "<title>schmeide dashboard</title>"},
		{name: "state", method: "GET", path: "/api/state", wantCode: 200, wantBody: `"steward":"alice"`},
		{name: "unknown path", method: "GET", path: "/nope", wantCode: 404},
		{name: "ask", method: "POST", path: "/api/ask", ctype: "application/json", body: `{"question":"hi"}`, wantCode: 200, wantBody: `"answer":"answer to hi"`},
		{name: "ask needs json", method: "POST", path: "/api/ask", ctype: "text/plain", body: `{"question":"hi"}`, wantCode: 415, wantBody: "application/json"},
		{name: "ask junk", method: "POST", path: "/api/ask", ctype: "application/json", body: `nope`, wantCode: 400, wantBody: "not a question"},
		{name: "ask too big", method: "POST", path: "/api/ask", ctype: "application/json", body: `{"question":"` + strings.Repeat("x", maxAskBody) + `"}`, wantCode: 400},
		{name: "ask empty", method: "POST", path: "/api/ask", ctype: "application/json", body: `{}`, wantCode: 400, wantBody: "empty"},
		{name: "ask busy", method: "POST", path: "/api/ask", ctype: "application/json", body: `{"question":"busy"}`, wantCode: 429, wantBody: "still being answered"},
		{name: "ask fails", method: "POST", path: "/api/ask", ctype: "application/json", body: `{"question":"broken"}`, wantCode: 502, wantBody: "claude failed"},
		{name: "state is GET only", method: "POST", path: "/api/state", wantCode: 405},
		{name: "loopback name", method: "GET", path: "/api/state", host: "localhost:7878", loopback: true, wantCode: 200},
		{name: "loopback ip", method: "GET", path: "/api/state", host: "127.0.0.1:7878", loopback: true, wantCode: 200},
		{name: "loopback v6", method: "GET", path: "/api/state", host: "[::1]:7878", loopback: true, wantCode: 200},
		{name: "rebound name refused", method: "GET", path: "/api/state", host: "evil.example:7878", loopback: true, wantCode: 403, wantBody: "only answers on localhost"},
		{name: "any name when not loopback", method: "GET", path: "/api/state", host: "box.lan:7878", wantCode: 200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), tt.method, tt.path, strings.NewReader(tt.body))
			if tt.host != "" {
				req.Host = tt.host
			}
			if tt.ctype != "" {
				req.Header.Set("Content-Type", tt.ctype)
			}
			rec := httptest.NewRecorder()
			Handler(view, ask, tt.loopback).ServeHTTP(rec, req)
			if rec.Code != tt.wantCode || !strings.Contains(rec.Body.String(), tt.wantBody) {
				t.Errorf("%s %s = %d %q, want %d containing %q", tt.method, tt.path, rec.Code, rec.Body.String(), tt.wantCode, tt.wantBody)
			}
		})
	}
}

func TestStateIsJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler(func() View { return newBoard("alice", "sonnet").View() }, nil, false).
		ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), "GET", "/api/state", nil))
	var v map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	// The page reads every one of these; an empty board must still send lists.
	for _, key := range []string{"steward", "version", "model", "connected", "updated", "runners", "jobs", "finished", "activity"} {
		if _, ok := v[key]; !ok {
			t.Errorf("state has no %q: %s", key, rec.Body.String())
		}
	}
	for _, key := range []string{"runners", "jobs", "finished", "activity"} {
		if string(v[key]) != "[]" {
			t.Errorf("%s = %s, want []", key, v[key])
		}
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("content type = %q", got)
	}
	if got := (&http.Response{Header: rec.Header()}).Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("cache control = %q", got)
	}
}
