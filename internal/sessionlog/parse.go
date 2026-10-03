// Package sessionlog reads Claude Code session logs (~/.claude/projects/*/*.jsonl)
// and turns them into blocks of activity that can be laid out on a calendar.
package sessionlog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	typeUser      = "user"
	typeAssistant = "assistant"

	fallbackTitleRunes = 80

	// activeJoinGap merges turns separated by less than this into one active
	// interval, so a quick back-and-forth reads as one stretch of work.
	activeJoinGap = 5 * time.Minute
)

// Relative prices of each token kind against uncached input, used only to
// split a model's session cost across blocks. The session total stays the
// cost-state figure; prices that vary per message (fast mode, long context)
// make the split approximate.
const (
	weightInput        = 1.0
	weightOutput       = 5.0
	weightCacheRead    = 0.1
	weightCacheWrite5m = 1.25
	weightCacheWrite1h = 2.0
)

// Session is the summary of one session log file.
type Session struct {
	ID     string `json:"id"`
	Cwd    string `json:"cwd"`
	Branch string `json:"branch"`
	Title  string `json:"title"`
	// Entrypoint is how the session was started: "cli" for an interactive
	// session, "sdk-cli" for one driven by a program through the SDK.
	Entrypoint string `json:"entrypoint"`
	Version    string `json:"version"`
	Slug       string `json:"slug"`
	// Path is the session log file and Subagents the base names of its
	// subagent logs; both are set by Index.
	Path      string   `json:"path,omitempty"`
	Subagents []string `json:"subagents,omitempty"`
	// Usage is the per-model token usage from the session's last cost-state.
	// It is nil while a session is running or when it ended without writing one.
	Usage map[string]ModelUsage `json:"usage,omitempty"`
	// CostEstimated is set when the blocks' cost was estimated from their
	// tokens because the session has no cost-state; see Index.Sessions.
	CostEstimated bool `json:"costEstimated,omitempty"`

	Blocks []Block `json:"blocks"`
}

// ModelUsage is the token usage and API-priced cost of one model.
type ModelUsage struct {
	InputTokens         int64   `json:"inputTokens"`
	OutputTokens        int64   `json:"outputTokens"`
	CacheReadTokens     int64   `json:"cacheReadInputTokens"`
	CacheCreationTokens int64   `json:"cacheCreationInputTokens"`
	CostUSD             float64 `json:"costUSD"`
}

// Block is a run of messages with no idle gap longer than the split threshold.
type Block struct {
	Start    time.Time `json:"start"`
	End      time.Time `json:"end"`
	Prompts  int       `json:"prompts"`
	Messages int       `json:"messages"`

	// ActiveMs is the total time Claude spent working on turns in this block.
	// Active lists those turns, merged when they are close together.
	// ActiveEstimated is set when the log has no turn durations, as with SDK
	// sessions and older Claude Code versions; each turn then runs from a
	// prompt to the last message before the next one.
	ActiveMs        int64      `json:"activeMs"`
	Active          []Interval `json:"active,omitempty"`
	ActiveEstimated bool       `json:"activeEstimated,omitempty"`

	// CostUSD is this block's share of the session cost, split by Tokens.
	CostUSD float64                `json:"costUSD"`
	Tokens  map[string]TokenCounts `json:"tokens,omitempty"`

	// LinesAdded and LinesRemoved count the lines in the patches of file
	// edits made in this block, by edit tools and by Bash commands alike.
	LinesAdded   int `json:"linesAdded"`
	LinesRemoved int `json:"linesRemoved"`

	Tools  map[string]int `json:"tools,omitempty"`
	Models map[string]int `json:"models,omitempty"`
	Skills map[string]int `json:"skills,omitempty"`
	MCP    map[string]int `json:"mcp,omitempty"`
	Files  []string       `json:"files,omitempty"`
	PRs    []PR           `json:"prs,omitempty"`
	Errors int            `json:"errors"`
}

// TokenCounts is the tokens one model used.
type TokenCounts struct {
	Input        int64 `json:"input"`
	Output       int64 `json:"output"`
	CacheRead    int64 `json:"cacheRead"`
	CacheWrite5m int64 `json:"cacheWrite5m"`
	CacheWrite1h int64 `json:"cacheWrite1h"`
}

func (t TokenCounts) add(o TokenCounts) TokenCounts {
	return TokenCounts{
		Input:        t.Input + o.Input,
		Output:       t.Output + o.Output,
		CacheRead:    t.CacheRead + o.CacheRead,
		CacheWrite5m: t.CacheWrite5m + o.CacheWrite5m,
		CacheWrite1h: t.CacheWrite1h + o.CacheWrite1h,
	}
}

func (t TokenCounts) weight() float64 {
	return weightInput*float64(t.Input) + weightOutput*float64(t.Output) +
		weightCacheRead*float64(t.CacheRead) +
		weightCacheWrite5m*float64(t.CacheWrite5m) + weightCacheWrite1h*float64(t.CacheWrite1h)
}

// Interval is a span of time.
type Interval struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// PR is a pull request the session created or linked.
type PR struct {
	Number     int       `json:"number"`
	Repository string    `json:"repository"`
	URL        string    `json:"url"`
	At         time.Time `json:"at"`
}

type logLine struct {
	Type         string    `json:"type"`
	Subtype      string    `json:"subtype"`
	Timestamp    time.Time `json:"timestamp"`
	Cwd          string    `json:"cwd"`
	GitBranch    string    `json:"gitBranch"`
	Entrypoint   string    `json:"entrypoint"`
	Version      string    `json:"version"`
	Slug         string    `json:"slug"`
	AITitle      string    `json:"aiTitle"`
	LastPrompt   string    `json:"lastPrompt"`
	IsMeta       bool      `json:"isMeta"`
	IsCompact    bool      `json:"isCompactSummary"`
	PromptSource string    `json:"promptSource"`
	Skill        string    `json:"attributionSkill"`
	MCPServer    string    `json:"attributionMcpServer"`
	DurationMs   int64     `json:"durationMs"`

	PRNumber     int    `json:"prNumber"`
	PRRepository string `json:"prRepository"`
	PRURL        string `json:"prUrl"`

	ModelUsage map[string]ModelUsage `json:"modelUsage"`

	ToolUseResult json.RawMessage `json:"toolUseResult"`

	Message struct {
		ID      string          `json:"id"`
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
		Usage   *apiUsage       `json:"usage"`
	} `json:"message"`
}

type apiUsage struct {
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	CacheReadTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
	CacheCreation       *struct {
		Ephemeral5m int64 `json:"ephemeral_5m_input_tokens"`
		Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
}

func (u *apiUsage) counts() TokenCounts {
	c := TokenCounts{Input: u.InputTokens, Output: u.OutputTokens, CacheRead: u.CacheReadTokens}
	if u.CacheCreation != nil {
		c.CacheWrite5m = u.CacheCreation.Ephemeral5m
		c.CacheWrite1h = u.CacheCreation.Ephemeral1h
	} else {
		c.CacheWrite5m = u.CacheCreationTokens
	}

	return c
}

type hunk struct {
	Lines []string `json:"lines"`
}

// toolResult is the part of a tool's structured result that describes file
// changes: an edit's patch, a created file's content, or the files a Bash
// command changed.
type toolResult struct {
	Type            string `json:"type"`
	FilePath        string `json:"filePath"`
	Content         string `json:"content"`
	StructuredPatch []hunk `json:"structuredPatch"`
	BashEditDiff    *struct {
		Files []struct {
			FilePath string `json:"filePath"`
		} `json:"files"`
	} `json:"bashEditDiff"`
}

type contentPart struct {
	Type  string `json:"type"`
	Text  string `json:"text"`
	Name  string `json:"name"`
	Input struct {
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
	} `json:"input"`
}

// activity is one user or assistant message.
type activity struct {
	at           time.Time
	isPrompt     bool
	model        string
	skill        string
	mcp          string
	tools        []string
	files        []string
	linesAdded   int
	linesRemoved int
}

// apiMessage is one API response. Its usage is repeated on every log line
// that carries a part of the response.
type apiMessage struct {
	at     time.Time
	model  string
	counts TokenCounts
}

// extra is something that happened at a point in time and is credited to
// whichever block that time falls in.
type extra struct {
	at       time.Time
	turn     *Interval
	pr       *PR
	apiError bool

	// Set for work done by a subagent.
	tools        []string
	files        []string
	linesAdded   int
	linesRemoved int
}

type parser struct {
	session     Session
	acts        []activity
	extras      []extra
	apiMessages map[string]*apiMessage
	// modelCost is each model's session cost, accumulated over cost-states so
	// that a total which restarted from zero still adds up.
	modelCost     map[string]float64
	lastModelCost map[string]float64
	lastPrompt    string
	firstPrompt   string
}

// Parse reads one session log and splits its messages into blocks wherever
// two consecutive messages are more than gap apart. The logs of the
// session's subagents add their tokens, tools and file changes to the blocks
// they ran in.
func Parse(id string, r io.Reader, gap time.Duration, subagents ...io.Reader) (Session, error) {
	p := parser{
		session:       Session{ID: id},
		apiMessages:   map[string]*apiMessage{},
		modelCost:     map[string]float64{},
		lastModelCost: map[string]float64{},
	}

	if err := p.read(r, p.line); err != nil {
		return Session{}, fmt.Errorf("read session log %s: %w", id, err)
	}

	for _, sub := range subagents {
		if err := p.read(sub, p.subagentLine); err != nil {
			return Session{}, fmt.Errorf("read subagent log of %s: %w", id, err)
		}
	}

	return p.finish(gap), nil
}

func (p *parser) read(r io.Reader, handle func(*logLine)) error {
	br := bufio.NewReaderSize(r, 1<<20)

	for {
		raw, err := br.ReadBytes('\n')

		var l logLine
		// A log that is still being written can end in a partial line; skip it.
		if len(bytes.TrimSpace(raw)) > 0 && json.Unmarshal(raw, &l) == nil {
			handle(&l)
		}

		if err == io.EOF {
			return nil
		}

		if err != nil {
			return err //nolint:wrapcheck // wrapped by Parse with the session id
		}
	}
}

// subagentLine handles a line of a subagent log. A subagent works inside its
// parent's blocks, so it adds no messages, prompts or blocks of its own.
func (p *parser) subagentLine(l *logLine) {
	if l.Timestamp.IsZero() {
		return
	}

	e := extra{at: l.Timestamp}

	switch l.Type {
	case typeUser:
		e.files, e.linesAdded, e.linesRemoved = fileChanges(l.ToolUseResult)
	case typeAssistant:
		if realModel(l.Message.Model) {
			p.apiMessage(l)
		}

		e.tools, e.files = toolUses(l.Message.Content)
	default:
		return
	}

	if len(e.tools) > 0 || len(e.files) > 0 || e.linesAdded > 0 || e.linesRemoved > 0 {
		p.extras = append(p.extras, e)
	}
}

func (p *parser) line(l *logLine) {
	if l.Slug != "" {
		p.session.Slug = l.Slug
	}

	switch l.Type {
	case "ai-title":
		p.session.Title = l.AITitle
	case "last-prompt":
		p.lastPrompt = l.LastPrompt
	case typeUser, typeAssistant:
		p.message(l)
	case "system":
		p.system(l)
	case "pr-link":
		if l.PRURL != "" {
			p.extras = append(p.extras, extra{at: l.Timestamp, pr: &PR{
				Number: l.PRNumber, Repository: l.PRRepository, URL: l.PRURL, At: l.Timestamp,
			}})
		}
	case "cost-state":
		p.costState(l)
	}
}

func (p *parser) message(l *logLine) {
	if p.session.Cwd == "" {
		p.session.Cwd = l.Cwd
	}

	if p.session.Entrypoint == "" {
		p.session.Entrypoint = l.Entrypoint
	}

	if l.GitBranch != "" {
		p.session.Branch = l.GitBranch
	}

	if l.Version != "" {
		p.session.Version = l.Version
	}

	if l.Timestamp.IsZero() {
		return
	}

	a := activity{at: l.Timestamp}

	switch l.Type {
	case typeUser:
		if text, ok := isPrompt(l); ok {
			a.isPrompt = true

			if p.firstPrompt == "" && !strings.HasPrefix(text, "<") {
				p.firstPrompt = text
			}
		}

		a.files, a.linesAdded, a.linesRemoved = fileChanges(l.ToolUseResult)
	case typeAssistant:
		if realModel(l.Message.Model) {
			a.model = l.Message.Model
			p.apiMessage(l)
		}

		a.skill = l.Skill
		a.mcp = l.MCPServer
		a.tools, a.files = toolUses(l.Message.Content)
	}

	p.acts = append(p.acts, a)
}

// isPrompt reports whether a user message is an instruction given to the
// session, as opposed to a tool result, a meta message, or a message sent by
// another session or a scheduler. Logs from older Claude Code versions have
// no promptSource and are judged by their content alone.
func isPrompt(l *logLine) (string, bool) {
	if l.IsMeta || l.IsCompact {
		return "", false
	}

	text, ok := promptText(l.Message.Content)
	if !ok {
		return "", false
	}

	switch l.PromptSource {
	case "", "typed", "queued", "sdk":
		return text, true
	default:
		return "", false
	}
}

func (p *parser) system(l *logLine) {
	switch l.Subtype {
	case "turn_duration":
		// Stamped when the turn ends.
		if l.Timestamp.IsZero() || l.DurationMs <= 0 {
			return
		}

		end := l.Timestamp
		start := end.Add(-time.Duration(l.DurationMs) * time.Millisecond)
		p.extras = append(p.extras, extra{at: end, turn: &Interval{Start: start, End: end}})
	case "api_error":
		p.extras = append(p.extras, extra{at: l.Timestamp, apiError: true})
	}
}

// realModel reports whether a message came from a model rather than being
// written by Claude Code itself, which marks those as "<synthetic>".
func realModel(model string) bool {
	return model != "" && !strings.HasPrefix(model, "<")
}

// apiMessage records a response's usage once, at the time of its first part.
// The last part carries the final output token count.
func (p *parser) apiMessage(l *logLine) {
	if l.Message.ID == "" || l.Message.Usage == nil {
		return
	}

	m, ok := p.apiMessages[l.Message.ID]
	if !ok {
		m = &apiMessage{at: l.Timestamp, model: l.Message.Model}
		p.apiMessages[l.Message.ID] = m
	}

	m.counts = l.Message.Usage.counts()
}

// costState accumulates each model's cost. cost-state carries running totals
// and is written only now and then, often once at the end of the session.
func (p *parser) costState(l *logLine) {
	if len(l.ModelUsage) == 0 {
		return
	}

	p.session.Usage = l.ModelUsage

	for model, u := range l.ModelUsage {
		prev := p.lastModelCost[model]
		if u.CostUSD < prev {
			// The total restarted from zero.
			prev = 0
		}

		p.modelCost[model] += u.CostUSD - prev
		p.lastModelCost[model] = u.CostUSD
	}
}

func (p *parser) finish(gap time.Duration) Session {
	s := p.session

	if s.Title == "" {
		s.Title = p.firstPrompt
	}

	if s.Title == "" {
		s.Title = p.lastPrompt
	}

	s.Title = truncate(s.Title, fallbackTitleRunes)
	s.Blocks = splitBlocks(p.acts, gap)
	applyExtras(s.Blocks, p.extras)
	estimateTurns(s.Blocks, p.acts)
	p.applyTokens(s.Blocks)
	allocateCost(s.Blocks, p.modelCost)

	return s
}

func (p *parser) applyTokens(blocks []Block) {
	for _, m := range p.apiMessages {
		b := &blocks[blockAt(blocks, m.at)]
		if b.Tokens == nil {
			b.Tokens = map[string]TokenCounts{}
		}

		b.Tokens[m.model] = b.Tokens[m.model].add(m.counts)
	}
}

// allocateCost splits each model's session cost across blocks by how much of
// that model's weighted tokens each block used. A model with no messages in
// the log, such as one used for background calls, is split by every model's
// tokens instead.
func allocateCost(blocks []Block, modelCost map[string]float64) {
	if len(blocks) == 0 {
		return
	}

	byModel := map[string][]float64{}
	all := make([]float64, len(blocks))

	for i, b := range blocks {
		for model, t := range b.Tokens {
			if byModel[model] == nil {
				byModel[model] = make([]float64, len(blocks))
			}

			byModel[model][i] += t.weight()
			all[i] += t.weight()
		}
	}

	for model, cost := range modelCost {
		weights := byModel[baseModel(model)]
		if total(weights) == 0 {
			weights = all
		}

		if total(weights) == 0 {
			blocks[len(blocks)-1].CostUSD += cost

			continue
		}

		w := total(weights)
		for i := range blocks {
			blocks[i].CostUSD += cost * weights[i] / w
		}
	}
}

// baseModel strips a variant suffix such as "[1m]", which cost-state keeps
// in model names but API messages do not.
func baseModel(model string) string {
	if i := strings.IndexByte(model, '['); i >= 0 {
		return model[:i]
	}

	return model
}

func total(xs []float64) float64 {
	var t float64
	for _, x := range xs {
		t += x
	}

	return t
}

// promptText returns the text of a user message's content, and false when
// the content is a tool result fed back to the model.
func promptText(content json.RawMessage) (string, bool) {
	content = bytes.TrimSpace(content)
	if len(content) == 0 {
		return "", false
	}

	if content[0] == '"' {
		var s string
		if json.Unmarshal(content, &s) != nil {
			return "", false
		}

		return s, true
	}

	var parts []contentPart
	if json.Unmarshal(content, &parts) != nil {
		return "", false
	}

	var text string

	for _, p := range parts {
		switch p.Type {
		case "tool_result":
			return "", false
		case "text":
			if text == "" {
				text = p.Text
			}
		}
	}

	return text, text != ""
}

func toolUses(content json.RawMessage) ([]string, []string) {
	content = bytes.TrimSpace(content)
	if len(content) == 0 || content[0] != '[' {
		return nil, nil
	}

	var parts []contentPart
	if json.Unmarshal(content, &parts) != nil {
		return nil, nil
	}

	tools := make([]string, 0, len(parts))

	var files []string

	for _, p := range parts {
		if p.Type != "tool_use" || p.Name == "" {
			continue
		}

		tools = append(tools, p.Name)

		switch p.Name {
		case "Edit", "MultiEdit", "Write":
			if p.Input.FilePath != "" {
				files = append(files, p.Input.FilePath)
			}
		case "NotebookEdit":
			if p.Input.NotebookPath != "" {
				files = append(files, p.Input.NotebookPath)
			}
		}
	}

	return tools, files
}

// fileChanges returns the files a tool result changed and the lines it added
// and removed. Lines are counted the way Claude Code's own totals count
// them: from edit tools only, with a created file's trailing newline
// starting one more line. Files changed by a Bash command are listed but
// their lines are not counted.
func fileChanges(raw json.RawMessage) ([]string, int, int) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return nil, 0, 0
	}

	var r toolResult
	if json.Unmarshal(raw, &r) != nil {
		return nil, 0, 0
	}

	var (
		files          []string
		added, removed int
	)

	switch {
	case r.Type == "create" && r.FilePath != "":
		files = append(files, r.FilePath)
		added = strings.Count(r.Content, "\n") + 1
	case len(r.StructuredPatch) > 0 && r.FilePath != "":
		files = append(files, r.FilePath)
		added, removed = countHunks(r.StructuredPatch)
	}

	if r.BashEditDiff != nil {
		for _, f := range r.BashEditDiff.Files {
			if f.FilePath != "" {
				files = append(files, f.FilePath)
			}
		}
	}

	return files, added, removed
}

func countHunks(hunks []hunk) (int, int) {
	var added, removed int

	for _, h := range hunks {
		for _, line := range h.Lines {
			switch {
			case strings.HasPrefix(line, "+"):
				added++
			case strings.HasPrefix(line, "-"):
				removed++
			}
		}
	}

	return added, removed
}

func splitBlocks(acts []activity, gap time.Duration) []Block {
	if len(acts) == 0 {
		return nil
	}

	sort.SliceStable(acts, func(i, j int) bool { return acts[i].at.Before(acts[j].at) })

	var blocks []Block

	cur := Block{Start: acts[0].at, End: acts[0].at}

	for i, a := range acts {
		if i > 0 && a.at.Sub(cur.End) > gap {
			blocks = append(blocks, cur)
			cur = Block{Start: a.at, End: a.at}
		}

		cur.End = a.at
		cur.Messages++
		cur.LinesAdded += a.linesAdded
		cur.LinesRemoved += a.linesRemoved

		if a.isPrompt {
			cur.Prompts++
		}

		cur.Models = inc(cur.Models, a.model)
		cur.Skills = inc(cur.Skills, a.skill)
		cur.MCP = inc(cur.MCP, a.mcp)

		for _, t := range a.tools {
			cur.Tools = inc(cur.Tools, t)
		}

		cur.Files = append(cur.Files, a.files...)
	}

	return append(blocks, cur)
}

func inc(m map[string]int, key string) map[string]int {
	if key == "" {
		return m
	}

	if m == nil {
		m = map[string]int{}
	}

	m[key]++

	return m
}

// estimateTurns fills in the turns of blocks that have no turn durations.
// acts must be sorted, as splitBlocks leaves them.
func estimateTurns(blocks []Block, acts []activity) {
	var (
		turns   = make([][]Interval, len(blocks))
		cur     *Interval
		curBlk  = -1
		closeAt = func() {
			if cur != nil && cur.End.After(cur.Start) {
				turns[curBlk] = append(turns[curBlk], *cur)
			}

			cur = nil
		}
	)

	for _, a := range acts {
		i := blockAt(blocks, a.at)
		if len(blocks[i].Active) > 0 {
			continue
		}

		if i != curBlk || a.isPrompt {
			closeAt()

			curBlk = i
			cur = &Interval{Start: a.at, End: a.at}

			continue
		}

		if cur != nil {
			cur.End = a.at
		}
	}

	closeAt()

	for i, ts := range turns {
		if len(ts) == 0 {
			continue
		}

		for _, t := range ts {
			blocks[i].ActiveMs += t.End.Sub(t.Start).Milliseconds()
		}

		blocks[i].Active = mergeIntervals(ts, activeJoinGap)
		blocks[i].ActiveEstimated = true
	}
}

// blockAt returns the index of the last block starting at or before t, or 0
// when t comes before every block.
func blockAt(blocks []Block, t time.Time) int {
	return max(sort.Search(len(blocks), func(i int) bool { return blocks[i].Start.After(t) })-1, 0)
}

// applyExtras credits each extra to the block it happened in.
func applyExtras(blocks []Block, extras []extra) {
	if len(blocks) == 0 {
		return
	}

	seenPR := map[string]bool{}

	for _, e := range extras {
		b := &blocks[blockAt(blocks, e.at)]

		switch {
		case e.turn != nil:
			t := *e.turn
			if t.Start.Before(b.Start) {
				t.Start = b.Start
			}

			b.ActiveMs += t.End.Sub(t.Start).Milliseconds()
			b.Active = append(b.Active, t)
		case e.pr != nil:
			if !seenPR[e.pr.URL] {
				seenPR[e.pr.URL] = true
				b.PRs = append(b.PRs, *e.pr)
			}
		case e.apiError:
			b.Errors++
		default:
			for _, t := range e.tools {
				b.Tools = inc(b.Tools, t)
			}

			b.Files = append(b.Files, e.files...)
			b.LinesAdded += e.linesAdded
			b.LinesRemoved += e.linesRemoved
		}
	}

	for i := range blocks {
		blocks[i].Active = mergeIntervals(blocks[i].Active, activeJoinGap)
		blocks[i].Files = unique(blocks[i].Files)
	}
}

func unique(xs []string) []string {
	if len(xs) == 0 {
		return nil
	}

	seen := make(map[string]bool, len(xs))
	out := xs[:0]

	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}

	return out
}

func mergeIntervals(in []Interval, join time.Duration) []Interval {
	if len(in) == 0 {
		return nil
	}

	sort.Slice(in, func(i, j int) bool { return in[i].Start.Before(in[j].Start) })

	out := []Interval{in[0]}

	for _, iv := range in[1:] {
		last := &out[len(out)-1]
		if iv.Start.Sub(last.End) <= join {
			if iv.End.After(last.End) {
				last.End = iv.End
			}

			continue
		}

		out = append(out, iv)
	}

	return out
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}

	return string([]rune(s)[:n]) + "…"
}
