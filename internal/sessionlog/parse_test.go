package sessionlog

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func mustTime(t *testing.T, v string) time.Time {
	t.Helper()

	tm, err := time.Parse(time.RFC3339, v)
	if err != nil {
		t.Fatal(err)
	}

	return tm
}

func TestParse(t *testing.T) {
	t.Parallel()

	log := strings.Join([]string{
		`{"type":"permission-mode","permissionMode":"default","sessionId":"s1"}`,
		`{"type":"user","timestamp":"2026-10-01T10:00:00Z","entrypoint":"cli","cwd":"/work/repo","gitBranch":"main","message":{"role":"user","content":"<command-name>/clear</command-name>"}}`,
		`{"type":"user","timestamp":"2026-10-01T10:00:05Z","cwd":"/work/repo","gitBranch":"main","message":{"role":"user","content":"fix the   flaky\ntest"}}`,
		`{"type":"assistant","timestamp":"2026-10-01T10:01:00Z","cwd":"/work/repo/sub","gitBranch":"main","message":{"content":[{"type":"text","text":"ok"}]}}`,
		`{"type":"user","timestamp":"2026-10-01T10:02:00Z","cwd":"/work/repo","message":{"content":[{"type":"tool_result","content":"done"}]}}`,
		`{"type":"user","timestamp":"2026-10-01T10:03:00Z","isMeta":true,"cwd":"/work/repo","message":{"content":"caveat"}}`,
		`{"type":"system","timestamp":"2026-10-01T12:00:00Z","cwd":"/work/repo"}`,
		`{"type":"user","timestamp":"2026-10-02T09:00:00Z","isCompactSummary":true,"cwd":"/work/repo","message":{"content":"This session is being continued"}}`,
		`{"type":"user","timestamp":"2026-10-02T09:00:30Z","cwd":"/work/repo","gitBranch":"feature","message":{"content":[{"type":"text","text":"continue"}]}}`,
		`{"type":"last-prompt","lastPrompt":"continue","sessionId":"s1"}`,
		`{"type":"assistant","timestamp":"2026-10-02T09:05:00Z","cwd":"/work/repo","gitBranch":"feature","message":{"content":[]}}`,
		`{"type":"assistant","timestamp":"2026-10-02T09:06:00Z","cwd":"/work/re`, // still being written
	}, "\n")

	s, err := Parse("s1", strings.NewReader(log), 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	if s.ID != "s1" || s.Cwd != "/work/repo" || s.Branch != "feature" || s.Entrypoint != "cli" {
		t.Errorf("got id=%q cwd=%q branch=%q entrypoint=%q", s.ID, s.Cwd, s.Branch, s.Entrypoint)
	}

	if want := "fix the flaky test"; s.Title != want {
		t.Errorf("title = %q, want %q", s.Title, want)
	}

	want := []Block{
		{Start: mustTime(t, "2026-10-01T10:00:00Z"), End: mustTime(t, "2026-10-01T10:03:00Z"), Prompts: 2, Messages: 5},
		{Start: mustTime(t, "2026-10-02T09:00:00Z"), End: mustTime(t, "2026-10-02T09:05:00Z"), Prompts: 1, Messages: 3},
	}
	if len(s.Blocks) != len(want) {
		t.Fatalf("blocks = %+v, want %+v", s.Blocks, want)
	}

	for i := range want {
		if !s.Blocks[i].Start.Equal(want[i].Start) || !s.Blocks[i].End.Equal(want[i].End) ||
			s.Blocks[i].Prompts != want[i].Prompts || s.Blocks[i].Messages != want[i].Messages {
			t.Errorf("block[%d] = %+v, want %+v", i, s.Blocks[i], want[i])
		}
	}
}

func TestParseTitle(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		log  string
		want string
	}{
		{
			name: "ai title wins",
			log: `{"type":"user","timestamp":"2026-10-01T10:00:00Z","message":{"content":"first"}}
{"type":"ai-title","aiTitle":"old"}
{"type":"ai-title","aiTitle":"Latest title"}`,
			want: "Latest title",
		},
		{
			name: "last prompt when no prompt is in the log",
			log:  `{"type":"last-prompt","lastPrompt":"only this"}`,
			want: "only this",
		},
		{
			name: "long prompt is truncated",
			log:  `{"type":"user","timestamp":"2026-10-01T10:00:00Z","message":{"content":"` + strings.Repeat("あ", 100) + `"}}`,
			want: strings.Repeat("あ", 80) + "…",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s, err := Parse("id", strings.NewReader(tt.log), time.Hour)
			if err != nil {
				t.Fatal(err)
			}

			if s.Title != tt.want {
				t.Errorf("title = %q, want %q", s.Title, tt.want)
			}
		})
	}
}

func TestParseEmpty(t *testing.T) {
	t.Parallel()

	s, err := Parse("id", strings.NewReader(""), time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	if len(s.Blocks) != 0 {
		t.Errorf("blocks = %+v, want none", s.Blocks)
	}
}

func TestParseDetails(t *testing.T) {
	t.Parallel()

	log := strings.Join([]string{
		`{"type":"user","timestamp":"2026-10-01T10:00:00Z","version":"2.1.0","slug":"calm-fox","promptSource":"typed","cwd":"/w","message":{"content":"edit it"}}`,
		// Two parts of one response repeat its usage; it must count once.
		`{"type":"assistant","timestamp":"2026-10-01T10:00:10Z","attributionSkill":"review","message":{"id":"m1","model":"opus","usage":{"input_tokens":100,"output_tokens":10},"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/w/a.go"}}]}}`,
		`{"type":"assistant","timestamp":"2026-10-01T10:00:11Z","message":{"id":"m1","model":"opus","usage":{"input_tokens":100,"output_tokens":20},"content":[{"type":"tool_use","name":"Bash","input":{"command":"sed -i s/a/b/ b.go"}}]}}`,
		`{"type":"user","timestamp":"2026-10-01T10:00:12Z","toolUseResult":{"filePath":"/w/a.go","structuredPatch":[{"lines":[" ctx","-old","+new","+more"]}]},"message":{"content":[{"type":"tool_result","content":"ok"}]}}`,
		`{"type":"user","timestamp":"2026-10-01T10:00:13Z","toolUseResult":{"stdout":"","bashEditDiff":{"files":[{"filePath":"/w/b.go","hunks":[{"lines":["-x","+y"]}]}]}},"message":{"content":[{"type":"tool_result","content":"ok"}]}}`,
		`{"type":"assistant","timestamp":"2026-10-01T10:00:20Z","attributionMcpServer":"example-mcp","message":{"id":"m2","model":"<synthetic>","content":[{"type":"tool_use","name":"Write","input":{"file_path":"/w/a.go"}}]}}`,
		`{"type":"user","timestamp":"2026-10-01T10:00:21Z","toolUseResult":{"type":"create","filePath":"/w/c.go","content":"package c\n\nfunc C() {}\n"},"message":{"content":[{"type":"tool_result","content":"ok"}]}}`,
		`{"type":"system","subtype":"turn_duration","timestamp":"2026-10-01T10:01:00Z","durationMs":60000}`,
		`{"type":"user","timestamp":"2026-10-01T10:03:00Z","promptSource":"system","turnOrigin":"peer","message":{"content":"message from another session"}}`,
		`{"type":"system","subtype":"turn_duration","timestamp":"2026-10-01T10:04:00Z","durationMs":60000}`,
		`{"type":"pr-link","timestamp":"2026-10-01T10:04:30Z","prNumber":7,"prRepository":"o/r","prUrl":"https://github.com/o/r/pull/7"}`,
		`{"type":"pr-link","timestamp":"2026-10-01T10:04:40Z","prNumber":7,"prRepository":"o/r","prUrl":"https://github.com/o/r/pull/7"}`,
		`{"type":"user","timestamp":"2026-10-01T12:00:00Z","promptSource":"queued","message":{"content":"next"}}`,
		`{"type":"assistant","timestamp":"2026-10-01T12:00:05Z","message":{"id":"m3","model":"opus","usage":{"input_tokens":50,"output_tokens":10,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0}},"content":[]}}`,
		`{"type":"system","subtype":"api_error","timestamp":"2026-10-01T12:00:06Z"}`,
		`{"type":"system","subtype":"turn_duration","timestamp":"2026-10-01T12:00:30Z","durationMs":30000}`,
		// Written once at the end; the [1m] suffix is not in message model names,
		// and haiku made background calls that left no messages.
		`{"type":"cost-state","totalCostUSD":4,"modelUsage":{"opus[1m]":{"outputTokens":30,"costUSD":3},"haiku":{"outputTokens":5,"costUSD":1}}}`,
	}, "\n")

	s, err := Parse("s", strings.NewReader(log), 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	if s.Version != "2.1.0" || s.Slug != "calm-fox" {
		t.Errorf("version=%q slug=%q", s.Version, s.Slug)
	}

	if len(s.Blocks) != 2 {
		t.Fatalf("blocks = %+v, want 2", s.Blocks)
	}

	b, b2 := s.Blocks[0], s.Blocks[1]

	if b.Prompts != 1 {
		t.Errorf("prompts = %d, want 1 (a message from another session is not a prompt)", b.Prompts)
	}

	if b.ActiveMs != 120000 {
		t.Errorf("activeMs = %d, want 120000", b.ActiveMs)
	}

	wantActive := Interval{Start: mustTime(t, "2026-10-01T10:00:00Z"), End: mustTime(t, "2026-10-01T10:04:00Z")}
	if len(b.Active) != 1 || !b.Active[0].Start.Equal(wantActive.Start) || !b.Active[0].End.Equal(wantActive.End) {
		t.Errorf("active = %+v, want turns 2 minutes apart merged into %+v", b.Active, wantActive)
	}

	if got := b.Tokens["opus"]; got.Input != 100 || got.Output != 20 {
		t.Errorf("tokens = %+v, want one response counted once with its last usage", got)
	}

	// Weights: block 1 = 100 + 5*20 = 200, block 2 = 50 + 5*10 = 100.
	const eps = 1e-9
	if want := 3.0*2/3 + 1.0*2/3; b.CostUSD < want-eps || b.CostUSD > want+eps {
		t.Errorf("cost = %v, want %v", b.CostUSD, want)
	}

	if want := 3.0/3 + 1.0/3; b2.CostUSD < want-eps || b2.CostUSD > want+eps {
		t.Errorf("second block cost = %v, want %v", b2.CostUSD, want)
	}

	if b.LinesAdded != 2+4 || b.LinesRemoved != 1 {
		t.Errorf("lines = +%d -%d, want +6 -1 from the edit and the created file, not the Bash edit", b.LinesAdded, b.LinesRemoved)
	}

	if b.Tools["Edit"] != 1 || b.Tools["Bash"] != 1 || b.Tools["Write"] != 1 {
		t.Errorf("tools = %v", b.Tools)
	}

	if !slices.Equal(b.Files, []string{"/w/a.go", "/w/b.go", "/w/c.go"}) {
		t.Errorf("files = %v, want one entry per file", b.Files)
	}

	if len(b.Models) != 1 || b.Models["opus"] != 2 {
		t.Errorf("models = %v, want synthetic messages left out", b.Models)
	}

	if b.Skills["review"] != 1 || b.MCP["example-mcp"] != 1 {
		t.Errorf("skills = %v, mcp = %v", b.Skills, b.MCP)
	}

	if len(b.PRs) != 1 || b.PRs[0].Number != 7 {
		t.Errorf("prs = %+v, want the PR once", b.PRs)
	}

	if b2.Prompts != 1 || b2.Errors != 1 || b2.ActiveMs != 30000 {
		t.Errorf("second block = %+v", b2)
	}

	if s.Usage["opus[1m]"].OutputTokens != 30 {
		t.Errorf("usage = %+v", s.Usage)
	}
}

func TestParseCostRestart(t *testing.T) {
	t.Parallel()

	log := strings.Join([]string{
		`{"type":"assistant","timestamp":"2026-10-01T10:00:00Z","message":{"id":"m1","model":"opus","usage":{"output_tokens":10},"content":[]}}`,
		`{"type":"cost-state","modelUsage":{"opus":{"costUSD":2}}}`,
		`{"type":"cost-state","modelUsage":{"opus":{"costUSD":3}}}`,
		`{"type":"cost-state","modelUsage":{"opus":{"costUSD":0.5}}}`,
	}, "\n")

	s, err := Parse("s", strings.NewReader(log), time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	if got := s.Blocks[0].CostUSD; got != 3.5 {
		t.Errorf("cost = %v, want 3.5 (a total that restarted adds to what came before)", got)
	}
}

func TestParseEstimatedTurns(t *testing.T) {
	t.Parallel()

	log := strings.Join([]string{
		`{"type":"user","timestamp":"2026-10-01T10:00:00Z","promptSource":"sdk","message":{"content":"task 1"}}`,
		`{"type":"assistant","timestamp":"2026-10-01T10:00:30Z","message":{"content":[]}}`,
		`{"type":"assistant","timestamp":"2026-10-01T10:01:00Z","message":{"content":[]}}`,
		`{"type":"user","timestamp":"2026-10-01T10:20:00Z","promptSource":"sdk","message":{"content":"task 2"}}`,
		`{"type":"assistant","timestamp":"2026-10-01T10:22:00Z","message":{"content":[]}}`,
		`{"type":"user","timestamp":"2026-10-01T10:25:00Z","promptSource":"sdk","message":{"content":"unanswered"}}`,
	}, "\n")

	s, err := Parse("s", strings.NewReader(log), time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	b := s.Blocks[0]

	if !b.ActiveEstimated || b.ActiveMs != (60+120)*1000 {
		t.Errorf("activeMs = %d estimated = %v, want 180000 estimated", b.ActiveMs, b.ActiveEstimated)
	}

	if len(b.Active) != 2 {
		t.Errorf("active = %+v, want the two answered turns", b.Active)
	}
}
