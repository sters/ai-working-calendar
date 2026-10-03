package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sters/ai-working-calendar/internal/sessionlog"
)

type fakeSource []sessionlog.Session

func get(target string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, target, nil)
	r.Host = "127.0.0.1:8137"

	return r
}

func (f fakeSource) Sessions() ([]sessionlog.Session, error) { return f, nil }

func at(t *testing.T, v string) time.Time {
	t.Helper()

	tm, err := time.Parse(time.RFC3339, v)
	if err != nil {
		t.Fatal(err)
	}

	return tm
}

func TestEvents(t *testing.T) {
	t.Parallel()

	src := fakeSource{{
		ID:    "s1",
		Title: "work",
		Cwd:   "/work/repo",
		Blocks: []sessionlog.Block{
			{Start: at(t, "2026-09-30T23:00:00Z"), End: at(t, "2026-10-01T00:30:00Z"), Prompts: 1, Messages: 4, CostUSD: 1},
			{Start: at(t, "2026-10-01T10:00:00Z"), End: at(t, "2026-10-01T10:00:00Z"), Prompts: 1, Messages: 1},
			{
				Start: at(t, "2026-10-02T10:00:00Z"), End: at(t, "2026-10-02T11:00:00Z"), Prompts: 1, Messages: 2, CostUSD: 0.5,
				PRs: []sessionlog.PR{{Number: 1, URL: "https://example.com/pr/1"}},
			},
		},
	}}

	h := New(src)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, get("/api/events?start=2026-10-01T00:00:00%2B00:00&end=2026-10-02T00:00:00%2B00:00"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}

	var got []event
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}

	if len(got) != 2 {
		t.Fatalf("events = %+v, want the block crossing midnight and the single-message block", got)
	}

	if got[0].ID != "s1#0" || got[1].ID != "s1#1" {
		t.Errorf("ids = %q, %q", got[0].ID, got[1].ID)
	}

	if want := at(t, "2026-10-01T10:05:00Z"); !got[1].End.Equal(want) {
		t.Errorf("single-message end = %v, want %v", got[1].End, want)
	}

	if want := at(t, "2026-10-01T10:00:00Z"); !got[1].ExtendedProps.Block.End.Equal(want) {
		t.Errorf("block end = %v, want %v", got[1].ExtendedProps.Block.End, want)
	}

	if s := got[0].ExtendedProps.Session; s.Blocks != 3 || s.CostUSD != 1.5 || len(s.PRs) != 1 {
		t.Errorf("session info = %+v, want totals over all 3 blocks", s)
	}
}

func TestEventsBadRange(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	New(fakeSource{}).ServeHTTP(rec, get("/api/events?start=yesterday"))

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestIndexPage(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	New(fakeSource{}).ServeHTTP(rec, get("/"))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestLog(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "s1.jsonl")
	sub := filepath.Join(dir, "s1", "subagents", "agent-1.jsonl")

	if err := os.MkdirAll(filepath.Dir(sub), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(`{"type":"user"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(sub, []byte(`{"type":"assistant"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	h := New(fakeSource{{ID: "s1", Path: path, Subagents: []string{"agent-1.jsonl"}}})

	tests := []struct {
		target string
		code   int
		body   string
	}{
		{"/api/sessions/s1/log", http.StatusOK, `{"type":"user"}`},
		{"/api/sessions/s1/log?file=agent-1.jsonl", http.StatusOK, `{"type":"assistant"}`},
		{"/api/sessions/s1/log?file=../../s1.jsonl", http.StatusNotFound, ""},
		{"/api/sessions/nope/log", http.StatusNotFound, ""},
	}

	for _, tt := range tests {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, get(tt.target))

		if rec.Code != tt.code {
			t.Errorf("%s: status = %d, want %d", tt.target, rec.Code, tt.code)
		}

		if tt.body != "" && rec.Body.String() != tt.body {
			t.Errorf("%s: body = %q, want %q", tt.target, rec.Body, tt.body)
		}
	}
}

func TestLoopbackOnly(t *testing.T) {
	t.Parallel()

	h := New(fakeSource{})

	for host, want := range map[string]int{
		"127.0.0.1:8137":    http.StatusOK,
		"localhost:8137":    http.StatusOK,
		"[::1]:8137":        http.StatusOK,
		"evil.example:8137": http.StatusForbidden,
		"127.0.0.1.nip.io":  http.StatusForbidden,
	} {
		r := get("/")
		r.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)

		if rec.Code != want {
			t.Errorf("Host %s: status = %d, want %d", host, rec.Code, want)
		}
	}
}
