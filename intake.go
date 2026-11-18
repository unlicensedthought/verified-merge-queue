package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrReadOnly is every intake write in shadow mode.
var ErrReadOnly = errors.New("intake write refused: shadow mode is read-only")

// PR is one land request.
type PR struct {
	Number int
	Title  string
	Body   string
	State  string // "open" while its request ref exists
	Head   string // sha of the request head
}

// Intake is where land requests come from and where verdicts go. The queue knows nothing else
// about the git host: to serve another host (pull requests with labels, say), implement this.
type Intake interface {
	Labelled(label string) ([]int, error)
	LabelledAt(n int, label string) (time.Time, error)
	Pull(n int) (PR, error)
	Comment(n int, body string) error
	Close(n int) error
	Unlabel(n int, label string) error
}

// RefIntake takes land requests from refs on the repo's own remote. A request is a push of its head:
//
//	git push <remote> HEAD:refs/land/<n>          # n: a positive integer, unique per request
//	git push <remote> HEAD:refs/land-first/<n>    # additionally, to sort ahead of the queue
//
// The request's title and body are the head commit's subject and body, its time is that commit's
// committer date. A landed or rejected request has its refs deleted; the verdict text goes to
// <workdir>/results/<name>/<n>.md and the log.
type RefIntake struct {
	git           Git
	remote, trunk string
	label, first  string
	results       string
	readOnly      bool
	log           *Logger
	once          sync.Once
	ensureErr     error
}

// NewRefIntake builds the intake for one repo; its clone is the queue's own (<workdir>/<name>).
func NewRefIntake(cfg *Config, rc RepoConf, shadow bool, log *Logger) *RefIntake {
	return &RefIntake{git: Git{Dir: filepath.Join(cfg.WorkDir, rc.Name)}, remote: rc.Remote, trunk: rc.Trunk,
		label: cfg.Label, first: cfg.FirstLabel, results: filepath.Join(cfg.WorkDir, "results", rc.Name),
		readOnly: shadow, log: log}
}

func (r *RefIntake) ready() error {
	r.once.Do(func() { r.ensureErr = r.git.Ensure(r.remote) })
	return r.ensureErr
}

func (r *RefIntake) prefix(label string) (string, error) {
	switch {
	case label == r.label:
		return "refs/land/", nil
	case r.first != "" && label == r.first:
		return "refs/land-first/", nil
	}
	return "", fmt.Errorf("ref intake has no label %q", label)
}

// tips maps request number to sha for every ref under prefix; a ref whose last segment is not a
// positive integer is ignored.
func (r *RefIntake) tips(prefix string) (map[int]string, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	out, err := r.git.git("ls-remote", "origin", prefix+"*")
	if err != nil {
		return nil, err
	}
	tips := map[int]string{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) != 2 || !shaRE.MatchString(f[0]) || !strings.HasPrefix(f[1], prefix) {
			continue
		}
		if n, err := strconv.Atoi(strings.TrimPrefix(f[1], prefix)); err == nil && n > 0 {
			tips[n] = f[0]
		}
	}
	return tips, nil
}

// Labelled lists the request numbers present under label's ref namespace.
func (r *RefIntake) Labelled(label string) ([]int, error) {
	p, err := r.prefix(label)
	if err != nil {
		return nil, err
	}
	tips, err := r.tips(p)
	if err != nil {
		return nil, err
	}
	var out []int
	for n := range tips {
		out = append(out, n)
	}
	return out, nil
}

func (r *RefIntake) fetchReq(n int, prefix string) (string, error) {
	dst := fmt.Sprintf("refs/vmq/intake-%d", n)
	_, err := r.git.git("fetch", "--no-tags", "--force", "origin", fmt.Sprintf("%s%d:%s", prefix, n, dst),
		"refs/heads/"+r.trunk+":refs/remotes/origin/"+r.trunk)
	return dst, err
}

// LabelledAt is the committer date of the request head.
func (r *RefIntake) LabelledAt(n int, label string) (time.Time, error) {
	p, err := r.prefix(label)
	if err != nil {
		return time.Time{}, err
	}
	if err := r.ready(); err != nil {
		return time.Time{}, err
	}
	ref, err := r.fetchReq(n, p)
	if err != nil {
		return time.Time{}, err
	}
	s, err := r.git.HeadTime(ref)
	if err != nil {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339, s)
}

// Pull reads request n: open while refs/land/<n> exists, closed once it is gone.
func (r *RefIntake) Pull(n int) (PR, error) {
	tips, err := r.tips("refs/land/")
	if err != nil {
		return PR{}, err
	}
	pr := PR{Number: n, State: "closed"}
	sha, ok := tips[n]
	if !ok {
		return pr, nil
	}
	ref, err := r.fetchReq(n, "refs/land/")
	if err != nil {
		return PR{}, err
	}
	title, err := r.git.git("log", "-1", "--format=%s", ref)
	if err != nil {
		return PR{}, err
	}
	body, err := r.git.git("log", "-1", "--format=%b", ref)
	if err != nil {
		return PR{}, err
	}
	pr.State, pr.Title, pr.Body, pr.Head = "open", title, body, sha
	return pr, nil
}

// Comment records the verdict text for request n; the line is logged too, so it is never silent.
func (r *RefIntake) Comment(n int, body string) error {
	if r.readOnly {
		return ErrReadOnly
	}
	if err := os.MkdirAll(r.results, 0o755); err != nil {
		return err
	}
	path := filepath.Join(r.results, fmt.Sprintf("%d.md", n))
	fh, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, werr := fmt.Fprintf(fh, "%s\n\n%s\n\n", time.Now().UTC().Format(time.RFC3339), body)
	if cerr := fh.Close(); werr == nil {
		werr = cerr
	}
	r.log.Info("verdict_written", map[string]any{"request": n, "file": path})
	return werr
}

// Close ends request n: both of its refs are removed.
func (r *RefIntake) Close(n int) error {
	if r.readOnly {
		return ErrReadOnly
	}
	var errs []string
	for _, p := range []string{"refs/land/", "refs/land-first/"} {
		if err := r.drop(p, n); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// Unlabel removes label's ref for request n.
func (r *RefIntake) Unlabel(n int, label string) error {
	if r.readOnly {
		return ErrReadOnly
	}
	p, err := r.prefix(label)
	if err != nil {
		return err
	}
	return r.drop(p, n)
}

// drop deletes prefix<n> on the remote; a ref that is already gone is not an error.
func (r *RefIntake) drop(prefix string, n int) error {
	tips, err := r.tips(prefix)
	if err != nil {
		return err
	}
	if _, ok := tips[n]; !ok {
		return nil
	}
	_, err = r.git.git("-c", "core.hooksPath=/dev/null", "push", "--no-verify", "origin", fmt.Sprintf(":%s%d", prefix, n))
	return err
}
