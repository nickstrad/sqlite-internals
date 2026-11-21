package procs

import "fmt"

// LockHolder describes the conflicting POSIX advisory lock reported by
// fcntl(F_GETLK) for a probed byte range.
type LockHolder struct {
	Held   bool
	Write  bool
	Pid    int
	Start  int64
	Length int64
}

// Kind returns "none", "read", or "write".
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

// Describe returns a human-readable lock summary naming the conflicting holder via names.
func (h LockHolder) Describe(names func(pid int) string) string {
	if !h.Held {
		return "unlocked"
	}
	return fmt.Sprintf("%s lock held by %s", h.Kind(), names(h.Pid))
}
