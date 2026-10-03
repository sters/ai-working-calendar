package sessionlog

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"
	"time"
)

func writeLog(t *testing.T, path, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func titles(t *testing.T, idx *Index) []string {
	t.Helper()

	sessions, err := idx.Sessions()
	if err != nil {
		t.Fatal(err)
	}

	got := make([]string, 0, len(sessions))
	for _, s := range sessions {
		got = append(got, s.ID+":"+s.Title)
	}

	sort.Strings(got)

	return got
}

func TestIndex(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cache := filepath.Join(t.TempDir(), "cache", "index.json")

	a := filepath.Join(root, "-work-a", "aaa.jsonl")
	b := filepath.Join(root, "-work-b", "bbb.jsonl")

	writeLog(t, a, `{"type":"ai-title","aiTitle":"A"}`)
	writeLog(t, b, `{"type":"ai-title","aiTitle":"B"}`)
	sub := filepath.Join(root, "-work-b", "bbb", "subagents", "agent-1.jsonl")
	writeLog(t, sub, `{"type":"ai-title","aiTitle":"sub"}`)
	writeLog(t, filepath.Join(root, "-work-b", "notes.txt"), "ignored")

	idx := NewIndex(root, time.Hour, cache)

	if got, want := titles(t, idx), []string{"aaa:A", "bbb:B"}; !slices.Equal(got, want) {
		t.Errorf("first scan = %v, want %v", got, want)
	}

	writeLog(t, b, `{"type":"ai-title","aiTitle":"B"}
{"type":"user","timestamp":"2026-10-01T10:00:00Z","message":{"content":"go"}}`)
	writeLog(t, sub, `{"type":"user","timestamp":"2026-10-01T10:00:05Z","toolUseResult":{"type":"create","filePath":"/w/x","content":"x"},"message":{"content":[]}}`)

	sessions, err := idx.Sessions()
	if err != nil {
		t.Fatal(err)
	}

	for _, s := range sessions {
		if s.ID == "bbb" && (len(s.Blocks) != 1 || s.Blocks[0].LinesAdded != 1) {
			t.Errorf("bbb blocks = %+v, want the subagent's created file counted in the parent's block", s.Blocks)
		}
	}

	writeLog(t, a, `{"type":"ai-title","aiTitle":"A2"}`)

	if err := os.Remove(b); err != nil {
		t.Fatal(err)
	}

	if got, want := titles(t, idx), []string{"aaa:A2"}; !slices.Equal(got, want) {
		t.Errorf("after change = %v, want %v", got, want)
	}

	if got, want := titles(t, NewIndex(root, time.Hour, cache)), []string{"aaa:A2"}; !slices.Equal(got, want) {
		t.Errorf("from cache = %v, want %v", got, want)
	}
}

func TestIndexReusesCache(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cache := filepath.Join(t.TempDir(), "index.json")
	path := filepath.Join(root, "-work-a", "aaa.jsonl")

	writeLog(t, path, `{"type":"ai-title","aiTitle":"A"}`)

	if _, err := NewIndex(root, time.Hour, cache).Sessions(); err != nil {
		t.Fatal(err)
	}

	// Same size and mtime as the cached entry, so the file must not be read again.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	writeLog(t, path, `{"type":"ai-title","aiTitle":"Z"}`)

	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}

	if got, want := titles(t, NewIndex(root, time.Hour, cache)), []string{"aaa:A"}; !slices.Equal(got, want) {
		t.Errorf("same gap = %v, want %v", got, want)
	}

	if got, want := titles(t, NewIndex(root, 2*time.Hour, cache)), []string{"aaa:Z"}; !slices.Equal(got, want) {
		t.Errorf("different gap = %v, want %v", got, want)
	}
}
