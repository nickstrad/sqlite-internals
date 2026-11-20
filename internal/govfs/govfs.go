// Package govfs registers a pure-Go forwarding VFS shim on top of
// modernc.org/sqlite's default VFS (unix on Linux and macOS). It records every
// VFS and sqlite3_io_methods call in-process and supports deterministic fault
// injection on xRead, xWrite, xSync, and xTruncate without cgo or custom C.
//
// Like ext/misc/vfstrace.c (vfstrace_register, lines 1138-1199), each shim
// sqlite3_file prefixes an 8-byte header (its own pMethods pointer) ahead of
// the underlying VFS's sqlite3_file bytes in memory allocated by SQLite.
package govfs

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"unsafe"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

// headerSize is the byte size of the leading sqlite3_file header (one pMethods
// pointer on 64-bit platforms) placed in front of the wrapped VFS's file object
// (sqlite.h.in:747-749).
const headerSize = 8

// Options configures a shim VFS registered with Register.
type Options struct {
	IOVersion   int32
	MakeDefault bool
}

// Fault specifies a single deterministic error to inject on the Nth matching
// call to Method (one of xRead, xWrite, xSync, xTruncate), optionally limited
// to files of Kind.
type Fault struct {
	Method string
	Nth    int
	Code   int32
	Kind   FileKind
}

// FaultReport summarizes what happened to an armed Fault when Disarm is called.
type FaultReport struct {
	Armed bool
	Fault Fault
	Seen  int
	Fired bool
	Call  Call
}

// VFS is a registered Go forwarding VFS that records calls and can inject
// single-shot I/O errors.
type VFS struct {
	name      string
	address   uintptr
	ioVersion int32

	mu        sync.Mutex
	recording bool
	seq       int
	calls     []Call
	fault     *Fault
	faultSeen int
	fired     *Call
	err       error
}

type openFile struct {
	vfs         *VFS
	path        string
	name        string
	kind        FileKind
	realMethods uintptr
}

type methodTable struct {
	address uintptr
	version int32
}

// registry maps C-memory sqlite3_vfs and sqlite3_file addresses back to Go
// state so no Go pointer is ever stored inside C-allocated SQLite structs.
var registry = struct {
	sync.RWMutex
	realVFS   uintptr
	byAddress map[uintptr]*VFS
	byName    map[string]*VFS
	files     map[uintptr]*openFile
	tables    map[[2]uintptr]methodTable
	checked   map[uintptr]error
}{
	byAddress: map[uintptr]*VFS{},
	byName:    map[string]*VFS{},
	files:     map[uintptr]*openFile{},
	tables:    map[[2]uintptr]methodTable{},
	checked:   map[uintptr]error{},
}

// ErrAlreadyRegistered is returned by Register when SQLite already has a VFS
// with the requested name.
var ErrAlreadyRegistered = errors.New("a VFS with this name is already registered")

// Register allocates and registers a forwarding sqlite3_vfs named name using
// sqlite3_vfs_find(NULL) as its underlying VFS (os.c:362-381) and
// sqlite3_vfs_register (os.c:408-431). An Options.IOVersion of 0 defaults to 3.
// Startup self-checks verify the default VFS layout and ccgo's function-pointer
// convention before registering.
func Register(name string, options Options) (*VFS, error) {
	if name == "" {
		return nil, errors.New("govfs: empty VFS name")
	}
	version := options.IOVersion
	if version == 0 {
		version = 3
	}
	if version < 1 || version > 3 {
		return nil, fmt.Errorf("govfs: io_methods version %d not in 1..3", version)
	}
	tls := libc.NewTLS()
	defer tls.Close()

	registry.Lock()
	defer registry.Unlock()
	if _, ok := registry.byName[name]; ok {
		return nil, fmt.Errorf("govfs: %q: %w", name, ErrAlreadyRegistered)
	}
	// Allocate zName in C memory and keep it alive for the process lifetime:
	// sqlite3_vfs_register stores the pointer without copying (os.c:408-431).
	cname, err := libc.CString(name)
	if err != nil {
		return nil, err
	}
	if sqlite3.Xsqlite3_vfs_find(tls, cname) != 0 {
		return nil, fmt.Errorf("govfs: %q: %w", name, ErrAlreadyRegistered)
	}
	if registry.realVFS == 0 {
		real := sqlite3.Xsqlite3_vfs_find(tls, 0)
		if real == 0 {
			return nil, errors.New("govfs: SQLite has no default VFS")
		}
		if err := checkRealVFS(real); err != nil {
			return nil, err
		}
		if err := checkFunctionPointerConvention(); err != nil {
			return nil, err
		}
		registry.realVFS = real
	}
	real := at[sqlite3.Tsqlite3_vfs](registry.realVFS)
	address := sqlite3.Xsqlite3_malloc64(tls, uint64(unsafe.Sizeof(sqlite3.Tsqlite3_vfs{})))
	if address == 0 {
		return nil, errors.New("govfs: out of memory")
	}
	// Copy the underlying VFS struct verbatim, grow szOsFile by headerSize for
	// our leading pMethods pointer, and swap in only the 9 wrapped VFS slots.
	shim := at[sqlite3.Tsqlite3_vfs](address)
	*shim = *real
	shim.FzName = cname
	shim.FpNext = 0
	shim.FszOsFile = real.FszOsFile + headerSize
	for _, slot := range vfsSlots {
		if slot.MinVersion > real.FiVersion || *slot.field(real) == 0 {
			continue
		}
		*slot.field(shim) = slot.wrapper
	}
	v := &VFS{name: name, address: address, ioVersion: version, recording: true}
	registry.byAddress[address] = v
	registry.byName[name] = v
	if rc := sqlite3.Xsqlite3_vfs_register(tls, address, libc.Bool32(options.MakeDefault)); rc != sqlite3.SQLITE_OK {
		delete(registry.byAddress, address)
		delete(registry.byName, name)
		return nil, fmt.Errorf("govfs: sqlite3_vfs_register: %d", rc)
	}
	return v, nil
}

// Lookup returns the registered shim VFS for name, or nil if none exists.
func Lookup(name string) *VFS {
	registry.RLock()
	defer registry.RUnlock()
	return registry.byName[name]
}

// Name returns the registered SQLite VFS name for URI selection (?vfs=<name>).
func (v *VFS) Name() string {
	return v.name
}

// IOVersion returns the maximum sqlite3_io_methods version this shim advertises.
func (v *VFS) IOVersion() int32 {
	return v.ioVersion
}

// SetRecording enables or disables recording of forwarded VFS calls.
func (v *VFS) SetRecording(on bool) {
	v.mu.Lock()
	v.recording = on
	v.mu.Unlock()
}

// Reset clears all recorded calls and resets the sequence counter to zero.
func (v *VFS) Reset() {
	v.mu.Lock()
	v.calls = nil
	v.seq = 0
	v.mu.Unlock()
}

// Calls returns a snapshot copy of all recorded calls since the last Reset or TakeCalls.
func (v *VFS) Calls() []Call {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]Call(nil), v.calls...)
}

// TakeCalls returns all recorded calls and clears the buffer, resetting the
// sequence counter to zero.
func (v *VFS) TakeCalls() []Call {
	v.mu.Lock()
	defer v.mu.Unlock()
	calls := v.calls
	v.calls = nil
	v.seq = 0
	return calls
}

// Arm installs a one-shot fault that will fail the Nth matching call with
// f.Code instead of forwarding to the underlying VFS.
func (v *VFS) Arm(f Fault) error {
	if f.Nth < 1 {
		return fmt.Errorf("govfs: fault Nth must be >= 1, got %d", f.Nth)
	}
	if !faultable[f.Method] {
		return fmt.Errorf("govfs: method %q cannot be failed (faultable: xRead xWrite xSync xTruncate)", f.Method)
	}
	if f.Code == CodeOK {
		return errors.New("govfs: fault code must not be SQLITE_OK")
	}
	v.mu.Lock()
	v.fault = &f
	v.faultSeen = 0
	v.fired = nil
	v.mu.Unlock()
	return nil
}

// Disarm clears any armed fault and returns a FaultReport describing how many
// matching calls were observed and whether the fault fired.
func (v *VFS) Disarm() FaultReport {
	v.mu.Lock()
	defer v.mu.Unlock()
	report := FaultReport{Armed: v.fault != nil, Seen: v.faultSeen}
	if v.fault != nil {
		report.Fault = *v.fault
	}
	if v.fired != nil {
		report.Fired = true
		report.Call = *v.fired
	}
	v.fault = nil
	v.faultSeen = 0
	v.fired = nil
	return report
}

var faultable = map[string]bool{"xRead": true, "xWrite": true, "xSync": true, "xTruncate": true}

// Err returns the first internal self-check error encountered when wrapping a
// file's sqlite3_io_methods table during xOpen, or nil if none occurred.
func (v *VFS) Err() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.err
}

func (v *VFS) setError(err error) {
	if v == nil {
		return
	}
	v.mu.Lock()
	if v.err == nil {
		v.err = err
	}
	v.mu.Unlock()
}

func (v *VFS) injected(c Call, code int32) int32 {
	c.Result = code
	c.Injected = true
	v.record(c)
	return code
}

func (v *VFS) inject(method string, kind FileKind) (int32, bool) {
	if v == nil {
		return 0, false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	f := v.fault
	if f == nil || v.fired != nil || f.Method != method || (f.Kind != KindNone && f.Kind != kind) {
		return 0, false
	}
	v.faultSeen++
	if v.faultSeen != f.Nth {
		return 0, false
	}
	v.fired = &Call{}
	return f.Code, true
}

func (v *VFS) record(c Call) {
	if v == nil {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if c.Injected && v.fired != nil {
		v.seq++
		c.Seq = v.seq
		*v.fired = c
		if v.recording {
			v.calls = append(v.calls, c)
		}
		return
	}
	if !v.recording {
		return
	}
	v.seq++
	c.Seq = v.seq
	v.calls = append(v.calls, c)
}

func vfsFor(pVfs uintptr) *VFS {
	registry.RLock()
	defer registry.RUnlock()
	return registry.byAddress[pVfs]
}

func realVFS() *sqlite3.Tsqlite3_vfs {
	return at[sqlite3.Tsqlite3_vfs](registry.realVFS)
}

func fileFor(pFile uintptr) *openFile {
	registry.RLock()
	defer registry.RUnlock()
	return registry.files[pFile]
}

// inner advances past the 8-byte shim sqlite3_file header to the embedded real
// VFS sqlite3_file struct.
func inner(pFile uintptr) uintptr {
	return pFile + headerSize
}

func realMethods(f *openFile) *sqlite3.Tsqlite3_io_methods {
	return at[sqlite3.Tsqlite3_io_methods](f.realMethods)
}

func displayName(path string) string {
	if path == "" {
		return "<temp>"
	}
	return filepath.Base(path)
}

// methodTableFor returns a cached shim sqlite3_io_methods table allocated in
// SQLite memory for the (shim VFS, real method table) pair. Its iVersion is
// capped at min(v.ioVersion, realTable.FiVersion) and null slots stay null,
// matching vfstraceOpen's pattern (vfstrace.c:894-922).
func methodTableFor(v *VFS, real uintptr) (uintptr, error) {
	key := [2]uintptr{v.address, real}
	registry.RLock()
	table, ok := registry.tables[key]
	registry.RUnlock()
	if ok {
		return table.address, nil
	}
	if err := checkRealMethods(real); err != nil {
		return 0, err
	}
	realTable := at[sqlite3.Tsqlite3_io_methods](real)
	version := min(v.ioVersion, realTable.FiVersion)
	tls := libc.NewTLS()
	defer tls.Close()
	address := sqlite3.Xsqlite3_malloc64(tls, uint64(unsafe.Sizeof(sqlite3.Tsqlite3_io_methods{})))
	if address == 0 {
		return 0, errors.New("govfs: out of memory")
	}
	shim := at[sqlite3.Tsqlite3_io_methods](address)
	*shim = sqlite3.Tsqlite3_io_methods{FiVersion: version}
	for _, slot := range ioSlots {
		if slot.MinVersion > version || *slot.field(realTable) == 0 {
			continue
		}
		*slot.field(shim) = slot.wrapper
	}
	registry.Lock()
	defer registry.Unlock()
	if existing, ok := registry.tables[key]; ok {
		return existing.address, nil
	}
	registry.tables[key] = methodTable{address: address, version: version}
	return address, nil
}
