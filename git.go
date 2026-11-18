package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

var (
	// ErrNotFastForward: the candidate does not descend from the trunk tip it was built on.
	ErrNotFastForward = errors.New("refusing non-fast-forward push: candidate does not descend from trunk")
	// ErrTrunkMoved: trunk is no longer the tip the candidate was verified against.
	ErrTrunkMoved = errors.New("trunk moved since the candidate was built")
	// ErrShadowPush: shadow mode never pushes.
	ErrShadowPush = errors.New("refusing push: shadow mode")
	// ErrConflict: the request does not rebase cleanly onto trunk.
	ErrConflict = errors.New("conflicts with trunk")
	// ErrEmpty: the rebased request changes nothing against trunk.
	ErrEmpty = errors.New("no changes against trunk after rebase")

	coAuthorRE = regexp.MustCompile(`(?im)^co-authored-by:\s*(.+?)\s*$`)
	shaRE      = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// gitEnv drops GIT_DIR/GIT_WORK_TREE/GIT_INDEX_FILE: git exports them to hook subprocesses, and
// a stale value would aim every git child at another repository.
func gitEnv(extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_DIR=") && !strings.HasPrefix(kv, "GIT_WORK_TREE=") && !strings.HasPrefix(kv, "GIT_INDEX_FILE=") {
			env = append(env, kv)
		}
	}
	return append(env, extra...)
}

// Git runs git in one persistent clone.
type Git struct {
	Dir string
}

func (g Git) run(stdin string, extraEnv []string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir, cmd.Env, cmd.Stdin = g.Dir, gitEnv(extraEnv...), strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, clip(errb.String()+out.String(), 600))
	}
	return strings.TrimSpace(out.String()), nil
}

func (g Git) git(args ...string) (string, error) { return g.run("", nil, args...) }

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// Ensure creates the clone in Dir when absent, points origin at remote (an existing clone follows
// the configured remote when it changes), turns repo hooks off and clears any half-done rebase.
func (g Git) Ensure(remote string) error {
	if _, err := os.Stat(g.Dir + "/.git"); os.IsNotExist(err) {
		if err := os.MkdirAll(g.Dir, 0o755); err != nil {
			return err
		}
		if _, err := g.git("init", "-q", g.Dir); err != nil {
			return err
		}
		if _, err := g.git("remote", "add", "origin", remote); err != nil {
			return err
		}
	}
	// core.hooksPath=/dev/null: the checkout holds candidate trees, so no repo hook may run here.
	for _, kv := range [][]string{{"core.hooksPath", "/dev/null"}, {"remote.origin.url", remote}} {
		if _, err := g.git("config", "--replace-all", kv[0], kv[1]); err != nil {
			return err
		}
	}
	_, _ = g.git("rebase", "--abort")
	return nil
}

// Fetch updates the trunk ref and refs/vmq/req-N (the request head), returning their shas.
func (g Git) Fetch(trunk string, n int) (tip, head string, err error) {
	if _, err = g.git("fetch", "--no-tags", "--force", "origin", "refs/heads/"+trunk+":refs/remotes/origin/"+trunk,
		fmt.Sprintf("refs/land/%d:refs/vmq/req-%d", n, n)); err == nil {
		if tip, err = g.git("rev-parse", "refs/remotes/origin/"+trunk); err == nil {
			head, err = g.git("rev-parse", fmt.Sprintf("refs/vmq/req-%d", n))
		}
	}
	return tip, head, err
}

// RemoteTip reads trunk on the remote itself.
func (g Git) RemoteTip(trunk string) (string, error) {
	out, err := g.git("ls-remote", "origin", "refs/heads/"+trunk)
	if f := strings.Fields(out); err == nil && (len(f) == 0 || !shaRE.MatchString(f[0])) {
		err = fmt.Errorf("ls-remote origin refs/heads/%s answered %q", trunk, out)
	} else if err == nil {
		return f[0], nil
	}
	return "", err
}

// Rebase checks out head detached and rebases it onto tip. A conflict aborts and is ErrConflict.
func (g Git) Rebase(head, tip string) error {
	if _, err := g.git("checkout", "--force", "--detach", head); err != nil {
		return err
	}
	if _, err := g.git("clean", "-ffd"); err != nil {
		return err
	}
	if _, err := g.run("", []string{"GIT_COMMITTER_NAME=vmq", "GIT_COMMITTER_EMAIL=vmq@localhost"}, "rebase", tip); err != nil {
		_, _ = g.git("rebase", "--abort")
		return fmt.Errorf("%w: %v", ErrConflict, err)
	}
	return nil
}

// Squash makes one commit, parent tip, with the rebased HEAD's tree: `<title> (#N)`, the request
// body, then every co-author of the request's commits. This commit is the candidate: the exact
// object that gets verified and, if verification passes, pushed.
func (g Git) Squash(tip string, pr PR, name, email string) (cand, tree string, err error) {
	if tree, err = g.git("rev-parse", "HEAD^{tree}"); err != nil {
		return "", "", err
	}
	if base, err := g.git("rev-parse", tip+"^{tree}"); err != nil {
		return "", "", err
	} else if base == tree {
		return "", "", ErrEmpty
	}
	log, err := g.git("log", "--format=%an <%ae>%x1f%B%x1e", tip+"..HEAD")
	if err != nil {
		return "", "", err
	}
	id := []string{"GIT_AUTHOR_NAME=" + name, "GIT_AUTHOR_EMAIL=" + email, "GIT_COMMITTER_NAME=" + name, "GIT_COMMITTER_EMAIL=" + email}
	if cand, err = g.run(squashMessage(pr, log, name, email), id, "commit-tree", "--no-gpg-sign", tree, "-p", tip, "-F", "-"); err == nil && !shaRE.MatchString(cand) {
		err = fmt.Errorf("git commit-tree returned %q", cand)
	}
	return cand, tree, err
}

// squashMessage builds the trunk commit message; log is `author<US>body<RS>` per commit.
func squashMessage(pr PR, log, name, email string) string {
	msg := fmt.Sprintf("%s (#%d)", strings.TrimSpace(pr.Title), pr.Number)
	body := strings.TrimSpace(pr.Body)
	if body != "" {
		msg += "\n\n" + body
	}
	seen := map[string]bool{strings.ToLower(name + " <" + email + ">"): true}
	for _, m := range coAuthorRE.FindAllStringSubmatch(body, -1) {
		seen[strings.ToLower(m[1])] = true
	}
	var trailers []string
	add := func(who string) {
		if who = strings.TrimSpace(who); who != "" && !seen[strings.ToLower(who)] {
			seen[strings.ToLower(who)] = true
			trailers = append(trailers, "Co-authored-by: "+who)
		}
	}
	for _, rec := range strings.Split(log, "\x1e") {
		if parts := strings.SplitN(strings.TrimSpace(rec), "\x1f", 2); len(parts) == 2 {
			add(parts[0])
			for _, m := range coAuthorRE.FindAllStringSubmatch(parts[1], -1) {
				add(m[1])
			}
		}
	}
	if len(trailers) > 0 {
		msg += "\n\n" + strings.Join(trailers, "\n")
	}
	return msg + "\n"
}

// PushFF advances trunk from tip to cand on the remote, fast-forward only. It never forces: the
// refspec has no `+` and the argv no force flag. ErrTrunkMoved means rebuild and re-verify.
// After the push it reads trunk back from the remote: the remote's answer is the proof, not git's
// exit code.
func (g Git) PushFF(shadow bool, trunk, tip, cand string) error {
	if shadow {
		return ErrShadowPush
	}
	if _, err := g.git("merge-base", "--is-ancestor", tip, cand); err != nil {
		return ErrNotFastForward
	}
	if now, err := g.RemoteTip(trunk); err != nil {
		return err
	} else if now != tip {
		return ErrTrunkMoved
	}
	_, perr := g.git(pushArgs(trunk, cand)...)
	now, err := g.RemoteTip(trunk)
	switch {
	case err != nil:
		return err
	case now == cand:
		return nil
	case now != tip:
		return ErrTrunkMoved
	case perr == nil:
		return fmt.Errorf("push reported success but remote trunk is %s, not %s", now, cand)
	}
	return perr
}

// HeadTime is the committer date of commit sha.
func (g Git) HeadTime(sha string) (string, error) { return g.git("show", "-s", "--format=%cI", sha) }

func pushArgs(trunk, cand string) []string {
	return []string{"-c", "core.hooksPath=/dev/null", "push", "--no-verify", "--porcelain", "origin", cand + ":refs/heads/" + trunk}
}
