package node

func (p *Process) Signal() error { return p.cmd.Process.Kill() }

func (p *Process) Reap() error {
	_ = p.stdin.Close()
	return p.cmd.Wait()
}
