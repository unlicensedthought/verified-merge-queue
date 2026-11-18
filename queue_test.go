package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain doubles as the fake verifier and fake hook, so no test helper is a script:
// `<test binary> fake-verify <ctl-dir> --base <sha>` and `<test binary> fake-record <file> args...`.
// The verifier records the HEAD it ran against, which is how tests prove what was verified.
func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == "fake-verify" {
		mode, _ := os.ReadFile(filepath.Join(os.Args[2], "mode"))
		head, _ := exec.Command("git", "rev-parse", "HEAD").Output()
		_ = os.WriteFile(filepath.Join(os.Args[2], "base"), []byte(os.Args[4]), 0o644)
		_ = os.WriteFile(filepath.Join(os.Args[2], "head"), []byte(strings.TrimSpace(string(head))), 0o644)
		switch strings.TrimSpace(string(mode)) {
		case "fail":
			fmt.Println("step lint: 3 errors")
			fmt.Println(`VERIFY {"verdict":"fail","failed":"lint"}`)
			os.Exit(1)
		case "hang":
			sigs := make(chan os.Signal, 1)
			signal.Notify(sigs, syscall.SIGTERM)
			select {
			case <-sigs:
				_ = os.WriteFile(filepath.Join(os.Args[2], "term"), []byte("SIGTERM"), 0o644)
				os.Exit(2)
			case <-time.After(time.Minute):
				os.Exit(0)
			}
		case "cnr":
			fmt.Println(`VERIFY {"verdict":"could-not-run"}`)
			os.Exit(2)
		case "no-contract":
			fmt.Println("all good, trust me")
			os.Exit(0)
		default:
			fmt.Println(`VERIFY {"verdict":"pass","gates":3}`)
		}
		os.Exit(0)
	}
	if len(os.Args) > 2 && os.Args[1] == "fake-record" {
		f, _ := os.OpenFile(os.Args[2], os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		fmt.Fprintln(f, strings.Join(os.Args[3:], " "))
		f.Close()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type env struct {
	t      *testing.T
	root   string
	bare   string
	seed   Git // a work clone used to make commits
	q      *Queue
	ctl    string
	record string
	logs   *bytes.Buffer
}

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv("GIT_AUTHOR_NAME=Dev One", "GIT_AUTHOR_EMAIL=dev1@example.com",
		"GIT_COMMITTER_NAME=Dev One", "GIT_COMMITTER_EMAIL=dev1@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newEnv(t *testing.T, shadow bool) *env {
	root := t.TempDir()
	e := &env{t: t, root: root, bare: filepath.Join(root, "remote.git"), ctl: filepath.Join(root, "ctl"),
		record: filepath.Join(root, "after-land.txt"), logs: &bytes.Buffer{}}
	_ = os.MkdirAll(e.ctl, 0o755)
	draining.Store(false)
	gitT(t, root, "init", "--bare", "-b", "main", e.bare)
	seed := filepath.Join(root, "seed")
	gitT(t, root, "clone", e.bare, seed)
	e.seed = Git{Dir: seed}
	e.write("README.md", "base\n")
	gitT(t, seed, "add", ".")
	gitT(t, seed, "commit", "-m", "base")
	gitT(t, seed, "push", "origin", "HEAD:refs/heads/main")
	cfg := &Config{Repos: []RepoConf{{Name: "r", Remote: e.bare}}, WorkDir: filepath.Join(root, "work"),
		VerifyCmd: []string{os.Args[0], "fake-verify", e.ctl},
		AfterLand: [][]string{{os.Args[0], "fake-record", e.record, "landed", "{repo}", "{sha}", "{before}", "{tree}", "{pr}"}}}
	if err := cfg.Defaults(); err != nil {
		t.Fatal(err)
	}
	log := &Logger{w: e.logs}
	e.q = NewQueue(cfg, cfg.Repos[0], NewRefIntake(cfg, cfg.Repos[0], shadow, log), log, shadow)
	return e
}

func (e *env) write(path, body string) {
	_ = os.MkdirAll(filepath.Dir(filepath.Join(e.seed.Dir, path)), 0o755)
	if err := os.WriteFile(filepath.Join(e.seed.Dir, path), []byte(body), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) mode(m string) { _ = os.WriteFile(filepath.Join(e.ctl, "mode"), []byte(m), 0o644) }

// request publishes a one-commit request off trunk as refs/land/<n> (and refs/land-first/<n> when
// first). date is the commit's committer date: the queue orders by it.
func (e *env) request(n int, path, body string, first bool, date time.Time) string {
	gitT(e.t, e.seed.Dir, "fetch", "origin", "main")
	gitT(e.t, e.seed.Dir, "checkout", "-q", "--detach", "origin/main")
	e.write(path, body)
	gitT(e.t, e.seed.Dir, "add", ".")
	cmd := exec.Command("git", "commit", "-m", fmt.Sprintf("feat: request %d", n), "-m", "Body of "+path+"\n\nCo-authored-by: Pair Two <pair2@example.com>")
	cmd.Dir = e.seed.Dir
	cmd.Env = gitEnv("GIT_AUTHOR_NAME=Dev One", "GIT_AUTHOR_EMAIL=dev1@example.com", "GIT_COMMITTER_NAME=Dev One",
		"GIT_COMMITTER_EMAIL=dev1@example.com", "GIT_COMMITTER_DATE="+date.UTC().Format(time.RFC3339))
	if out, err := cmd.CombinedOutput(); err != nil {
		e.t.Fatalf("commit: %v %s", err, out)
	}
	head := gitT(e.t, e.seed.Dir, "rev-parse", "HEAD")
	gitT(e.t, e.seed.Dir, "push", "-f", "origin", fmt.Sprintf("HEAD:refs/land/%d", n))
	if first {
		gitT(e.t, e.seed.Dir, "push", "-f", "origin", fmt.Sprintf("HEAD:refs/land-first/%d", n))
	}
	return head
}

// req is request with a label time that orders by n.
func (e *env) req(n int, path, body string) string {
	return e.request(n, path, body, false, time.Now().Add(time.Duration(n)*time.Second))
}

// advanceTrunk lands an unrelated commit on the remote trunk directly.
func (e *env) advanceTrunk(path string) string {
	gitT(e.t, e.seed.Dir, "fetch", "origin", "main")
	gitT(e.t, e.seed.Dir, "checkout", "-q", "--detach", "origin/main")
	e.write(path, "other\n")
	gitT(e.t, e.seed.Dir, "add", ".")
	gitT(e.t, e.seed.Dir, "commit", "-m", "other "+path)
	gitT(e.t, e.seed.Dir, "push", "origin", "HEAD:refs/heads/main")
	return gitT(e.t, e.seed.Dir, "rev-parse", "HEAD")
}

func (e *env) trunk() string {
	return strings.Fields(gitT(e.t, e.root, "--git-dir", e.bare, "rev-parse", "refs/heads/main"))[0]
}

func (e *env) hasRef(ref string) bool {
	return gitT(e.t, e.root, "--git-dir", e.bare, "for-each-ref", ref) != ""
}

func (e *env) result(n int) string {
	b, _ := os.ReadFile(filepath.Join(e.q.cfg.WorkDir, "results", "r", fmt.Sprintf("%d.md", n)))
	return string(b)
}

func (e *env) verified(file string) string {
	b, _ := os.ReadFile(filepath.Join(e.ctl, file))
	return string(b)
}

func verdicts(t *testing.T, e *env) []map[string]any {
	raw, err := os.ReadFile(e.q.cfg.VerdictLog)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("bad JSONL %q", l)
		}
		out = append(out, m)
	}
	return out
}

func startedOrder(t *testing.T, logs string) []int {
	var got []int
	for _, l := range strings.Split(strings.TrimSpace(logs), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil && m["event"] == "verify_start" {
			got = append(got, int(m["request"].(float64)))
		}
	}
	return got
}

// The core safety property: the commit trunk advances to is the commit that was verified.
func TestLandedCommitIsExactlyTheVerifiedCandidate(t *testing.T) {
	e := newEnv(t, false)
	before := e.trunk()
	e.req(7, "a.txt", "a\n")
	if got := e.q.Tick()[7]; got != "landed" {
		t.Fatalf("outcome %q, logs:\n%s", got, e.logs)
	}
	tip := e.trunk()
	if verified := e.verified("head"); verified != tip {
		t.Fatalf("trunk is %s but the verifier ran on %s", tip, verified)
	}
	if parent := gitT(t, e.root, "--git-dir", e.bare, "rev-parse", tip+"^"); parent != before {
		t.Fatalf("not a fast-forward of trunk: parent %s, want %s", parent, before)
	}
	if e.verified("base") != before {
		t.Errorf("verify --base %q, want trunk %s", e.verified("base"), before)
	}
	msg := gitT(t, e.root, "--git-dir", e.bare, "log", "-1", "--format=%B", tip)
	for _, want := range []string{"feat: request 7 (#7)", "Body of a.txt", "Co-authored-by: Dev One <dev1@example.com>", "Co-authored-by: Pair Two <pair2@example.com>"} {
		if !strings.Contains(msg, want) {
			t.Errorf("squash message lacks %q:\n%s", want, msg)
		}
	}
	if !strings.HasPrefix(msg, "feat: request 7 (#7)\n") {
		t.Errorf("subject is not `<title> (#N)`: %q", msg)
	}
	if a := gitT(t, e.root, "--git-dir", e.bare, "log", "-1", "--format=%an <%ae>|%cn <%ce>", tip); a != "merge queue <vmq@example.invalid>|merge queue <vmq@example.invalid>" {
		t.Errorf("identity %q", a)
	}
	if e.hasRef("refs/land/7") {
		t.Error("request ref survived a landing")
	}
	tree := gitT(t, e.root, "--git-dir", e.bare, "rev-parse", tip+"^{tree}")
	rec, _ := os.ReadFile(e.record)
	if want := fmt.Sprintf("landed r %s %s %s 7", tip, before, tree); strings.TrimSpace(string(rec)) != want {
		t.Errorf("after_land argv %q, want %q", rec, want)
	}
	if !strings.Contains(e.result(7), `VERIFY {"verdict":"pass","gates":3}`) {
		t.Errorf("landing note lacks the VERIFY line: %s", e.result(7))
	}
}

// Negative proof: a failing verify never moves trunk, and says which step failed.
func TestFailDoesNotMoveTrunkAndNamesTheStep(t *testing.T) {
	e := newEnv(t, false)
	before := e.trunk()
	e.req(3, "b.txt", "b\n")
	e.mode("fail")
	if got := e.q.Tick()[3]; got != "fail" {
		t.Fatalf("outcome %q", got)
	}
	if e.trunk() != before {
		t.Fatal("a failing request moved trunk")
	}
	if e.hasRef("refs/land/3") {
		t.Error("rejected request kept its ref")
	}
	if r := e.result(3); !strings.Contains(r, `"failed":"lint"`) || !strings.Contains(r, "step lint: 3 errors") {
		t.Errorf("verdict lacks the failing step and log tail: %s", r)
	}
	if _, err := os.Stat(e.record); err == nil {
		t.Error("a failing request fired the after-land hook")
	}
}

// A verifier that does not print `VERIFY {json}` cannot vouch for anything, even with exit 0.
func TestVerifierThatBreaksItsContractIsNotAPass(t *testing.T) {
	e := newEnv(t, false)
	before := e.trunk()
	e.req(5, "c.txt", "c\n")
	e.mode("no-contract")
	if got := e.q.Tick()[5]; got != "could-not-run-retry" {
		t.Fatalf("outcome %q", got)
	}
	if e.trunk() != before {
		t.Fatal("an unproven request moved trunk")
	}
	if !strings.Contains(e.logs.String(), "verifier broke its contract") {
		t.Errorf("contract break not logged: %s", e.logs)
	}
}

func TestConflictRejectsWithoutMovingTrunk(t *testing.T) {
	e := newEnv(t, false)
	e.req(4, "README.md", "request side\n")
	e.advanceTrunk("README.md")
	before := e.trunk()
	if got := e.q.Tick()[4]; got != "conflict" {
		t.Fatalf("outcome %q logs %s", got, e.logs)
	}
	if e.trunk() != before {
		t.Fatal("a conflicting request moved trunk")
	}
	if !strings.Contains(e.result(4), "Conflicts with trunk, rebase") {
		t.Errorf("conflict note: %s", e.result(4))
	}
}

func TestRequestThatChangesNothingIsRejected(t *testing.T) {
	e := newEnv(t, false)
	before := e.trunk()
	gitT(t, e.seed.Dir, "fetch", "origin", "main")
	gitT(t, e.seed.Dir, "checkout", "-q", "--detach", "origin/main")
	gitT(t, e.seed.Dir, "commit", "--allow-empty", "-m", "nothing")
	gitT(t, e.seed.Dir, "push", "-f", "origin", "HEAD:refs/land/6")
	if got := e.q.Tick()[6]; got != "empty" {
		t.Fatalf("outcome %q logs %s", got, e.logs)
	}
	if e.trunk() != before {
		t.Fatal("an empty request moved trunk")
	}
}

// Trunk moves while verify runs: the passing result is for a stale base, so it is thrown away and
// the request is rebuilt and verified again on the new trunk.
func TestTrunkMovedMidVerifyReverifies(t *testing.T) {
	e := newEnv(t, false)
	e.req(5, "c.txt", "c\n")
	inner, calls, moved := e.q.Verify, 0, ""
	e.q.Verify = func(base, log string) Verdict {
		calls++
		v := inner(base, log)
		if calls == 1 {
			moved = e.advanceTrunk("other.txt")
		}
		return v
	}
	if got := e.q.Tick()[5]; got != "landed" {
		t.Fatalf("outcome %q logs %s", got, e.logs)
	}
	if calls != 2 {
		t.Fatalf("verify ran %d times, want 2", calls)
	}
	tip := e.trunk()
	if p := gitT(t, e.root, "--git-dir", e.bare, "rev-parse", tip+"^"); p != moved {
		t.Fatalf("landed on %s, want on the moved trunk %s", p, moved)
	}
	if e.verified("head") != tip {
		t.Errorf("trunk %s is not what the second verify ran on (%s)", tip, e.verified("head"))
	}
	if !strings.Contains(e.logs.String(), `"event":"trunk_moved"`) {
		t.Errorf("no trunk_moved log line")
	}
}

func TestCouldNotRunRetriesWithBackoffThenGivesUp(t *testing.T) {
	e := newEnv(t, false)
	before := e.trunk()
	e.req(6, "d.txt", "d\n")
	e.mode("cnr")
	clock := time.Now()
	e.q.now = func() time.Time { return clock }
	for i := 1; i <= 2; i++ {
		if got := e.q.Tick()[6]; got != "could-not-run-retry" {
			t.Fatalf("attempt %d outcome %q", i, got)
		}
		if !e.hasRef("refs/land/6") || e.result(6) != "" {
			t.Fatalf("could-not-run must keep the request and say nothing before giving up")
		}
		if got, ok := e.q.Tick()[6]; ok {
			t.Fatalf("retried inside backoff: %q", got)
		}
		clock = clock.Add(time.Duration(e.q.cfg.RetryBaseSec<<(i-1))*time.Second + time.Second)
	}
	if got := e.q.Tick()[6]; got != "could-not-run-gave-up" {
		t.Fatalf("third outcome %q", got)
	}
	if e.trunk() != before {
		t.Fatal("an unverifiable request moved trunk")
	}
	if !strings.Contains(e.result(6), "could not run verification 3 times") {
		t.Errorf("give-up note: %s", e.result(6))
	}
}

func TestVerifyTimeoutSendsSIGTERMFirstAndDoesNotLand(t *testing.T) {
	e := newEnv(t, false)
	before := e.trunk()
	e.req(4, "c.txt", "c\n")
	e.mode("hang")
	e.q.cfg.VerifyTimeoutSec = 1
	_ = e.q.Tick()
	if b, err := os.ReadFile(filepath.Join(e.ctl, "term")); err != nil || string(b) != "SIGTERM" {
		t.Fatalf("timed-out verify did not get SIGTERM first: %q %v", b, err)
	}
	if e.trunk() != before {
		t.Fatal("a timed-out verify moved trunk")
	}
}

// Shadow mode runs the whole pipeline and changes nothing outside its own work dir.
func TestShadowRecordsVerdictsAndNeverPushesOrWrites(t *testing.T) {
	e := newEnv(t, true)
	before := e.trunk()
	e.req(9, "e.txt", "e\n")
	if got := e.q.Tick()[9]; got != "shadow-pass" {
		t.Fatalf("outcome %q logs %s", got, e.logs)
	}
	if got := e.q.Tick()[9]; got != "shadow-already-judged" {
		t.Fatalf("re-judged the same candidate: %q", got)
	}
	e.req(10, "README.md", "x\n")
	e.mode("fail")
	e.q.Tick()
	if e.trunk() != before {
		t.Fatalf("SHADOW PUSHED: trunk %s, want %s", e.trunk(), before)
	}
	if !e.hasRef("refs/land/9") || !e.hasRef("refs/land/10") {
		t.Fatal("shadow deleted a request ref")
	}
	if e.result(9) != "" || e.result(10) != "" {
		t.Fatal("shadow wrote a verdict back to the intake")
	}
	if _, err := os.Stat(e.record); err == nil {
		t.Fatal("shadow fired the after-land hook")
	}
	var outcomes []string
	for _, m := range verdicts(t, e) {
		outcomes = append(outcomes, fmt.Sprint(m["request"], ":", m["outcome"], ":", m["shadow"]))
	}
	if got := strings.Join(outcomes, ","); got != "9:pass:true,10:fail:true" {
		t.Fatalf("verdicts %s", got)
	}
}

func TestPushFFRefusesNonFastForwardAndShadow(t *testing.T) {
	e := newEnv(t, false)
	trunkTip := e.trunk()
	gitT(t, e.seed.Dir, "checkout", "-q", "--orphan", "rogue")
	e.write("rogue.txt", "r\n")
	gitT(t, e.seed.Dir, "add", ".")
	gitT(t, e.seed.Dir, "commit", "-m", "rogue")
	rogue := gitT(t, e.seed.Dir, "rev-parse", "HEAD")
	if err := e.seed.PushFF(false, "main", trunkTip, rogue); !errors.Is(err, ErrNotFastForward) {
		t.Fatalf("PushFF(non-ff) = %v, want ErrNotFastForward", err)
	}
	if e.trunk() != trunkTip {
		t.Fatal("non-ff candidate reached trunk")
	}
	for _, a := range pushArgs("main", rogue) {
		if strings.HasPrefix(a, "+") || strings.HasPrefix(a, "-f") || strings.Contains(a, "force") {
			t.Fatalf("push argv carries a force: %v", pushArgs("main", rogue))
		}
	}
	if err := e.seed.PushFF(true, "main", trunkTip, rogue); !errors.Is(err, ErrShadowPush) {
		t.Fatalf("shadow PushFF = %v, want ErrShadowPush", err)
	}
}

// A candidate built on an old tip is refused when trunk has moved, even though it is a descendant
// of that old tip.
func TestPushFFRefusesAStaleTip(t *testing.T) {
	e := newEnv(t, false)
	old := e.trunk()
	gitT(t, e.seed.Dir, "fetch", "origin", "main")
	gitT(t, e.seed.Dir, "checkout", "-q", "--detach", "origin/main")
	e.write("cand.txt", "c\n")
	gitT(t, e.seed.Dir, "add", ".")
	gitT(t, e.seed.Dir, "commit", "-m", "cand")
	cand := gitT(t, e.seed.Dir, "rev-parse", "HEAD")
	e.advanceTrunk("other.txt")
	moved := e.trunk()
	if err := e.seed.PushFF(false, "main", old, cand); !errors.Is(err, ErrTrunkMoved) {
		t.Fatalf("PushFF on a stale tip = %v, want ErrTrunkMoved", err)
	}
	if e.trunk() != moved {
		t.Fatal("stale candidate reached trunk")
	}
}

func TestFIFOByLabelTime(t *testing.T) {
	e := newEnv(t, true)
	now := time.Now()
	e.request(21, "x.txt", "x\n", false, now.Add(-time.Hour))
	e.request(20, "y.txt", "y\n", false, now)
	e.q.Tick()
	if got := startedOrder(t, e.logs.String()); !reflect.DeepEqual(got, []int{21, 20}) {
		t.Fatalf("verify order %v, want [21 20]", got)
	}
}

func TestFirstLabelSortsAheadOfOlderRequests(t *testing.T) {
	e := newEnv(t, false)
	now := time.Now()
	e.request(1, "one.txt", "x\n", false, now.Add(-3*time.Hour))
	e.request(2, "two.txt", "x\n", false, now.Add(-2*time.Hour))
	e.request(3, "three.txt", "x\n", true, now) // newest, but first
	out := e.q.Tick()
	for _, n := range []int{1, 2, 3} {
		if out[n] != "landed" {
			t.Fatalf("request %d: %q\n%s", n, out[n], e.logs)
		}
	}
	if got := startedOrder(t, e.logs.String()); !reflect.DeepEqual(got, []int{3, 1, 2}) {
		t.Fatalf("verify order %v, want [3 1 2]", got)
	}
	if e.hasRef("refs/land-first/3") {
		t.Error("the first ref was left on a landed request")
	}
}

// The queue is polled again before each request, so a first-label request that appears while
// another is verifying goes next.
func TestFirstLabelAddedDuringATickGoesNext(t *testing.T) {
	e := newEnv(t, false)
	now := time.Now()
	e.request(81, "a.txt", "a\n", false, now.Add(-3*time.Hour))
	e.request(82, "b.txt", "b\n", false, now.Add(-2*time.Hour))
	head83 := e.request(83, "c.txt", "c\n", false, now.Add(-time.Hour))
	inner, once := e.q.Verify, false
	e.q.Verify = func(base, log string) Verdict {
		if !once {
			once = true
			gitT(t, e.root, "--git-dir", e.bare, "update-ref", "refs/land-first/83", head83)
		}
		return inner(base, log)
	}
	e.q.Tick()
	if got := startedOrder(t, e.logs.String()); !reflect.DeepEqual(got, []int{81, 83, 82}) {
		t.Fatalf("verify order %v, want [81 83 82]", got)
	}
}

// Landing is serial: the second request is rebuilt on the first one's result and verified on it.
func TestSecondRequestIsVerifiedOnTopOfTheFirst(t *testing.T) {
	e := newEnv(t, false)
	e.req(1, "a.txt", "a\n")
	e.req(2, "b.txt", "b\n")
	out := e.q.Tick()
	if out[1] != "landed" || out[2] != "landed" {
		t.Fatalf("outcomes %v\n%s", out, e.logs)
	}
	subjects := gitT(t, e.root, "--git-dir", e.bare, "log", "-2", "--format=%s", "refs/heads/main")
	if subjects != "feat: request 2 (#2)\nfeat: request 1 (#1)" {
		t.Errorf("trunk history %q", subjects)
	}
	first := gitT(t, e.root, "--git-dir", e.bare, "rev-parse", "refs/heads/main^")
	if e.verified("base") != first {
		t.Errorf("second verify --base %s, want the first landing %s", e.verified("base"), first)
	}
}

func TestEnsureRepointsAnExistingCloneOriginAndDisablesHooks(t *testing.T) {
	e := newEnv(t, false)
	g := Git{Dir: filepath.Join(t.TempDir(), "c")}
	if err := g.Ensure("https://example.invalid/old/r"); err != nil {
		t.Fatal(err)
	}
	if err := g.Ensure(e.bare); err != nil {
		t.Fatal(err)
	}
	if u, _ := g.git("remote", "get-url", "origin"); u != e.bare {
		t.Fatalf("origin %q after the remote moved, want %q", u, e.bare)
	}
	if h, _ := g.git("config", "core.hooksPath"); h != "/dev/null" {
		t.Fatalf("core.hooksPath %q: candidate-tree hooks would run", h)
	}
}

// A verify step that re-arms a repo hook (a package install can) must not block the queue's own push.
func TestPushIgnoresRepoPrePushHook(t *testing.T) {
	e := newEnv(t, false)
	before := e.trunk()
	e.req(12, "x.txt", "x\n")
	hooks := filepath.Join(e.root, "refusing-hooks")
	_ = os.MkdirAll(hooks, 0o755)
	if err := os.WriteFile(filepath.Join(hooks, "pre-push"), []byte("#!/bin/sh\necho 'Direct push forbidden' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	inner := e.q.Verify
	e.q.Verify = func(base, log string) Verdict {
		gitT(t, e.q.git.Dir, "config", "core.hooksPath", hooks)
		return inner(base, log)
	}
	if got := e.q.Tick()[12]; got != "landed" || e.trunk() == before {
		t.Fatalf("a repo pre-push hook blocked the queue: outcome %q logs %s", got, e.logs)
	}
}

func TestPerRepoVerifyAndAfterLandOverrideGlobal(t *testing.T) {
	cfg := &Config{
		Repos: []RepoConf{
			{Name: "own", Remote: "x", VerifyCmd: []string{"own-verify"}, AfterLand: [][]string{{"own-hook"}}},
			{Name: "inherits", Remote: "x"},
		},
		WorkDir: t.TempDir(), VerifyCmd: []string{"global-verify"}, AfterLand: [][]string{{"global-hook"}},
	}
	if err := cfg.Defaults(); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(cfg.Repos[0].VerifyCmd, cfg.Repos[0].AfterLand) != "[own-verify] [[own-hook]]" {
		t.Errorf("override lost: %+v", cfg.Repos[0])
	}
	if fmt.Sprint(cfg.Repos[1].VerifyCmd, cfg.Repos[1].AfterLand) != "[global-verify] [[global-hook]]" {
		t.Errorf("inherit lost: %+v", cfg.Repos[1])
	}
}

func TestConfigRefusesWhatCannotRun(t *testing.T) {
	for name, c := range map[string]Config{
		"no repos":     {},
		"no remote":    {Repos: []RepoConf{{Name: "r"}}},
		"no name":      {Repos: []RepoConf{{Remote: "x"}}},
		"path as name": {Repos: []RepoConf{{Name: "../r", Remote: "x"}}},
	} {
		if err := c.Defaults(); err == nil {
			t.Errorf("%s: config accepted", name)
		}
	}
}

// While draining nothing new starts, and the request is untouched.
func TestDrainingQueueStartsNothingAndKeepsTheRequest(t *testing.T) {
	e := newEnv(t, false)
	e.req(31, "d.txt", "d\n")
	draining.Store(true)
	defer draining.Store(false)
	ran := false
	e.q.Verify = func(string, string) Verdict { ran = true; return Verdict{Exit: 0, JSON: json.RawMessage(`{}`)} }
	if out := e.q.Tick(); len(out) != 0 {
		t.Fatalf("draining queue ran %v", out)
	}
	if got := e.q.Land(31); got != "drained" {
		t.Fatalf("Land while draining = %q", got)
	}
	if ran || !e.hasRef("refs/land/31") {
		t.Fatalf("ran %v, ref kept %v", ran, e.hasRef("refs/land/31"))
	}
}

func TestSquashMessageDedupesCoAuthors(t *testing.T) {
	log := "Dev One <dev1@example.com>\x1fa\n\nCo-authored-by: Pair Two <pair2@example.com>\x1e" +
		"Pair Two <PAIR2@example.com>\x1fb\x1e"
	msg := squashMessage(PR{Number: 5, Title: " t ", Body: "body"}, log, "merge queue", "vmq@example.invalid")
	if n := len(regexp.MustCompile(`(?i)pair2@example.com`).FindAllString(msg, -1)); n != 1 {
		t.Fatalf("co-author listed %d times:\n%s", n, msg)
	}
	if !strings.HasPrefix(msg, "t (#5)\n\nbody\n\nCo-authored-by: Dev One") {
		t.Fatalf("message:\n%s", msg)
	}
}
