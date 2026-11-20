package procs

import "fmt"

type LockHolder struct {
	Held   bool
	Write  bool
	Pid    int
	Start  int64
	Length int64
}

func (h LockHolder) Kind() string {
	switch {
	case !h.Held:
		return "none"
	case h.Write:
		return "write"
	default:
		return "read"
	}
}

func (h LockHolder) Describe(names func(pid int) string) string {
	if !h.Held {
		return "unlocked"
	}
	return fmt.Sprintf("%s lock held by %s", h.Kind(), names(h.Pid))
}
