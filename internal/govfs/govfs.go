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

const headerSize = 8

type Options struct {
	IOVersion   int32
	MakeDefault bool
}

type Fault struct {
	Method string
	Nth    int
	Code   int32
	Kind   FileKind
}

type FaultReport struct {
	Armed bool
	Fault Fault
	Seen  int
	Fired bool
	Call  Call
}

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

var ErrAlreadyRegistered = errors.New("a VFS with this name is already registered")

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

func Lookup(name string) *VFS {
	registry.RLock()
	defer registry.RUnlock()
	return registry.byName[name]
}

func (v *VFS) Name() string {
	return v.name
}

func (v *VFS) IOVersion() int32 {
	return v.ioVersion
}

func (v *VFS) SetRecording(on bool) {
	v.mu.Lock()
	v.recording = on
	v.mu.Unlock()
}

func (v *VFS) Reset() {
	v.mu.Lock()
	v.calls = nil
	v.seq = 0
	v.mu.Unlock()
}

func (v *VFS) Calls() []Call {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]Call(nil), v.calls...)
}

func (v *VFS) TakeCalls() []Call {
	v.mu.Lock()
	defer v.mu.Unlock()
	calls := v.calls
	v.calls = nil
	v.seq = 0
	return calls
}

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
