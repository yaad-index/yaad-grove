package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/alecthomas/kong"
	kongyaml "github.com/alecthomas/kong-yaml"
	"github.com/yaad-index/bonyan/secret"
	"gopkg.in/yaml.v3"

	"github.com/yaad-index/yaad-grove/internal/budget"
	"github.com/yaad-index/yaad-grove/internal/core"
)

// ReplayCmd answers a fixed set of questions with the configured engine and
// compares two such runs, so a change to the answer path can be read side by
// side, on the same questions, before it is deployed.
type ReplayCmd struct {
	Answer  ReplayRunCmd     `cmd:"" name:"run" help:"Answer every question in a file with the engine serve would build, and write one JSON line per answer."`
	Compare ReplayCompareCmd `cmd:"" help:"Print two runs side by side, flagging changed refusals, errors and empty answers."`
}

// ReplayRunCmd answers each question once, standalone: no history, no reply,
// no asker, no consent gate and no long-term memory. It takes serve's flags,
// and reads serve's section of the configuration file, so it answers with the
// deployment's model, prompt and tools.
type ReplayRunCmd struct {
	ServeCmd `embed:""`

	Questions string        `name:"questions" required:"" type:"existingfile" help:"Questions as JSON lines, each with an id and a query; a query_en, when present, is asked too, as <id>@en."`
	Out       string        `name:"out" required:"" type:"path" help:"File the answers are written to, one JSON line each. It must not exist yet."`
	Timeout   time.Duration `name:"timeout" default:"3m" help:"Longest one question may take before it is recorded as an error."`
}

// replayUser is the user every replayed question is asked as.
const replayUser = "replay"

// replayQuestion is one question to answer.
type replayQuestion struct {
	ID   string
	Text string
}

// replayAnswer is one line of a run's output.
type replayAnswer struct {
	ID       string `json:"id"`
	Question string `json:"question"`
	Answer   string `json:"answer"`
	Refused  bool   `json:"refused"`
	Error    string `json:"error,omitempty"`
	MS       int64  `json:"ms"`
}

// answerer is the engine as a replay uses it.
type answerer interface {
	Answer(ctx context.Context, q core.Query) (core.Reply, error)
}

// Run builds the engine and answers every question.
func (r *ReplayRunCmd) Run(log *slog.Logger) error {
	c := &r.ServeCmd
	if c.ContextSize <= 0 {
		return fmt.Errorf("--context-size is required: set the retrieved-material token cap for the chosen model's window (e.g. 8000); there is no default because the right cap depends on the model")
	}
	questions, err := readQuestions(r.Questions)
	if err != nil {
		return err
	}
	out, err := os.OpenFile(r.Out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("replay: %w", err)
	}
	defer func() { _ = out.Close() }()

	// The replay's meter is its own and in memory, so a replay never spends the
	// bot's persisted budget, yet still stops at the configured ceiling.
	meter, err := budget.New(budget.Config{Ceiling: c.SpendCeiling, Period: c.SpendPeriod}, &budget.MemoryStore{})
	if err != nil {
		return err
	}
	if c.LongMemoryURL != "" {
		log.Info("replay: long-term memory is configured but not used; a replay neither recalls nor keeps anything")
	}
	a, err := c.buildAnswering(log, meter, secret.NewResolver(secret.Env{}), nil)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := a.registry.Connect(ctx); err != nil {
		return err
	}
	defer func() { _ = a.registry.Close() }()

	log.Info("replay: answering", "questions", len(questions), "model", c.ModelName, "out", r.Out)
	if err := replay(ctx, a.engine, questions, out, r.Timeout, log); err != nil {
		return err
	}
	return out.Close()
}

// replay answers each question in turn and writes its line as soon as it is
// answered, so an interrupted run keeps what it finished. A question that
// fails or times out is written with its error and the run goes on; only a
// failed write, or ctx ending, stops it.
func replay(ctx context.Context, e answerer, questions []replayQuestion, w io.Writer, timeout time.Duration, log *slog.Logger) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	for i, q := range questions {
		if err := ctx.Err(); err != nil {
			return err
		}
		qctx, cancel := context.WithTimeout(ctx, timeout)
		start := time.Now()
		reply, err := e.Answer(qctx, core.Query{User: core.User{ID: replayUser}, Surface: core.SurfaceGroup, Text: q.Text})
		elapsed := time.Since(start)
		cancel()
		line := replayAnswer{ID: q.ID, Question: q.Text, Answer: reply.Text, Refused: reply.Refused, MS: elapsed.Milliseconds()}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			line = replayAnswer{ID: q.ID, Question: q.Text, Error: err.Error(), MS: elapsed.Milliseconds()}
		}
		if err := enc.Encode(line); err != nil {
			return fmt.Errorf("replay: write %s: %w", q.ID, err)
		}
		log.Info("replay: answered", "n", i+1, "of", len(questions), "id", q.ID, "refused", line.Refused, "error", line.Error != "", "ms", line.MS)
	}
	return nil
}

// readQuestions reads a JSON-lines question file. Every line needs an id and
// a query; a query_en is asked as a second question with id <id>@en. Blank
// lines are skipped, other fields ignored, and a repeated id is an error,
// since the comparison joins two runs by id.
func readQuestions(path string) ([]replayQuestion, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("replay: %w", err)
	}
	defer func() { _ = f.Close() }()
	var out []replayQuestion
	seen := map[string]bool{}
	add := func(id, text string) error {
		if seen[id] {
			return fmt.Errorf("replay: %s: id %q appears twice", path, id)
		}
		seen[id] = true
		out = append(out, replayQuestion{ID: id, Text: text})
		return nil
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for n := 1; sc.Scan(); n++ {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var q struct {
			ID      string `json:"id"`
			Query   string `json:"query"`
			QueryEN string `json:"query_en"`
		}
		if err := json.Unmarshal(line, &q); err != nil {
			return nil, fmt.Errorf("replay: %s line %d: %w", path, n, err)
		}
		if q.ID == "" || strings.TrimSpace(q.Query) == "" {
			return nil, fmt.Errorf("replay: %s line %d: needs an id and a query", path, n)
		}
		if err := add(q.ID, q.Query); err != nil {
			return nil, err
		}
		if strings.TrimSpace(q.QueryEN) != "" {
			if err := add(q.ID+"@en", q.QueryEN); err != nil {
				return nil, err
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("replay: %s: %w", path, err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("replay: %s holds no questions", path)
	}
	return out, nil
}

// ReplayCompareCmd prints two runs side by side.
type ReplayCompareCmd struct {
	Old string `arg:"" type:"existingfile" help:"The earlier run's output."`
	New string `arg:"" type:"existingfile" help:"The later run's output."`
}

// Run prints the comparison to standard output.
func (r *ReplayCompareCmd) Run() error {
	older, err := readAnswers(r.Old)
	if err != nil {
		return err
	}
	newer, err := readAnswers(r.New)
	if err != nil {
		return err
	}
	return compare(older, newer, os.Stdout)
}

// readAnswers reads a run's output.
func readAnswers(path string) ([]replayAnswer, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("replay: %w", err)
	}
	var out []replayAnswer
	dec := json.NewDecoder(bytes.NewReader(b))
	for {
		var a replayAnswer
		err := dec.Decode(&a)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("replay: %s: %w", path, err)
		}
		out = append(out, a)
	}
}

// compare writes a summary, then every question with both answers, in the
// newer run's order followed by any only the older run has. Each question
// carries its flags: a refusal that changed, an error, an empty answer, or a
// question one run lacks.
func compare(older, newer []replayAnswer, w io.Writer) error {
	byID := make(map[string]replayAnswer, len(older))
	for _, a := range older {
		byID[a.ID] = a
	}
	type pair struct {
		id       string
		old, new *replayAnswer
		flags    []string
	}
	var pairs []pair
	inNew := make(map[string]bool, len(newer))
	for i := range newer {
		n := &newer[i]
		inNew[n.ID] = true
		p := pair{id: n.ID, new: n}
		if o, ok := byID[n.ID]; ok {
			p.old = &o
		}
		pairs = append(pairs, p)
	}
	for i := range older {
		if o := &older[i]; !inNew[o.ID] {
			pairs = append(pairs, pair{id: o.ID, old: o})
		}
	}

	counts := map[string]int{}
	var flagged []string
	for i := range pairs {
		p := &pairs[i]
		switch {
		case p.old == nil:
			p.flags = append(p.flags, "only in new")
		case p.new == nil:
			p.flags = append(p.flags, "only in old")
		case p.old.Error == "" && p.new.Error == "" && p.old.Refused != p.new.Refused:
			p.flags = append(p.flags, fmt.Sprintf("refusal changed: %s -> %s", outcome(*p.old), outcome(*p.new)))
		}
		for _, side := range []struct {
			name string
			a    *replayAnswer
		}{{"old", p.old}, {"new", p.new}} {
			switch {
			case side.a == nil:
			case side.a.Error != "":
				p.flags = append(p.flags, side.name+" error")
			case strings.TrimSpace(side.a.Answer) == "":
				p.flags = append(p.flags, side.name+" empty")
			}
		}
		for _, f := range p.flags {
			counts[strings.SplitN(f, ":", 2)[0]]++
		}
		if len(p.flags) > 0 {
			flagged = append(flagged, p.id)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "replay compare: %d questions, %d flagged", len(pairs), len(flagged))
	for _, k := range []string{"refusal changed", "old error", "new error", "old empty", "new empty", "only in old", "only in new"} {
		if counts[k] > 0 {
			fmt.Fprintf(&b, "; %s %d", k, counts[k])
		}
	}
	b.WriteString("\n")
	if len(flagged) > 0 {
		fmt.Fprintf(&b, "flagged: %s\n", strings.Join(flagged, ", "))
	}
	for _, p := range pairs {
		fmt.Fprintf(&b, "\n## %s", p.id)
		if len(p.flags) > 0 {
			fmt.Fprintf(&b, "  [%s]", strings.Join(p.flags, "; "))
		}
		b.WriteString("\n")
		q := p.new
		if q == nil {
			q = p.old
		}
		fmt.Fprintf(&b, "question: %s\n", q.Question)
		writeSide(&b, "old", p.old)
		writeSide(&b, "new", p.new)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// outcome names whether a run answered or refused.
func outcome(a replayAnswer) string {
	if a.Refused {
		return "refused"
	}
	return "answered"
}

// writeSide writes one run's answer to a question.
func writeSide(b *strings.Builder, name string, a *replayAnswer) {
	if a == nil {
		fmt.Fprintf(b, "--- %s: not asked\n", name)
		return
	}
	switch {
	case a.Error != "":
		fmt.Fprintf(b, "--- %s (%d ms) error: %s\n", name, a.MS, a.Error)
	case a.Refused:
		fmt.Fprintf(b, "--- %s (%d ms) refused\n%s\n", name, a.MS, a.Answer)
	default:
		fmt.Fprintf(b, "--- %s (%d ms)\n%s\n", name, a.MS, a.Answer)
	}
}

// configLoader reads the YAML configuration file with kong-yaml, and lets
// `replay run` take its values from serve's section, since a replay answers as
// the bot configured there does. A replay section of the file's own is used
// instead when there is one.
func configLoader(r io.Reader) (kong.Resolver, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	config := map[string]any{}
	if err := yaml.Unmarshal(b, &config); err != nil {
		return nil, fmt.Errorf("YAML config decode error: %w", err)
	}
	if serve, ok := config["serve"]; ok {
		if _, own := config["replay"]; !own {
			config["replay"] = map[string]any{"run": serve}
		}
	}
	out, err := yaml.Marshal(config)
	if err != nil {
		return nil, err
	}
	return kongyaml.Loader(bytes.NewReader(out))
}
