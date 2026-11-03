package httpapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type fakeDB struct {
	Store // unused methods panic
	err   error
}

func (f fakeDB) Ping(context.Context) error { return f.err }

func TestHealthz(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cases := []struct {
		name     string
		db       fakeDB
		method   string
		wantCode int
		wantBody string
	}{
		{"ok", fakeDB{}, "GET", http.StatusOK, "ok"},
		{"db down", fakeDB{err: errors.New("disk gone")}, "GET", http.StatusServiceUnavailable, "unavailable"},
		{"wrong method", fakeDB{}, "POST", http.StatusMethodNotAllowed, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			New(c.db, nil, nil, log, time.Now).Handler().ServeHTTP(rec, httptest.NewRequest(c.method, "/healthz", nil))
			if rec.Code != c.wantCode {
				t.Fatalf("status = %d, want %d", rec.Code, c.wantCode)
			}
			if c.wantBody != "" && rec.Body.String() != c.wantBody {
				t.Fatalf("body = %q, want %q (no details leaked)", rec.Body.String(), c.wantBody)
			}
		})
	}
}
