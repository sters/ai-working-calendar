// Package server serves the calendar page and the events API over HTTP.
package server

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/sters/ai-working-calendar/internal/sessionlog"
)

// minEventDuration keeps a block made of a single message visible on the calendar.
const minEventDuration = 5 * time.Minute

var errInvalidTime = errors.New("invalid time")

//go:embed static
var staticFS embed.FS

// SessionSource provides the sessions to lay out on the calendar.
type SessionSource interface {
	Sessions() ([]sessionlog.Session, error)
}

type event struct {
	ID            string     `json:"id"`
	Title         string     `json:"title"`
	Start         time.Time  `json:"start"`
	End           time.Time  `json:"end"`
	ExtendedProps eventProps `json:"extendedProps"`
}

type eventProps struct {
	Session sessionInfo      `json:"session"`
	Block   sessionlog.Block `json:"block"`
}

// sessionInfo is a Session without its blocks, plus totals over all of them.
type sessionInfo struct {
	ID            string                           `json:"id"`
	Cwd           string                           `json:"cwd"`
	Branch        string                           `json:"branch"`
	Entrypoint    string                           `json:"entrypoint"`
	Version       string                           `json:"version"`
	Slug          string                           `json:"slug"`
	Path          string                           `json:"path"`
	Subagents     []string                         `json:"subagents,omitempty"`
	Usage         map[string]sessionlog.ModelUsage `json:"usage,omitempty"`
	CostEstimated bool                             `json:"costEstimated,omitempty"`
	Blocks        int                              `json:"blocks"`
	CostUSD       float64                          `json:"costUSD"`
	ActiveMs      int64                            `json:"activeMs"`
	LinesAdded    int                              `json:"linesAdded"`
	LinesRemoved  int                              `json:"linesRemoved"`
	PRs           []sessionlog.PR                  `json:"prs,omitempty"`
}

func newSessionInfo(s *sessionlog.Session) sessionInfo {
	info := sessionInfo{
		ID:            s.ID,
		Cwd:           s.Cwd,
		Branch:        s.Branch,
		Entrypoint:    s.Entrypoint,
		Version:       s.Version,
		Slug:          s.Slug,
		Path:          s.Path,
		Subagents:     s.Subagents,
		Usage:         s.Usage,
		CostEstimated: s.CostEstimated,
		Blocks:        len(s.Blocks),
	}

	for _, b := range s.Blocks {
		info.CostUSD += b.CostUSD
		info.ActiveMs += b.ActiveMs
		info.LinesAdded += b.LinesAdded
		info.LinesRemoved += b.LinesRemoved
		info.PRs = append(info.PRs, b.PRs...)
	}

	return info
}

// New returns a handler serving the calendar page at /, events at
// /api/events, and a session's raw log at /api/sessions/{id}/log.
func New(src SessionSource) http.Handler {
	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}

	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServerFS(static))
	mux.HandleFunc("GET /api/events", func(w http.ResponseWriter, r *http.Request) {
		handleEvents(w, r, src)
	})
	mux.HandleFunc("GET /api/sessions/{id}/log", func(w http.ResponseWriter, r *http.Request) {
		handleLog(w, r, src)
	})

	return loopbackOnly(mux)
}

// loopbackOnly rejects requests whose Host is not a loopback name. Browsers
// already keep other sites from reading responses, but a site whose domain
// resolves to 127.0.0.1 (DNS rebinding) would count as same-origin; its
// requests still carry its own domain as Host.
func loopbackOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}

		switch host {
		case "127.0.0.1", "localhost", "::1":
			next.ServeHTTP(w, r)
		default:
			http.Error(w, "forbidden host", http.StatusForbidden)
		}
	})
}

// handleLog serves the session log, or with ?file= one of its subagent logs.
func handleLog(w http.ResponseWriter, r *http.Request, src SessionSource) {
	sessions, err := src.Sessions()
	if err != nil {
		slog.Error("load sessions", "err", err)
		http.Error(w, "failed to load sessions", http.StatusInternalServerError)

		return
	}

	id := r.PathValue("id")

	var path string

	for i := range sessions {
		if sessions[i].ID != id {
			continue
		}

		path = sessions[i].Path

		if name := r.URL.Query().Get("file"); name != "" {
			var ok bool
			if path, ok = sessions[i].SubagentPath(name); !ok {
				http.NotFound(w, r)

				return
			}
		}

		break
	}

	if path == "" {
		http.NotFound(w, r)

		return
	}

	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "failed to open log", http.StatusInternalServerError)

		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		http.Error(w, "failed to open log", http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	http.ServeContent(w, r, "", info.ModTime(), f)
}

func handleEvents(w http.ResponseWriter, r *http.Request, src SessionSource) {
	start, err := parseTime(r.URL.Query().Get("start"))
	if err != nil {
		http.Error(w, "invalid start: "+err.Error(), http.StatusBadRequest)

		return
	}

	end, err := parseTime(r.URL.Query().Get("end"))
	if err != nil {
		http.Error(w, "invalid end: "+err.Error(), http.StatusBadRequest)

		return
	}

	sessions, err := src.Sessions()
	if err != nil {
		slog.Error("load sessions", "err", err)
		http.Error(w, "failed to load sessions", http.StatusInternalServerError)

		return
	}

	events := eventsBetween(sessions, start, end)

	w.Header().Set("Content-Type", "application/json")

	if err := json.NewEncoder(w).Encode(events); err != nil {
		slog.Warn("write events", "err", err)
	}
}

func eventsBetween(sessions []sessionlog.Session, start, end time.Time) []event {
	events := []event{}

	for _, s := range sessions {
		var info *sessionInfo

		for i, b := range s.Blocks {
			shownEnd := b.End
			if shownEnd.Sub(b.Start) < minEventDuration {
				shownEnd = b.Start.Add(minEventDuration)
			}

			if !shownEnd.After(start) || !b.Start.Before(end) {
				continue
			}

			if info == nil {
				si := newSessionInfo(&s)
				info = &si
			}

			events = append(events, event{
				ID:            s.ID + "#" + strconv.Itoa(i),
				Title:         s.Title,
				Start:         b.Start,
				End:           shownEnd,
				ExtendedProps: eventProps{Session: *info, Block: b},
			})
		}
	}

	sort.Slice(events, func(i, j int) bool { return events[i].Start.Before(events[j].Start) })

	return events
}

func parseTime(v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, fmt.Errorf("%w: empty", errInvalidTime)
	}

	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", time.DateOnly} {
		// A time without an offset is the browser's local time, and the browser runs on this machine.
		if t, err := time.ParseInLocation(layout, v, time.Local); err == nil { //nolint:gosmopolitan
			return t, nil
		}
	}

	return time.Time{}, fmt.Errorf("%w: %q", errInvalidTime, v)
}
