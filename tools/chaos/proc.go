package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// A worker process runs in its own process group, so signals reach the SDK
// process however the command starts it (a shell, go run, npx).
type worker struct {
	index   int
	sdk     string
	command string
	run     *run
	// version is the code version the worker runs as (-rolling); "" for none.
	version string

	mu          sync.Mutex
	incarnation int
	cmd         *exec.Cmd
	label       string
	expectExit  bool
	paused      bool
	retired     bool // stopped for good by the rollout; never restarted
	exited      chan struct{}
	unexpected  []string
	readyWithin time.Duration
}

func (w *worker) name() string {
	if w.version != "" {
		return fmt.Sprintf("%s@%s-%d", w.sdk, w.version, w.index)
	}
	return fmt.Sprintf("%s-%d", w.sdk, w.index)
}

// labelVersion is the version in a worker label ("go@A-0.3"), or "".
func labelVersion(label string) string {
	_, rest, ok := strings.Cut(label, "@")
	if !ok {
		return ""
	}
	v, _, _ := strings.Cut(rest, "-")
	return v
}

func (w *worker) env(label string) []string {
	r := w.run
	env := append(os.Environ(),
		"RUNNERQ_CHAOS_DSN="+r.dsn,
		"RUNNERQ_CHAOS_QUEUE="+r.queue,
		"RUNNERQ_CHAOS_PREFIX="+w.sdk,
		"RUNNERQ_CHAOS_WORKER="+label,
		fmt.Sprintf("RUNNERQ_CHAOS_CONCURRENCY=%d", r.concurrency),
		fmt.Sprintf("RUNNERQ_CHAOS_LEASE_MS=%d", r.lease.Milliseconds()),
	)
	if w.version != "" {
		env = append(env, "RUNNERQ_CHAOS_VERSION="+w.version)
	}
	return env
}

// start launches a new incarnation and waits for its ready line.
func (w *worker) start() error {
	w.mu.Lock()
	if w.retired {
		w.mu.Unlock()
		return nil
	}
	w.incarnation++
	label := fmt.Sprintf("%s.%d", w.name(), w.incarnation)
	w.mu.Unlock()

	logFile, err := os.OpenFile(filepath.Join(w.run.logs, w.name()+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	fmt.Fprintf(logFile, "--- %s started at %s\n", label, time.Now().Format(time.RFC3339Nano))
	cmd := exec.Command("sh", "-c", "exec "+w.command+" work")
	cmd.Env = w.env(label)
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		logFile.Close()
		return err
	}
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("start %s: %w", label, err)
	}
	ready := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(stdout)
		once := false
		for sc.Scan() {
			if !once && bytes.Contains(sc.Bytes(), []byte(`"ready"`)) {
				once = true
				close(ready)
				continue
			}
			fmt.Fprintf(logFile, "%s\n", sc.Bytes())
		}
	}()
	exited := make(chan struct{})
	w.mu.Lock()
	w.cmd, w.label, w.expectExit, w.paused, w.exited = cmd, label, false, false, exited
	w.mu.Unlock()
	go func() {
		err := cmd.Wait()
		fmt.Fprintf(logFile, "--- %s exited at %s: %v\n", label, time.Now().Format(time.RFC3339Nano), err)
		logFile.Close()
		w.mu.Lock()
		if !w.expectExit {
			w.unexpected = append(w.unexpected, fmt.Sprintf("%s exited on its own (%v)", label, err))
		}
		w.mu.Unlock()
		close(exited)
	}()
	select {
	case <-ready:
		return nil
	case <-exited:
		return fmt.Errorf("%s exited before it was ready; see %s", label, logFile.Name())
	case <-time.After(w.readyWithin):
		return fmt.Errorf("%s not ready within %s; see %s", label, w.readyWithin, logFile.Name())
	}
}

func (w *worker) signal(sig syscall.Signal) error {
	w.mu.Lock()
	cmd := w.cmd
	w.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return errors.New("not running")
	}
	return syscall.Kill(-cmd.Process.Pid, sig)
}

// kill SIGKILLs the current incarnation and waits for it to go.
func (w *worker) kill() {
	w.mu.Lock()
	w.expectExit = true
	exited := w.exited
	w.mu.Unlock()
	_ = w.signal(syscall.SIGKILL)
	<-exited
}

func (w *worker) alive() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.exited == nil {
		return false
	}
	select {
	case <-w.exited:
		return false
	default:
		return true
	}
}

// retire stops the worker for good.
func (w *worker) retire(grace time.Duration) {
	w.mu.Lock()
	w.retired = true
	w.mu.Unlock()
	w.stop(grace)
}

func (w *worker) isRetired() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.retired
}

// stop asks for a graceful shutdown, then kills.
func (w *worker) stop(grace time.Duration) {
	w.mu.Lock()
	w.expectExit = true
	exited := w.exited
	paused := w.paused
	w.mu.Unlock()
	if exited == nil {
		return
	}
	if paused {
		_ = w.signal(syscall.SIGCONT)
	}
	_ = w.signal(syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(grace):
		_ = w.signal(syscall.SIGKILL)
		<-exited
	}
}

// client is an SDK's client process: enqueue and signal requests on stdin.
type client struct {
	sdk     string
	command string
	run     *run

	mu      sync.Mutex
	version string // stamped on the roots it enqueues (-rolling)
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	lines   chan []byte
}

// setVersion makes the next incarnation enqueue as version, and ends this one.
func (c *client) setVersion(version string) {
	c.mu.Lock()
	c.version = version
	c.mu.Unlock()
	c.stop()
}

func (c *client) start() error {
	logFile, err := os.OpenFile(filepath.Join(c.run.logs, c.sdk+"-client.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	cmd := exec.Command("sh", "-c", "exec "+c.command+" client")
	cmd.Env = append(os.Environ(),
		"RUNNERQ_CHAOS_DSN="+c.run.dsn,
		"RUNNERQ_CHAOS_QUEUE="+c.run.queue,
		"RUNNERQ_CHAOS_PREFIX="+c.sdk,
		"RUNNERQ_CHAOS_WORKER="+c.sdk+"-client",
	)
	c.mu.Lock()
	if c.version != "" {
		cmd.Env = append(cmd.Env, "RUNNERQ_CHAOS_VERSION="+c.version)
	}
	c.mu.Unlock()
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s client: %w", c.sdk, err)
	}
	lines := make(chan []byte, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		for sc.Scan() {
			lines <- bytes.Clone(sc.Bytes())
		}
		close(lines)
	}()
	go func() {
		err := cmd.Wait()
		fmt.Fprintf(logFile, "--- client exited at %s: %v\n", time.Now().Format(time.RFC3339Nano), err)
		logFile.Close()
	}()
	select {
	case line, ok := <-lines:
		if ok && bytes.Contains(line, []byte(`"ready"`)) {
			c.mu.Lock()
			c.cmd, c.stdin, c.lines = cmd, stdin, lines
			c.mu.Unlock()
			return nil
		}
	case <-time.After(2 * time.Minute):
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	return fmt.Errorf("%s client did not start; see %s", c.sdk, logFile.Name())
}

// call sends one request; a dead client is restarted on the next call.
func (c *client) call(req map[string]any) (json.RawMessage, error) {
	c.mu.Lock()
	stdin, lines := c.stdin, c.lines
	c.mu.Unlock()
	if stdin == nil {
		if err := c.start(); err != nil {
			return nil, err
		}
		c.mu.Lock()
		stdin, lines = c.stdin, c.lines
		c.mu.Unlock()
	}
	body, _ := json.Marshal(req)
	if _, err := stdin.Write(append(body, '\n')); err != nil {
		return nil, c.restart(err)
	}
	select {
	case line, ok := <-lines:
		if !ok {
			return nil, c.restart(errors.New("client exited"))
		}
		var r struct {
			OK    json.RawMessage `json:"ok"`
			Error string          `json:"error"`
		}
		if err := json.Unmarshal(line, &r); err != nil {
			return nil, fmt.Errorf("bad reply %s", line)
		}
		if r.OK == nil {
			return nil, errors.New(r.Error)
		}
		return r.OK, nil
	case <-time.After(30 * time.Second):
		return nil, c.restart(errors.New("no reply within 30s"))
	}
}

// restart drops the client; the next call starts a new one.
func (c *client) restart(cause error) error {
	c.stop()
	return cause
}

func (c *client) stop() {
	c.mu.Lock()
	cmd, stdin, lines := c.cmd, c.stdin, c.lines
	c.cmd, c.stdin, c.lines = nil, nil, nil
	c.mu.Unlock()
	if cmd == nil {
		return
	}
	_ = stdin.Close()
	done := make(chan struct{})
	go func() {
		for range lines {
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
