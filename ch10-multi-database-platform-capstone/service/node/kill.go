package node

// Signal delivers SIGKILL to the child process without waiting for it to exit.
// Splitting Signal from Reap lets Experiment 10.2's timer goroutine kill a
// node mid-command while the main goroutine stops sending and reaps it cleanly.
func (p *Process) Signal() error { return p.cmd.Process.Kill() }

// Reap closes stdin and waits for a signaled child process to exit so no
// zombie process remains.
func (p *Process) Reap() error {
	_ = p.stdin.Close()
	return p.cmd.Wait()
}
