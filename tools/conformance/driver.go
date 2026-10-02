package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"
)

// driver is one SDK's driver process, speaking JSON lines.
type driver struct {
	name   string
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	lines  chan []byte
	stderr *tailBuffer
}

type reply struct {
	OK    json.RawMessage `json:"ok"`
	Error *struct {
		Kind    string `json:"kind"`
		Message string `json:"message"`
	} `json:"error"`
}

func startDriver(name, command string) (*driver, error) {
	cmd := exec.Command("sh", "-c", command)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	d := &driver{name: name, cmd: cmd, stdin: stdin, lines: make(chan []byte), stderr: &tailBuffer{max: 8 << 10}}
	cmd.Stderr = d.stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("driver %s: %w", name, err)
	}
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		for sc.Scan() {
			d.lines <- bytes.Clone(sc.Bytes())
		}
		close(d.lines)
	}()
	return d, nil
}

// call sends one request and waits for its reply. A driver may take a while
// to start (compiling, say), so the first call waits longer.
func (d *driver) call(op string, args map[string]any, timeout time.Duration) (*reply, error) {
	req, err := json.Marshal(map[string]any{"op": op, "args": args})
	if err != nil {
		return nil, err
	}
	if _, err := d.stdin.Write(append(req, '\n')); err != nil {
		return nil, fmt.Errorf("driver %s: write %s: %w", d.name, op, err)
	}
	select {
	case line, ok := <-d.lines:
		if !ok {
			return nil, fmt.Errorf("driver %s exited during %s", d.name, op)
		}
		var r reply
		if err := json.Unmarshal(line, &r); err != nil || (r.OK == nil && r.Error == nil) {
			return nil, fmt.Errorf("driver %s: %s: bad reply %s", d.name, op, line)
		}
		return &r, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("driver %s: no reply to %s within %s", d.name, op, timeout)
	}
}

func (d *driver) stderrTail() string { return d.stderr.String() }

func (d *driver) stop() {
	d.stdin.Close()
	done := make(chan struct{})
	go func() { d.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		d.cmd.Process.Kill()
	}
}

// tailBuffer keeps the last max bytes written to it, for failure reports.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := string(t.buf)
	t.buf = nil
	return s
}
