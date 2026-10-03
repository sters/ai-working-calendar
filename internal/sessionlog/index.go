package sessionlog

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// cacheVersion must be bumped whenever Parse would produce a different Session
// from the same file, so that caches written by older builds are discarded.
const cacheVersion = 7

// Index keeps one Session per log file under root and re-parses only the files
// whose size or mtime changed since they were last read.
type Index struct {
	root      string
	gap       time.Duration
	cachePath string

	mu    sync.Mutex
	files map[string]entry
}

type entry struct {
	ModTime time.Time `json:"modTime"`
	Size    int64     `json:"size"`
	Session Session   `json:"session"`
}

type cacheFile struct {
	Version int              `json:"version"`
	Gap     time.Duration    `json:"gap"`
	Files   map[string]entry `json:"files"`
}

// fileRef is a session log and the logs of its subagents. size and modTime
// cover all of them, so a change to any one marks the session stale.
type fileRef struct {
	path      string
	subagents []string
	size      int64
	modTime   time.Time
}

// NewIndex loads the cache at cachePath if it was built with the same gap.
// An empty cachePath disables the on-disk cache.
func NewIndex(root string, gap time.Duration, cachePath string) *Index {
	idx := &Index{root: root, gap: gap, cachePath: cachePath, files: map[string]entry{}}
	idx.loadCache()

	return idx
}

// Sessions refreshes the index and returns every known session. A session
// without a cost-state gets its cost estimated from its tokens, at each
// model's average price over the sessions that have one.
func (idx *Index) Sessions() ([]Session, error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	if err := idx.refresh(); err != nil {
		return nil, err
	}

	sessions := make([]Session, 0, len(idx.files))
	for _, e := range idx.files {
		sessions = append(sessions, e.Session)
	}

	prices := pricePerWeight(sessions)

	for i := range sessions {
		if sessions[i].Usage == nil {
			sessions[i] = estimateCost(sessions[i], prices)
		}
	}

	return sessions, nil
}

// pricePerWeight returns each model's cost per weighted token over the
// sessions whose cost-state reports a cost for it.
func pricePerWeight(sessions []Session) map[string]float64 {
	cost := map[string]float64{}
	weight := map[string]float64{}

	for _, s := range sessions {
		for model, u := range s.Usage {
			base := baseModel(model)

			var w float64
			for _, b := range s.Blocks {
				w += b.Tokens[base].weight()
			}

			if w > 0 && u.CostUSD > 0 {
				cost[base] += u.CostUSD
				weight[base] += w
			}
		}
	}

	prices := make(map[string]float64, len(cost))
	for model, c := range cost {
		prices[model] = c / weight[model]
	}

	return prices
}

func estimateCost(s Session, prices map[string]float64) Session {
	blocks := make([]Block, len(s.Blocks))
	copy(blocks, s.Blocks)

	for i := range blocks {
		for model, t := range blocks[i].Tokens {
			if p, ok := prices[model]; ok {
				blocks[i].CostUSD += t.weight() * p
				s.CostEstimated = true
			}
		}
	}

	s.Blocks = blocks

	return s
}

func (idx *Index) refresh() error {
	refs, err := idx.listFiles()
	if err != nil {
		return err
	}

	seen := make(map[string]struct{}, len(refs))

	var stale []fileRef

	for _, ref := range refs {
		seen[ref.path] = struct{}{}

		e, ok := idx.files[ref.path]
		if !ok || e.Size != ref.size || !e.ModTime.Equal(ref.modTime) {
			stale = append(stale, ref)
		}
	}

	removed := false

	for path := range idx.files {
		if _, ok := seen[path]; !ok {
			delete(idx.files, path)

			removed = true
		}
	}

	if len(stale) == 0 {
		if removed {
			idx.saveCache()
		}

		return nil
	}

	started := time.Now()

	maps.Copy(idx.files, idx.parseAll(stale))

	slog.Info("indexed session logs", "parsed", len(stale), "total", len(idx.files), "took", time.Since(started).Round(time.Millisecond))
	idx.saveCache()

	return nil
}

// listFiles returns the session logs of each project directory, each with
// the subagent logs kept in <project>/<session id>/subagents/.
func (idx *Index) listFiles() ([]fileRef, error) {
	projects, err := os.ReadDir(idx.root)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", idx.root, err)
	}

	var refs []fileRef

	for _, p := range projects {
		if !p.IsDir() {
			continue
		}

		dir := filepath.Join(idx.root, p.Name())

		files, err := os.ReadDir(dir)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", dir, err)
		}

		for _, f := range files {
			if f.IsDir() || filepath.Ext(f.Name()) != ".jsonl" {
				continue
			}

			info, err := f.Info()
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}

			if err != nil {
				return nil, fmt.Errorf("stat %s: %w", f.Name(), err)
			}

			ref := fileRef{path: filepath.Join(dir, f.Name()), size: info.Size(), modTime: info.ModTime()}
			if err := addSubagents(&ref); err != nil {
				return nil, err
			}

			refs = append(refs, ref)
		}
	}

	return refs, nil
}

func addSubagents(ref *fileRef) error {
	dir := filepath.Join(strings.TrimSuffix(ref.path, filepath.Ext(ref.path)), "subagents")

	files, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("read %s: %w", dir, err)
	}

	for _, f := range files {
		if f.IsDir() || filepath.Ext(f.Name()) != ".jsonl" {
			continue
		}

		info, err := f.Info()
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}

		if err != nil {
			return fmt.Errorf("stat %s: %w", f.Name(), err)
		}

		ref.subagents = append(ref.subagents, filepath.Join(dir, f.Name()))
		ref.size += info.Size()

		if info.ModTime().After(ref.modTime) {
			ref.modTime = info.ModTime()
		}
	}

	return nil
}

func (idx *Index) parseAll(refs []fileRef) map[string]entry {
	jobs := make(chan fileRef)
	results := make(map[string]entry, len(refs))

	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)

	for range runtime.GOMAXPROCS(0) {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for ref := range jobs {
				s, err := idx.parseFile(ref)
				if err != nil {
					slog.Warn("skip session log", "path", ref.path, "err", err)

					continue
				}

				mu.Lock()
				results[ref.path] = entry{ModTime: ref.modTime, Size: ref.size, Session: s}
				mu.Unlock()
			}
		}()
	}

	for _, ref := range refs {
		jobs <- ref
	}

	close(jobs)
	wg.Wait()

	return results
}

func (idx *Index) parseFile(ref fileRef) (Session, error) {
	f, err := os.Open(ref.path)
	if err != nil {
		return Session{}, fmt.Errorf("open session log: %w", err)
	}
	defer f.Close()

	subs := make([]io.Reader, 0, len(ref.subagents))

	for _, path := range ref.subagents {
		sf, err := os.Open(path)
		if err != nil {
			return Session{}, fmt.Errorf("open subagent log: %w", err)
		}
		defer sf.Close()

		subs = append(subs, sf)
	}

	id := strings.TrimSuffix(filepath.Base(ref.path), filepath.Ext(ref.path))

	sess, err := Parse(id, f, idx.gap, subs...)
	if err != nil {
		return Session{}, err
	}

	sess.Path = ref.path
	for _, path := range ref.subagents {
		sess.Subagents = append(sess.Subagents, filepath.Base(path))
	}

	return sess, nil
}

// SubagentPath returns the path of the subagent log with the given base name.
func (s *Session) SubagentPath(name string) (string, bool) {
	for _, n := range s.Subagents {
		if n == name {
			dir := strings.TrimSuffix(s.Path, filepath.Ext(s.Path))

			return filepath.Join(dir, "subagents", n), true
		}
	}

	return "", false
}

func (idx *Index) loadCache() {
	if idx.cachePath == "" {
		return
	}

	b, err := os.ReadFile(idx.cachePath)
	if err != nil {
		return
	}

	var c cacheFile
	if err := json.Unmarshal(b, &c); err != nil || c.Version != cacheVersion || c.Gap != idx.gap || c.Files == nil {
		return
	}

	idx.files = c.Files
}

func (idx *Index) saveCache() {
	if idx.cachePath == "" {
		return
	}

	b, err := json.Marshal(cacheFile{Version: cacheVersion, Gap: idx.gap, Files: idx.files})
	if err != nil {
		slog.Warn("encode cache", "err", err)

		return
	}

	if err := os.MkdirAll(filepath.Dir(idx.cachePath), 0o700); err != nil {
		slog.Warn("create cache dir", "err", err)

		return
	}

	tmp := idx.cachePath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		slog.Warn("write cache", "err", err)

		return
	}

	if err := os.Rename(tmp, idx.cachePath); err != nil {
		slog.Warn("write cache", "err", err)
	}
}
