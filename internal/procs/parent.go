package procs

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

const DefaultTimeout = 30 * time.Second

var (
	ErrChildDied    = errors.New("procs: child exited")
	ErrTimeout      = errors.New("procs: child did not answer in time")
	ErrBusyChild    = errors.New("procs: child still has a request outstanding")
	ErrStepMismatch = errors.New("procs: reply does not match the request")
	ErrStopped      = errors.New("procs: child already stopped")
)

type Child struct {
	Name    string
	Role    string
	Pid     int
	Timeout time.Duration

	cmd      *exec.Cmd
	requests *os.File
	replies  chan Result
	exited   chan struct{}
	killed   chan struct{}
	killOnce sync.Once
	output   *tailBuffer

	mu          sync.Mutex
	outstanding *Call
	waitErr     error
	readErr     error
	stopped     bool
}

type Call struct {
	child   *Child
	request Request
	result  *Result
	err     error
	done    bool
}

func StartChild(name, role string, args ...string) (*Child, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return StartCommand(name, role, self, args...)
}

func StartCommand(name, role, executable string, args ...string) (*Child, error) {
	requestRead, requestWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	replyRead, replyWrite, err := os.Pipe()
	if err != nil {
		requestRead.Close()
		requestWrite.Close()
		return nil, err
	}
	output := &tailBuffer{limit: 8192}
	cmd := exec.Command(executable, append([]string{ChildMarker, role}, args...)...)
	cmd.Stdout = output
	cmd.Stderr = output
	cmd.ExtraFiles = []*os.File{requestRead, replyWrite}
	if err := cmd.Start(); err != nil {
		requestRead.Close()
		requestWrite.Close()
		replyRead.Close()
		replyWrite.Close()
		return nil, fmt.Errorf("start child %s: %w", name, err)
	}
	requestRead.Close()
	replyWrite.Close()
	child := &Child{
		Name:     name,
		Role:     role,
		Timeout:  DefaultTimeout,
		cmd:      cmd,
		requests: requestWrite,
		replies:  make(chan Result, 1),
		exited:   make(chan struct{}),
		killed:   make(chan struct{}),
		output:   output,
	}
	go child.readReplies(replyRead)
	go func() {
		err := cmd.Wait()
		child.mu.Lock()
		child.waitErr = err
		child.mu.Unlock()
		close(child.exited)
	}()
	ready, err := child.await(Request{Step: 0, Action: ActionReady}, child.Timeout)
	if err != nil {
		child.Kill()
		return nil, err
	}
	if !ready.OK() {
		child.Kill()
		return nil, fmt.Errorf("child %s: bad ready message: %s", name, ready.Outcome())
	}
	pid, err := strconv.Atoi(ready.Value)
	if err != nil || pid != cmd.Process.Pid {
		child.Kill()
		return nil, fmt.Errorf("child %s: ready message names pid %q, the process is %d", name, ready.Value, cmd.Process.Pid)
	}
	child.Pid = pid
	return child, nil
}

func (c *Child) readReplies(r io.ReadCloser) {
	defer r.Close()
	defer close(c.replies)
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var result Result
		if err := json.Unmarshal(scanner.Bytes(), &result); err != nil {
			c.mu.Lock()
			c.readErr = fmt.Errorf("malformed reply %q: %w", scanner.Text(), err)
			c.mu.Unlock()
			return
		}
		select {
		case c.replies <- result:
		case <-c.killed:
			return
		}
	}
	if err := scanner.Err(); err != nil {
		c.mu.Lock()
		c.readErr = err
		c.mu.Unlock()
	}
}

func (c *Child) await(request Request, timeout time.Duration) (Result, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case result, ok := <-c.replies:
		if !ok {
			return Result{}, c.deathError(request)
		}
		if result.Step != request.Step || result.Action != request.Action {
			return Result{}, fmt.Errorf("%w: child %s answered step %d %q to step %d %q", ErrStepMismatch, c.Name, result.Step, result.Action, request.Step, request.Action)
		}
		return result, nil
	case <-timer.C:
		c.Kill()
		return Result{}, fmt.Errorf("%w: child %s, step %d %q, after %v; output: %s", ErrTimeout, c.Name, request.Step, request.Action, timeout, c.output.String())
	}
}

func (c *Child) deathError(request Request) error {
	select {
	case <-c.exited:
	case <-time.After(5 * time.Second):
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	status := "still running"
	select {
	case <-c.exited:
		status = "exited cleanly"
		if c.waitErr != nil {
			status = c.waitErr.Error()
		}
	default:
	}
	detail := ""
	if c.readErr != nil {
		detail = "; reply pipe: " + c.readErr.Error()
	}
	return fmt.Errorf("%w: child %s (%s) during step %d %q: %s%s; output: %s", ErrChildDied, c.Name, c.Role, request.Step, request.Action, status, detail, c.output.String())
}

func (c *Child) Start(step int, action string, args ...string) (*Call, error) {
	if step <= 0 {
		return nil, fmt.Errorf("procs: step numbers start at 1, got %d", step)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return nil, fmt.Errorf("%w: %s", ErrStopped, c.Name)
	}
	if c.outstanding != nil && !c.outstanding.done {
		return nil, fmt.Errorf("%w: %s has step %d %q", ErrBusyChild, c.Name, c.outstanding.request.Step, c.outstanding.request.Action)
	}
	request := Request{Step: step, Action: action, Args: args}
	line, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	if _, err := c.requests.Write(append(line, '\n')); err != nil {
		return nil, fmt.Errorf("%w: child %s: write step %d: %v; output: %s", ErrChildDied, c.Name, step, err, c.output.String())
	}
	call := &Call{child: c, request: request}
	c.outstanding = call
	return call, nil
}

func (c *Child) Do(step int, action string, args ...string) (Result, error) {
	call, err := c.Start(step, action, args...)
	if err != nil {
		return Result{}, err
	}
	return call.Wait()
}

func (call *Call) Request() Request { return call.request }

func (call *Call) Wait() (Result, error) {
	return call.WaitFor(call.child.Timeout)
}

func (call *Call) WaitFor(timeout time.Duration) (Result, error) {
	if call.done {
		if call.result == nil {
			return Result{}, call.err
		}
		return *call.result, call.err
	}
	result, err := call.child.await(call.request, timeout)
	call.finish(result, err)
	return result, err
}

func (call *Call) Answered() (bool, error) {
	if call.done {
		return true, call.err
	}
	select {
	case result, ok := <-call.child.replies:
		return call.accept(result, ok)
	default:
		return false, nil
	}
}

func (call *Call) AnsweredWithin(d time.Duration) (bool, error) {
	if d <= 0 {
		return call.Answered()
	}
	if call.done {
		return true, call.err
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case result, ok := <-call.child.replies:
		return call.accept(result, ok)
	case <-timer.C:
		return false, nil
	}
}

func (call *Call) accept(result Result, ok bool) (bool, error) {
	if !ok {
		err := call.child.deathError(call.request)
		call.finish(Result{}, err)
		return true, err
	}
	if result.Step != call.request.Step || result.Action != call.request.Action {
		err := fmt.Errorf("%w: child %s answered step %d %q to step %d %q", ErrStepMismatch, call.child.Name, result.Step, result.Action, call.request.Step, call.request.Action)
		call.finish(Result{}, err)
		return true, err
	}
	call.finish(result, nil)
	return true, nil
}

func (call *Call) finish(result Result, err error) {
	call.child.mu.Lock()
	defer call.child.mu.Unlock()
	call.done = true
	call.err = err
	if err == nil {
		call.result = &result
	}
}

func (c *Child) Stop() error {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return nil
	}
	var pending *Call
	if c.outstanding != nil && !c.outstanding.done {
		pending = c.outstanding
	}
	c.mu.Unlock()
	if pending != nil {
		c.Kill()
		return fmt.Errorf("%w: %s stopped with step %d %q unanswered", ErrBusyChild, c.Name, pending.request.Step, pending.request.Action)
	}
	request := Request{Step: -1, Action: ActionExit}
	line, err := json.Marshal(request)
	if err != nil {
		return err
	}
	if _, err := c.requests.Write(append(line, '\n')); err != nil {
		c.Kill()
		return fmt.Errorf("%w: child %s: %v; output: %s", ErrChildDied, c.Name, err, c.output.String())
	}
	result, err := c.await(request, c.Timeout)
	c.requests.Close()
	c.mu.Lock()
	c.stopped = true
	c.mu.Unlock()
	if err != nil {
		c.Kill()
		return err
	}
	select {
	case <-c.exited:
	case <-time.After(c.Timeout):
		c.Kill()
		return fmt.Errorf("%w: child %s answered exit but did not exit", ErrTimeout, c.Name)
	}
	c.mu.Lock()
	waitErr := c.waitErr
	c.mu.Unlock()
	if !result.OK() {
		return fmt.Errorf("child %s: close failed: %s", c.Name, result.Outcome())
	}
	if waitErr != nil {
		return fmt.Errorf("child %s: %v; output: %s", c.Name, waitErr, c.output.String())
	}
	return nil
}

func (c *Child) Kill() {
	c.mu.Lock()
	c.stopped = true
	c.mu.Unlock()
	c.killOnce.Do(func() { close(c.killed) })
	if c.cmd.Process != nil {
		c.cmd.Process.Kill()
	}
	c.requests.Close()
	select {
	case <-c.exited:
	case <-time.After(5 * time.Second):
	}
}

func (c *Child) Output() string { return c.output.String() }

type tailBuffer struct {
	mu    sync.Mutex
	limit int
	data  []byte
	lost  int
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	if extra := len(b.data) - b.limit; extra > 0 {
		b.lost += extra
		b.data = append([]byte(nil), b.data[extra:]...)
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	text := strings.TrimSpace(string(b.data))
	if text == "" {
		return "(none)"
	}
	if b.lost > 0 {
		return fmt.Sprintf("[%d earlier bytes dropped] %s", b.lost, text)
	}
	return text
}
