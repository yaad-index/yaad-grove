package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alecthomas/kong"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-grove/internal/core"
)

func writeFile(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// Each line gives a question; query_en is asked too, as <id>@en; blank lines
// and other fields are ignored.
func TestReadQuestions(t *testing.T) {
	path := writeFile(t, "q.jsonl", `{"id":"q1","query":"سلام","query_en":"hello","expect":"x","expect_source":["a"],"note":""}

{"id":"q2","query":"only one"}
`)
	qs, err := readQuestions(path)
	require.NoError(t, err)
	assert.Equal(t, []replayQuestion{{"q1", "سلام"}, {"q1@en", "hello"}, {"q2", "only one"}}, qs)
}

// A file a comparison could not join by id, or one with nothing to ask, is
// refused before anything is spent.
func TestReadQuestionsErrors(t *testing.T) {
	cases := map[string]string{
		"repeated id":     `{"id":"q1","query":"a"}` + "\n" + `{"id":"q1","query":"b"}`,
		"repeated by @en": `{"id":"q1","query":"a","query_en":"b"}` + "\n" + `{"id":"q1@en","query":"c"}`,
		"@en repeats":     `{"id":"q1@en","query":"a"}` + "\n" + `{"id":"q1","query":"b","query_en":"c"}`,
		"no id":           `{"query":"a"}`,
		"no query":        `{"id":"q1","query":"  "}`,
		"not json":        `{"id":`,
		"empty":           "\n\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := readQuestions(writeFile(t, "q.jsonl", body))
			require.Error(t, err)
		})
	}
}

// fakeEngine answers from a table and records every query.
type fakeEngine struct {
	mu      sync.Mutex
	queries []core.Query
	replies map[string]core.Reply
	errs    map[string]error
	block   string
}

func (f *fakeEngine) Answer(ctx context.Context, q core.Query) (core.Reply, error) {
	f.mu.Lock()
	f.queries = append(f.queries, q)
	f.mu.Unlock()
	if q.Text == f.block {
		<-ctx.Done()
		return core.Reply{}, ctx.Err()
	}
	if err := f.errs[q.Text]; err != nil {
		return core.Reply{}, err
	}
	return f.replies[q.Text], nil
}

// Every question is asked standalone, and written with its answer, its
// refusal, or its error; a failed or timed-out question does not stop the run.
func TestReplay(t *testing.T) {
	e := &fakeEngine{
		replies: map[string]core.Reply{"a": {Text: "answer a"}, "b": {Text: "not mine", Refused: true}},
		errs:    map[string]error{"c": errors.New("model down")},
		block:   "slow",
	}
	qs := []replayQuestion{{"1", "a"}, {"2", "b"}, {"3", "c"}, {"4", "slow"}, {"5", "a"}}
	var out bytes.Buffer
	require.NoError(t, replay(context.Background(), e, qs, &out, 50*time.Millisecond, discard))

	got, err := readAnswers(writeFile(t, "out.jsonl", out.String()))
	require.NoError(t, err)
	require.Len(t, got, 5)
	assert.Equal(t, replayAnswer{ID: "1", Question: "a", Answer: "answer a", MS: got[0].MS}, got[0])
	assert.True(t, got[1].Refused)
	assert.Equal(t, "not mine", got[1].Answer)
	assert.Equal(t, "model down", got[2].Error)
	assert.Contains(t, got[3].Error, "deadline", "a question past its timeout is recorded as an error")
	assert.Less(t, got[3].MS, int64(5000), "and is cut off at the timeout")
	assert.Equal(t, "answer a", got[4].Answer, "the run goes on after an error")

	for _, q := range e.queries {
		assert.Equal(t, core.Query{User: core.User{ID: replayUser}, Surface: core.SurfaceGroup, Text: q.Text}, q,
			"no history, reply, asker, chat or memory")
	}
}

// Ending the run's context stops it after the question in flight, keeping
// what was already written.
func TestReplayStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	e := &fakeEngine{replies: map[string]core.Reply{"a": {Text: "x"}}, block: "slow"}
	var out bytes.Buffer
	go func() {
		for {
			e.mu.Lock()
			n := len(e.queries)
			e.mu.Unlock()
			if n == 2 {
				cancel()
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	err := replay(ctx, e, []replayQuestion{{"1", "a"}, {"2", "slow"}, {"3", "a"}}, &out, time.Minute, discard)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, strings.Count(out.String(), "\n"), "the finished question is kept, the interrupted one is not written")
	assert.Len(t, e.queries, 2, "nothing is asked after the stop")
}

// A run whose context has already ended asks nothing.
func TestReplayStoppedBeforeStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e := &fakeEngine{replies: map[string]core.Reply{"a": {Text: "x"}}}
	var out bytes.Buffer
	require.ErrorIs(t, replay(ctx, e, []replayQuestion{{"1", "a"}}, &out, time.Minute, discard), context.Canceled)
	assert.Empty(t, e.queries)
	assert.Empty(t, out.String())
}

// failWriter fails every write.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

// A failed write stops the run, since what it answers would be lost.
func TestReplayWriteError(t *testing.T) {
	e := &fakeEngine{replies: map[string]core.Reply{"a": {Text: "x"}}}
	err := replay(context.Background(), e, []replayQuestion{{"1", "a"}, {"2", "a"}}, failWriter{}, time.Minute, discard)
	require.ErrorContains(t, err, "disk full")
	assert.Len(t, e.queries, 1)
}

// The comparison flags what changed, follows the newer run's order and keeps
// questions only one run has.
func TestCompare(t *testing.T) {
	older := []replayAnswer{
		{ID: "same", Question: "q same", Answer: "A", MS: 10},
		{ID: "flip", Question: "q flip", Answer: "B"},
		{ID: "err", Question: "q err", Answer: "C"},
		{ID: "gone", Question: "q gone", Answer: "D"},
	}
	newer := []replayAnswer{
		{ID: "flip", Question: "q flip", Answer: "no", Refused: true},
		{ID: "same", Question: "q same", Answer: "A2", MS: 20},
		{ID: "err", Question: "q err", Error: "boom"},
		{ID: "empty", Question: "q empty", Answer: " "},
	}
	var out bytes.Buffer
	require.NoError(t, compare(older, newer, &out))
	s := out.String()

	assert.True(t, strings.HasPrefix(s, "replay compare: 5 questions, 4 flagged; refusal changed 1; new error 1; new empty 1; only in old 1; only in new 1\nflagged: flip, err, empty, gone\n"), s)
	assert.Contains(t, s, "## flip  [refusal changed: answered -> refused]\nquestion: q flip\n--- old (0 ms)\nB\n--- new (0 ms) refused\nno\n")
	assert.Contains(t, s, "## same\nquestion: q same\n--- old (10 ms)\nA\n--- new (20 ms)\nA2\n")
	assert.Contains(t, s, "## err  [new error]\n")
	assert.NotContains(t, s, "refusal changed: answered -> answered")
	assert.Contains(t, s, "## empty  [only in new; new empty]\nquestion: q empty\n--- old: not asked\n")
	assert.Contains(t, s, "## gone  [only in old]\nquestion: q gone\n--- old (0 ms)\nD\n--- new: not asked\n")
	assert.Less(t, strings.Index(s, "## flip"), strings.Index(s, "## same"), "newer run's order")
	assert.Less(t, strings.Index(s, "## empty"), strings.Index(s, "## gone"), "older-only questions last")
}

// `replay run` reads serve's section of the configuration file, unless the
// file has a replay section of its own; serve still reads its own.
func TestConfigLoaderReplayReadsServe(t *testing.T) {
	parse := func(t *testing.T, config string, args ...string) CLI {
		t.Helper()
		var cli CLI
		parser, err := kong.New(&cli, kong.Configuration(configLoader, writeFile(t, "config.yaml", config)))
		require.NoError(t, err)
		_, err = parser.Parse(args)
		require.NoError(t, err)
		return cli
	}
	q := writeFile(t, "q.jsonl", `{"id":"1","query":"a"}`)
	serve := "log-level: warn\nserve:\n  vault-dir: /srv/vault\n  model-name: m1\n  context-size: 8000\n  mcp-allow: ['svc=a,b']\n"

	cli := parse(t, serve, "replay", "run", "--questions", q, "--out", "o.jsonl")
	assert.Equal(t, "/srv/vault", cli.Replay.Answer.VaultDir)
	assert.Equal(t, "m1", cli.Replay.Answer.ModelName)
	assert.Equal(t, 8000, cli.Replay.Answer.ContextSize)
	assert.Equal(t, []string{"svc=a,b"}, cli.Replay.Answer.MCPAllow)
	assert.Equal(t, "warn", cli.LogLevel)

	cli = parse(t, serve, "replay", "run", "--questions", q, "--out", "o.jsonl", "--model-name", "m2")
	assert.Equal(t, "m2", cli.Replay.Answer.ModelName, "a flag still wins over the file")

	cli = parse(t, serve, "serve")
	assert.Equal(t, "/srv/vault", cli.Serve.VaultDir)

	cli = parse(t, serve+"replay:\n  run:\n    vault-dir: /other\n", "replay", "run", "--questions", q, "--out", "o.jsonl")
	assert.Equal(t, "/other", cli.Replay.Answer.VaultDir, "a replay section of the file's own wins")
}

// End to end: `replay run` builds serve's engine from the flags, asks the
// model, and writes the answer; an existing output file is never overwritten.
func TestReplayRunEndToEnd(t *testing.T) {
	var requests int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"the vault says hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer srv.Close()

	dir := t.TempDir()
	out := filepath.Join(dir, "out.jsonl")
	cmd := &ReplayRunCmd{
		ServeCmd: ServeCmd{
			VaultDir: tempVault(t), Scope: "notes", Language: "en", ModelBaseURL: srv.URL, ModelName: "m",
			SimilarityThreshold: 0.3, ContextSize: 8000,
			SpendCeiling: 1000, SpendPeriod: time.Hour, BudgetDB: filepath.Join(dir, "budget.db"),
		},
		Questions: writeFile(t, "q.jsonl", `{"id":"1","query":"hello world"}`),
		Out:       out,
		Timeout:   time.Minute,
	}
	t.Chdir(dir)
	require.NoError(t, cmd.Run(discard))

	got, err := readAnswers(out)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "the vault says hello", got[0].Answer)
	assert.Empty(t, got[0].Error)
	assert.Equal(t, 1, requests)
	_, err = os.Stat(cmd.BudgetDB)
	assert.ErrorIs(t, err, os.ErrNotExist, "a replay never opens the bot's budget")

	require.ErrorContains(t, cmd.Run(discard), "exists", "an existing output is never overwritten")
	assert.Equal(t, 1, requests)
}
