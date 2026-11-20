package trace

// canonicalNames collapses platform- and build-dependent Linux syscall variants
// (for example fsync vs fdatasync, open vs openat, stat vs newfstatat) so
// evidence output stays identical across CLI builds and architectures.
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

// CanonicalName maps a Linux syscall name to its normalized course name,
// returning name unchanged when no alias applies.
func CanonicalName(name string) string {
	if canonical, ok := canonicalNames[name]; ok {
		return canonical
	}
	return name
}

// Canonical returns a copy of s with Name replaced by CanonicalName(s.Name).
func (s Syscall) Canonical() Syscall {
	s.Name = CanonicalName(s.Name)
	return s
}

// IsSync reports whether s is a file synchronization syscall (fsync or fdatasync).
func IsSync(s Syscall) bool {
	return CanonicalName(s.Name) == "sync"
}
