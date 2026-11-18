package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// Queue lands one repo's requests, strictly serially.
type Queue struct {
	cfg    *Config
	rc     RepoConf
	in     Intake
	log    *Logger
	shadow bool
	git    Git
	now    func() time.Time
	// Verify runs the verifier in the clone (a seam so a test can move trunk mid-verify).
	Verify func(base, logPath string) Verdict

	labelledAt map[int]time.Time
	firstAt    map[int]time.Time
	retry      map[int]*retryState
	first      map[int]bool    // requests carrying the first label at the last poll
	land       map[int]bool    // requests carrying the land label at the last poll
	tried      map[int]bool    // requests already run in this Tick
	judged     map[string]bool // shadow: (request, head, trunk) keys already judged
	key        string          // the (request, head, trunk) being judged
}

type retryState struct {
	attempts int
	next     time.Time
}

// Verdict is one verifier run. Exit 0 is pass, 1 fail, 2 could not run.
type Verdict struct {
	Exit      int
	JSON      json.RawMessage // the `VERIFY {json}` payload
	Tail, Why string
}

// NewQueue builds the queue for one repo.
func NewQueue(cfg *Config, rc RepoConf, in Intake, log *Logger, shadow bool) *Queue {
	q := &Queue{cfg: cfg, rc: rc, in: in, log: log, shadow: shadow, now: time.Now,
		git:        Git{Dir: filepath.Join(cfg.WorkDir, rc.Name)},
		labelledAt: map[int]time.Time{}, firstAt: map[int]time.Time{}, tried: map[int]bool{}, retry: map[int]*retryState{}, judged: map[string]bool{}}
	q.Verify = q.runVerify
	return q
}

func (q *Queue) f(n int, kv ...any) map[string]any {
	m := map[string]any{"repo": q.rc.Name, "request": n, "shadow": q.shadow}
	for i := 0; i+1 < len(kv); i += 2 {
		m[fmt.Sprint(kv[i])] = kv[i+1]
	}
	return m
}

// Tick runs every eligible request once. It polls again before each one, so a request carrying
// the first label that arrives while another is landing goes next. It returns each outcome.
func (q *Queue) Tick() map[int]string {
	out := map[int]string{}
	q.tried = map[int]bool{}
	for {
		if draining.Load() {
			return out
		}
		order, ok := q.order()
		if !ok {
			return out
		}
		next := 0
		for _, n := range order {
			if !q.tried[n] && q.eligible(n) {
				next = n
				break
			}
		}
		if next == 0 {
			return out
		}
		q.tried[next] = true
		out[next] = q.Land(next)
	}
}

// eligible is false while a request waits out a could-not-run backoff.
func (q *Queue) eligible(n int) bool {
	r := q.retry[n]
	return r == nil || !q.now().Before(r.next)
}

// order polls the land and first labels and returns the requests to run, first-label requests
// ahead of the rest, each group oldest label time first. ok is false when the poll failed.
func (q *Queue) order() (order []int, ok bool) {
	land, err := q.in.Labelled(q.cfg.Label)
	q.first = map[int]bool{}
	if err == nil && q.cfg.FirstLabel != "" {
		var first []int
		if first, err = q.in.Labelled(q.cfg.FirstLabel); err == nil {
			for _, n := range first {
				q.first[n] = true
			}
		}
	}
	if err != nil {
		q.log.Error("poll_failed", q.f(0, "error", err.Error()))
		return nil, false
	}
	live := map[int]bool{}
	hasLand := map[int]bool{}
	for _, n := range land {
		live[n], hasLand[n] = true, true
	}
	for n := range q.first {
		live[n] = true
	}
	q.land = hasLand
	stamp := func(cache map[int]time.Time, n int, label string) {
		if !cache[n].IsZero() {
			return
		}
		t, err := q.in.LabelledAt(n, label)
		if err != nil {
			q.log.Error("label_time_failed", q.f(n, "error", err.Error(), "label", label))
			return
		}
		cache[n] = t
	}
	for n := range live {
		if hasLand[n] {
			stamp(q.labelledAt, n, q.cfg.Label)
		}
		if q.first[n] {
			stamp(q.firstAt, n, q.cfg.FirstLabel)
		}
	}
	for n := range q.labelledAt {
		if !live[n] { // gone: a later request with the same number is a new request
			delete(q.labelledAt, n)
		}
	}
	for n := range q.firstAt {
		if !live[n] || !q.first[n] {
			delete(q.firstAt, n)
		}
	}
	for n := range q.retry {
		if !live[n] {
			delete(q.retry, n)
		}
	}
	at := func(n int) time.Time {
		if q.first[n] {
			return q.firstAt[n]
		}
		return q.labelledAt[n]
	}
	for n := range live {
		if !at(n).IsZero() {
			order = append(order, n)
		}
	}
	sort.Slice(order, func(i, j int) bool {
		a, b := order[i], order[j]
		if q.first[a] != q.first[b] {
			return q.first[a]
		}
		if !at(a).Equal(at(b)) {
			return at(a).Before(at(b))
		}
		return a < b
	})
	return order, true
}

// Land runs one request to an outcome.
func (q *Queue) Land(n int) string {
	q.key = ""
	pr, err := q.in.Pull(n)
	if err != nil {
		return q.couldNotRun(n, pr, "read request", err.Error(), Verdict{})
	}
	if pr.State != "open" {
		return "skipped-not-open"
	}
	if err := q.git.Ensure(q.rc.Remote); err != nil {
		return q.couldNotRun(n, pr, "clone", err.Error(), Verdict{})
	}
	for loop := 0; loop < q.cfg.MaxReloops; loop++ {
		if draining.Load() {
			q.log.Info("land_left_queued", q.f(n, "why", "draining for a stop; the request is untouched"))
			return "drained"
		}
		tip, head, err := q.git.Fetch(q.rc.Trunk, n)
		if err != nil {
			return q.couldNotRun(n, pr, "fetch", err.Error(), Verdict{})
		}
		q.key = fmt.Sprintf("%d:%s:%s", n, head, tip)
		if q.shadow && q.judged[q.key] {
			return "shadow-already-judged"
		}
		if err := q.git.Rebase(head, tip); err != nil {
			if errors.Is(err, ErrConflict) {
				return q.reject(n, pr, "conflict", "Conflicts with trunk, rebase onto `"+q.rc.Trunk+"` and push the request again.", Verdict{Tail: err.Error()})
			}
			return q.couldNotRun(n, pr, "rebase", err.Error(), Verdict{})
		}
		cand, tree, err := q.git.Squash(tip, pr, q.cfg.AuthorName, q.cfg.AuthorEmail)
		if errors.Is(err, ErrEmpty) {
			return q.reject(n, pr, "empty", "Nothing to land: after rebasing onto trunk this request changes no file.", Verdict{})
		}
		if err != nil {
			return q.couldNotRun(n, pr, "squash", err.Error(), Verdict{})
		}
		if _, err := q.git.git("checkout", "--force", "--detach", cand); err != nil {
			return q.couldNotRun(n, pr, "checkout candidate", err.Error(), Verdict{})
		}
		logPath := filepath.Join(q.cfg.WorkDir, "logs", fmt.Sprintf("%s-%d-%s.log", q.rc.Name, n, cand[:12]))
		q.log.Info("verify_start", q.f(n, "head", head, "trunk", tip, "candidate", cand, "log", logPath))
		v := q.Verify(tip, logPath)
		q.log.Info("verify_done", q.f(n, "exit", v.Exit, "candidate", cand))
		rec := map[string]any{"head": head, "trunk": tip, "candidate": cand, "tree": tree, "exit": v.Exit, "verify": v.JSON}
		switch v.Exit {
		case 0:
		case 1:
			return q.reject(n, pr, "fail", "Verify failed on candidate `"+cand+"` (trunk `"+tip+"`).", v)
		default:
			return q.couldNotRun(n, pr, "verify", v.Why, v)
		}
		delete(q.retry, n)
		if q.shadow {
			q.judged[q.key] = true
			q.record(n, "pass", rec)
			return "shadow-pass"
		}
		if err := q.git.PushFF(q.shadow, q.rc.Trunk, tip, cand); errors.Is(err, ErrTrunkMoved) {
			q.log.Info("trunk_moved", q.f(n, "trunk", tip, "candidate", cand, "loop", loop))
			continue
		} else if err != nil {
			return q.couldNotRun(n, pr, "push", err.Error(), v)
		}
		q.log.Info("landed", q.f(n, "sha", cand, "before", tip, "tree", tree))
		q.record(n, "landed", rec)
		deploy := q.afterLand(n, tip, cand, tree)
		q.say(n, fmt.Sprintf("Landed on `%s` as %s (trunk was %s).\n\nAfter-land hook: %s\n\n```\nVERIFY %s\n```", q.rc.Trunk, cand, tip, deploy, v.JSON))
		if err := q.in.Close(n); err != nil {
			q.log.Error("close_failed", q.f(n, "error", err.Error()))
		}
		return "landed"
	}
	return q.couldNotRun(n, pr, "trunk-moved", fmt.Sprintf("trunk moved %d times while verifying", q.cfg.MaxReloops), Verdict{})
}

// runVerify executes the verifier against the checked-out candidate and parses its contract:
// exit 0 pass, 1 fail, anything else could not run; the last stdout line is `VERIFY {json}`.
func (q *Queue) runVerify(base, logPath string) Verdict {
	_ = os.MkdirAll(filepath.Dir(logPath), 0o755)
	lf, err := os.Create(logPath)
	if err != nil {
		return Verdict{Exit: 2, Why: err.Error()}
	}
	defer lf.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(q.cfg.VerifyTimeoutSec)*time.Second)
	defer cancel()
	argv := append(append([]string{}, q.rc.VerifyCmd...), "--base", base)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir, cmd.Env = q.git.Dir, gitEnv()
	// SIGTERM first so the verifier can flush its logs; SIGKILL only if it has not exited by WaitDelay.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 30 * time.Second
	var stdout, all bytes.Buffer
	cmd.Stdout, cmd.Stderr = io.MultiWriter(&stdout, &all, lf), io.MultiWriter(&all, lf)
	err = cmd.Run()
	v := Verdict{Tail: tail(all.String(), 60)}
	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	if raw := strings.TrimPrefix(strings.TrimSpace(lines[len(lines)-1]), "VERIFY "); strings.HasPrefix(raw, "{") && json.Valid([]byte(raw)) {
		v.JSON = json.RawMessage(raw)
	}
	var ee *exec.ExitError
	switch {
	case ctx.Err() != nil:
		v.Exit, v.Why = 2, fmt.Sprintf("verifier timed out after %ds", q.cfg.VerifyTimeoutSec)
	case err == nil:
	case errors.As(err, &ee) && ee.ExitCode() == 1:
		v.Exit = 1
	default:
		v.Exit, v.Why = 2, "verifier: "+err.Error()
	}
	if v.Exit != 2 && v.JSON == nil {
		v.Exit, v.Why = 2, "verifier broke its contract: last stdout line is not VERIFY {json}"
	}
	return v
}

// reject is a terminal non-land: say why (with the VERIFY json and log tail), drop the request.
func (q *Queue) reject(n int, pr PR, outcome, why string, v Verdict) string {
	if q.shadow && q.key != "" {
		q.judged[q.key] = true
	}
	q.log.Error("rejected", q.f(n, "outcome", outcome, "why", why))
	q.record(n, outcome, map[string]any{"head": pr.Head, "why": why, "verify": v.JSON, "exit": v.Exit})
	if v.JSON != nil {
		why += "\n\n```\nVERIFY " + string(v.JSON) + "\n```"
	}
	if v.Tail != "" {
		why += "\n\nLog tail:\n\n```\n" + v.Tail + "\n```"
	}
	q.say(n, why)
	q.unlabel(n)
	return outcome
}

// couldNotRun keeps the request and backs off; at MaxCouldNotRun it gives up and rejects.
func (q *Queue) couldNotRun(n int, pr PR, step, why string, v Verdict) string {
	r := q.retry[n]
	if r == nil {
		r = &retryState{}
		q.retry[n] = r
	}
	r.attempts++
	q.log.Error("could_not_run", q.f(n, "step", step, "why", why, "attempt", r.attempts, "max", q.cfg.MaxCouldNotRun))
	if r.attempts < q.cfg.MaxCouldNotRun {
		r.next = q.now().Add(time.Duration(q.cfg.RetryBaseSec<<(r.attempts-1)) * time.Second)
		q.record(n, "could-not-run-retry", map[string]any{"step": step, "why": why, "attempt": r.attempts})
		return "could-not-run-retry"
	}
	delete(q.retry, n)
	q.log.Error("could_not_run_gave_up", q.f(n, "step", step, "why", clip(why, 200), "attempts", r.attempts))
	q.reject(n, pr, "could-not-run", fmt.Sprintf("vmq could not run verification %d times (last step `%s`: %s). Push the request again to retry.", r.attempts, step, clip(why, 400)), v)
	return "could-not-run-gave-up"
}

// afterLand runs the configured hook commands in order, loud on any failure. It runs after trunk
// has moved: a failing hook does not undo the land.
func (q *Queue) afterLand(n int, before, sha, tree string) string {
	if len(q.rc.AfterLand) == 0 {
		return "none configured"
	}
	sub := strings.NewReplacer("{repo}", q.rc.Name, "{sha}", sha, "{before}", before, "{tree}", tree,
		"{pr}", fmt.Sprint(n), "{trunk}", q.rc.Trunk)
	for _, tmpl := range q.rc.AfterLand {
		argv := make([]string, len(tmpl))
		for i, a := range tmpl {
			argv[i] = sub.Replace(a)
		}
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Dir, cmd.Env = q.git.Dir, gitEnv()
		out, err := cmd.CombinedOutput()
		if err != nil {
			q.log.Error("after_land_failed", q.f(n, "sha", sha, "argv", argv, "error", err.Error(), "output", clip(string(out), 800)))
			return "FAILED at `" + strings.Join(argv, " ") + "` (" + err.Error() + ")"
		}
		q.log.Info("after_land_step", q.f(n, "sha", sha, "argv", argv, "output", clip(string(out), 400)))
	}
	return "ran"
}

func (q *Queue) say(n int, body string) {
	if err := q.in.Comment(n, body); err != nil && !errors.Is(err, ErrReadOnly) {
		q.log.Error("comment_failed", q.f(n, "error", err.Error()))
	}
}

func (q *Queue) unlabel(n int) {
	var labels []string
	if q.land[n] || !q.first[n] {
		labels = append(labels, q.cfg.Label)
	}
	if q.first[n] {
		labels = append(labels, q.cfg.FirstLabel)
	}
	for _, label := range labels {
		if err := q.in.Unlabel(n, label); err != nil && !errors.Is(err, ErrReadOnly) {
			q.log.Error("unlabel_failed", q.f(n, "error", err.Error(), "label", label))
		}
	}
}

// record appends one verdict line to the JSONL verdict log: the audit record, and in shadow mode
// the thing you compare against what you would have done by hand.
func (q *Queue) record(n int, outcome string, f map[string]any) {
	f["ts"], f["repo"], f["request"], f["outcome"], f["shadow"] = q.now().UTC().Format(time.RFC3339), q.rc.Name, n, outcome, q.shadow
	b, _ := json.Marshal(f)
	_ = os.MkdirAll(filepath.Dir(q.cfg.VerdictLog), 0o755)
	fh, err := os.OpenFile(q.cfg.VerdictLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err == nil {
		_, err = fh.Write(append(b, '\n'))
		err = errors.Join(err, fh.Close())
	}
	if err != nil {
		q.log.Error("verdict_log_failed", q.f(n, "path", q.cfg.VerdictLog, "error", err.Error()))
	}
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return strings.Join(lines[max(0, len(lines)-n):], "\n")
}
