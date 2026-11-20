// A logging VFS written in pure Go on ncruces/go-sqlite3: wraps the built-in "os" VFS (ncruces's
// own Go reimplementation of the OS layer, NOT os_unix.c) and logs every call it can see, for:
// open, journal_mode=WAL, create, insert-commit, checkpoint, close.
// What it cannot see: xShmMap/xShmLock/xShmUnmap/xShmBarrier (vfs.SharedMemory has unexported
// methods, so a wrapper can only forward the inner object) and file-controls without an
// exported optional interface.
package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/ncruces/go-sqlite3"
	"github.com/ncruces/go-sqlite3/util/vfsutil"
	"github.com/ncruces/go-sqlite3/vfs"
)

var (
	phase = "setup"
	dir   string
)

func logf(format string, args ...any) {
	fmt.Printf("%-10s %s\n", phase, strings.ReplaceAll(fmt.Sprintf(format, args...), dir+"/", ""))
}

type logVFS struct{ inner vfs.VFS }

func (v logVFS) Open(name string, flags vfs.OpenFlag) (vfs.File, vfs.OpenFlag, error) {
	f, out, err := vfsutil.WrapOpen(v.inner, name, flags)
	logf("xOpen(%q, flags=0x%x) -> err=%v", name, flags, err)
	if err != nil {
		return nil, out, err
	}
	return &logFile{File: f, name: filepath.Base(name)}, out, nil
}

// OpenFilename is required: ncruces's "os" VFS opens main databases through vfs.VFSFilename
// (plain Open of the main db fails with "unable to open database file").
func (v logVFS) OpenFilename(name *vfs.Filename, flags vfs.OpenFlag) (vfs.File, vfs.OpenFlag, error) {
	f, out, err := vfsutil.WrapOpenFilename(v.inner, name, flags)
	n := ""
	if name != nil {
		n = name.String()
	}
	logf("xOpen(%q, flags=0x%x) -> err=%v", n, flags, err)
	if err != nil {
		return nil, out, err
	}
	return &logFile{File: f, name: filepath.Base(n)}, out, nil
}

func (v logVFS) Delete(name string, syncDir bool) error {
	err := v.inner.Delete(name, syncDir)
	logf("xDelete(%q, syncDir=%v) -> %v", name, syncDir, err)
	return err
}
func (v logVFS) Access(name string, flags vfs.AccessFlag) (bool, error) {
	ok, err := v.inner.Access(name, flags)
	logf("xAccess(%q, %d) -> %v %v", name, flags, ok, err)
	return ok, err
}
func (v logVFS) FullPathname(name string) (string, error) { return v.inner.FullPathname(name) }

type logFile struct {
	vfs.File
	name string
}

func (f *logFile) ReadAt(p []byte, off int64) (int, error) {
	n, err := f.File.ReadAt(p, off)
	logf("xRead(%s, n=%d, ofst=%d) -> %d %v", f.name, len(p), off, n, err)
	return n, err
}
func (f *logFile) WriteAt(p []byte, off int64) (int, error) {
	n, err := f.File.WriteAt(p, off)
	logf("xWrite(%s, n=%d, ofst=%d) -> %v", f.name, len(p), off, err)
	return n, err
}
func (f *logFile) Truncate(size int64) error {
	err := f.File.Truncate(size)
	logf("xTruncate(%s, %d) -> %v", f.name, size, err)
	return err
}
func (f *logFile) Sync(flags vfs.SyncFlag) error {
	err := f.File.Sync(flags)
	logf("xSync(%s, flags=0x%x) -> %v", f.name, flags, err)
	return err
}
func (f *logFile) Lock(l vfs.LockLevel) error {
	err := f.File.Lock(l)
	logf("xLock(%s, %v) -> %v", f.name, l, err)
	return err
}
func (f *logFile) Unlock(l vfs.LockLevel) error {
	err := f.File.Unlock(l)
	logf("xUnlock(%s, %v) -> %v", f.name, l, err)
	return err
}
func (f *logFile) Close() error {
	err := f.File.Close()
	logf("xClose(%s) -> %v", f.name, err)
	return err
}

// Optional interfaces: forward so WAL, checkpoints and PERSIST_WAL keep working, and log the
// file-controls that have an exported interface.
func (f *logFile) Unwrap() vfs.File { return f.File }
func (f *logFile) SharedMemory() vfs.SharedMemory {
	logf("SharedMemory(%s) requested (xShmMap/xShmLock happen inside it, invisible here)", f.name)
	return vfsutil.WrapSharedMemory(f.File)
}
func (f *logFile) LockState() vfs.LockLevel { return vfsutil.WrapLockState(f.File) }
func (f *logFile) PersistWAL() bool         { return vfsutil.WrapPersistWAL(f.File) }
func (f *logFile) SetPersistWAL(b bool)     { vfsutil.WrapSetPersistWAL(f.File, b) }
func (f *logFile) HasMoved() (bool, error)  { return vfsutil.WrapHasMoved(f.File) }
func (f *logFile) SizeHint(n int64) error   { return vfsutil.WrapSizeHint(f.File, n) }
func (f *logFile) BusyHandler(h func() bool) { vfsutil.WrapBusyHandler(f.File, h) }
func (f *logFile) CheckpointStart() {
	logf("xFileControl(%s, CKPT_START)", f.name)
	vfsutil.WrapCheckpointStart(f.File)
}
func (f *logFile) CheckpointDone() {
	logf("xFileControl(%s, CKPT_DONE)", f.name)
	vfsutil.WrapCheckpointDone(f.File)
}
func (f *logFile) CommitPhaseTwo() error {
	logf("xFileControl(%s, COMMIT_PHASETWO)", f.name)
	return vfsutil.WrapCommitPhaseTwo(f.File)
}

func main() {
	dir, _ = os.MkdirTemp("", "ncruceslog")
	defer os.RemoveAll(dir)
	fmt.Println("SupportsSharedMemory:", vfs.SupportsSharedMemory, "SupportsFileLocking:", vfs.SupportsFileLocking)
	vfs.Register("log", logVFS{vfs.Find("os")})
	db, err := sqlite3.OpenFlags("file:"+filepath.Join(dir, "t.db")+"?vfs=log", sqlite3.OPEN_READWRITE|sqlite3.OPEN_CREATE|sqlite3.OPEN_URI)
	if err != nil {
		log.Fatal(err)
	}
	step := func(p, sql string) {
		phase = p
		if err := db.Exec(sql); err != nil {
			log.Fatalf("%s: %v", p, err)
		}
	}
	step("wal", "pragma journal_mode=wal")
	step("create", "create table t(x)")
	step("insert", "insert into t values(randomblob(100))")
	phase = "checkpoint"
	nLog, nCkpt, err := db.WALCheckpoint("main", sqlite3.CHECKPOINT_PASSIVE)
	fmt.Printf("checkpoint nLog=%d nCkpt=%d err=%v\n", nLog, nCkpt, err)
	phase = "close"
	db.Close()
	m, _ := sqlite3.Open(":memory:")
	st, _, _ := m.Prepare("select sqlite_source_id()")
	st.Step()
	fmt.Println("source_id:", st.ColumnText(0))
	st.Close()
	m.Close()
}
