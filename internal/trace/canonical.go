package trace

var canonicalNames = map[string]string{
	"open":       "open",
	"openat":     "open",
	"unlink":     "unlink",
	"unlinkat":   "unlink",
	"fsync":      "sync",
	"fdatasync":  "sync",
	"stat":       "stat",
	"lstat":      "stat",
	"stat64":     "stat",
	"lstat64":    "stat",
	"newfstatat": "stat",
	"fstatat64":  "stat",
	"statx":      "stat",
	"fstat":      "stat",
	"fstat64":    "stat",
	"access":     "access",
	"faccessat":  "access",
	"faccessat2": "access",
	"pwrite":     "pwrite64",
	"pread":      "pread64",
}

func CanonicalName(name string) string {
	if canonical, ok := canonicalNames[name]; ok {
		return canonical
	}
	return name
}

func (s Syscall) Canonical() Syscall {
	s.Name = CanonicalName(s.Name)
	return s
}

func IsSync(s Syscall) bool {
	return CanonicalName(s.Name) == "sync"
}
