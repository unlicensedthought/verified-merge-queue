// Command vmq is a serial merge queue. A land request is rebased onto trunk, squashed into one
// candidate commit, verified in a checkout of exactly that commit, and only then fast-forwarded onto
// trunk, one request at a time per repo, first label first, then oldest first. --shadow does
// everything except push, write verdicts back and delete request refs, and records each verdict in
// a JSONL log.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// RepoConf is one repo the queue serves.
type RepoConf struct {
	Name   string `json:"name"`   // clone directory and results directory name
	Remote string `json:"remote"` // git URL holding trunk and the request refs
	Trunk  string `json:"trunk"`  // default main
	// VerifyCmd and AfterLand override the global values for this repo; unset inherits them.
	VerifyCmd []string   `json:"verify_cmd"`
	AfterLand [][]string `json:"after_land"`
}

// Config is the daemon's configuration file. Unset fields take defaults.
type Config struct {
	Repos            []RepoConf `json:"repos"`
	WorkDir          string     `json:"workdir"`     // clones at <workdir>/<name>, verify logs at <workdir>/logs
	Label            string     `json:"label"`       // request namespace refs/<label>/<n>
	FirstLabel       string     `json:"first_label"` // sorts ahead of Label within a repo
	VerifyCmd        []string   `json:"verify_cmd"`  // run as <cmd...> --base <trunk-sha> in the candidate checkout
	VerifyTimeoutSec int        `json:"verify_timeout_seconds"`
	AfterLand        [][]string `json:"after_land"` // argv list run after a land; {repo} {sha} {before} {tree} {pr} {trunk}
	PollSeconds      int        `json:"poll_seconds"`
	VerdictLog       string     `json:"verdict_log"`
	AuthorName       string     `json:"author_name"` // squash author and committer
	AuthorEmail      string     `json:"author_email"`
	MaxCouldNotRun   int        `json:"max_could_not_run"`
	RetryBaseSec     int        `json:"retry_base_seconds"`
	MaxReloops       int        `json:"max_reloops"` // trunk-moved re-verifies before could-not-run
}

func orStr(s *string, v string) {
	if *s == "" {
		*s = v
	}
}

func orInt(n *int, v int) {
	if *n <= 0 {
		*n = v
	}
}

// Defaults fills every unset field and refuses a config that cannot run.
func (c *Config) Defaults() error {
	if len(c.Repos) == 0 {
		return errors.New("config: repos is empty")
	}
	orStr(&c.WorkDir, "vmq-work")
	orStr(&c.VerdictLog, filepath.Join(c.WorkDir, "verdicts.jsonl"))
	orStr(&c.Label, "land")
	orStr(&c.FirstLabel, "land-first")
	orStr(&c.AuthorName, "merge queue")
	orStr(&c.AuthorEmail, "vmq@example.invalid")
	if len(c.VerifyCmd) == 0 {
		c.VerifyCmd = []string{"./verify"}
	}
	orInt(&c.VerifyTimeoutSec, 1800)
	orInt(&c.PollSeconds, 30)
	orInt(&c.MaxCouldNotRun, 3)
	orInt(&c.RetryBaseSec, 60)
	orInt(&c.MaxReloops, 5)
	for i := range c.Repos {
		r := &c.Repos[i]
		if r.Name == "" || filepath.Base(r.Name) != r.Name {
			return fmt.Errorf("config: repo name %q must be a plain directory name", r.Name)
		}
		if r.Remote == "" {
			return fmt.Errorf("config: repo %q must name its remote", r.Name)
		}
		orStr(&r.Trunk, "main")
		if len(r.VerifyCmd) == 0 {
			r.VerifyCmd = c.VerifyCmd
		}
		if len(r.AfterLand) == 0 {
			r.AfterLand = c.AfterLand
		}
	}
	return nil
}

// Logger writes one JSON object per line.
type Logger struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *Logger) emit(level, event string, f map[string]any) {
	m := map[string]any{"ts": time.Now().UTC().Format(time.RFC3339Nano), "level": level, "event": event}
	for k, v := range f {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintln(l.w, string(b))
}

// Info logs progress.
func (l *Logger) Info(event string, f map[string]any) { l.emit("info", event, f) }

// Error logs a failure. Every error path goes through here.
func (l *Logger) Error(event string, f map[string]any) { l.emit("error", event, f) }

// draining is set by the first stop signal: no new land starts, the running one finishes.
var draining atomic.Bool

func main() {
	cfgPath := flag.String("config", "vmq.json", "config file")
	shadow := flag.Bool("shadow", false, "verify and record only: never push, write verdicts back or delete request refs")
	once := flag.Bool("once", false, "poll each repo once and exit")
	flag.Parse()
	log := &Logger{w: os.Stdout}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Error("config_invalid", map[string]any{"path": *cfgPath, "error": err.Error()})
		os.Exit(2)
	}
	log.Info("vmq_start", map[string]any{"shadow": *shadow, "repos": len(cfg.Repos), "verify_cmd": cfg.VerifyCmd})
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		s := <-sigs
		draining.Store(true)
		log.Info("draining", map[string]any{"signal": s.String(), "action": "the running land finishes; a second signal exits now"})
		<-sigs
		log.Error("drain_interrupted", map[string]any{"action": "exiting with a land possibly unfinished; its request ref is untouched"})
		os.Exit(130)
	}()
	var wg sync.WaitGroup
	for _, rc := range cfg.Repos {
		q := NewQueue(&cfg, rc, NewRefIntake(&cfg, rc, *shadow, log), log, *shadow)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !draining.Load() {
				q.Tick()
				if *once {
					return
				}
				time.Sleep(time.Duration(cfg.PollSeconds) * time.Second)
			}
		}()
	}
	wg.Wait()
}

func loadConfig(path string) (Config, error) {
	var cfg Config
	raw, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(raw, &cfg)
	}
	if err == nil {
		err = cfg.Defaults()
	}
	return cfg, err
}
