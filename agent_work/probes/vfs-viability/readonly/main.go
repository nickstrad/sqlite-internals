package main

import (
	"bytes"
	"database/sql"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing/fstest"
	"time"

	_ "modernc.org/sqlite"
	"modernc.org/sqlite/vfs"
)

type readLog struct {
	mu    sync.Mutex
	reads []string
}

func (l *readLog) add(format string, args ...any) {
	l.mu.Lock()
	l.reads = append(l.reads, fmt.Sprintf(format, args...))
	l.mu.Unlock()
}

type loggingFS struct {
	inner fs.FS
	log   *readLog
}

type loggingFile struct {
	fs.File
	name   string
	log    *readLog
	offset int64
}

func (f loggingFS) Open(name string) (fs.File, error) {
	file, err := f.inner.Open(name)
	f.log.add("Open %s err=%v", name, err != nil)
	if err != nil {
		return nil, err
	}
	return &loggingFile{File: file, name: name, log: f.log}, nil
}

func (f *loggingFile) Seek(offset int64, whence int) (int64, error) {
	position, err := f.File.(io.Seeker).Seek(offset, whence)
	f.offset = position
	return position, err
}

func (f *loggingFile) Read(p []byte) (int, error) {
	n, err := f.File.Read(p)
	f.log.add("Read %s ofst=%d n=%d got=%d", f.name, f.offset, len(p), n)
	f.offset += int64(n)
	return n, err
}

type httpFS struct {
	base     string
	requests *int
	mu       *sync.Mutex
}

type httpFile struct {
	fs     httpFS
	name   string
	size   int64
	offset int64
}

type httpInfo struct {
	name string
	size int64
}

func (i httpInfo) Name() string       { return i.name }
func (i httpInfo) Size() int64        { return i.size }
func (i httpInfo) Mode() fs.FileMode  { return 0o444 }
func (i httpInfo) ModTime() time.Time { return time.Time{} }
func (i httpInfo) IsDir() bool        { return false }
func (i httpInfo) Sys() any           { return nil }

func (h httpFS) count() {
	h.mu.Lock()
	*h.requests++
	h.mu.Unlock()
}

func (h httpFS) Open(name string) (fs.File, error) {
	h.count()
	response, err := http.Head(h.base + "/" + name)
	if err != nil {
		return nil, err
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fs.ErrNotExist
	}
	return &httpFile{fs: h, name: name, size: response.ContentLength}, nil
}

func (f *httpFile) Stat() (fs.FileInfo, error) { return httpInfo{f.name, f.size}, nil }
func (f *httpFile) Close() error               { return nil }

func (f *httpFile) Seek(offset int64, whence int) (int64, error) {
	if whence != io.SeekStart {
		return 0, fmt.Errorf("unsupported whence %d", whence)
	}
	f.offset = offset
	return offset, nil
}

func (f *httpFile) Read(p []byte) (int, error) {
	if f.offset >= f.size {
		return 0, io.EOF
	}
	f.fs.count()
	request, _ := http.NewRequest(http.MethodGet, f.fs.base+"/"+f.name, nil)
	request.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", f.offset, f.offset+int64(len(p))-1))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("range GET status %d", response.StatusCode)
	}
	n, err := io.ReadFull(response.Body, p[:min(int64(len(p)), f.size-f.offset)])
	f.offset += int64(n)
	if err == io.ErrUnexpectedEOF {
		err = nil
	}
	return n, err
}

func build(path, journal string, rows int, checkpoint bool) error {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode("+journal+")&_pragma=wal_autocheckpoint(0)")
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT); CREATE INDEX t_v ON t(v)"); err != nil {
		return err
	}
	for i := 1; i <= rows; i++ {
		if _, err := db.Exec("INSERT INTO t VALUES(?, ?)", i, fmt.Sprintf("value-%05d-%s", i, strings.Repeat("x", 200))); err != nil {
			return err
		}
	}
	if checkpoint {
		if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
			return err
		}
		return db.Close()
	}
	return db.Close()
}

func query(name, dsnSuffix string, fsys fs.FS, sqlText string) (string, error) {
	vfsName, handle, err := vfs.New(fsys)
	if err != nil {
		return "", err
	}
	defer handle.Close()
	db, err := sql.Open("sqlite", "file:"+name+"?vfs="+vfsName+dsnSuffix)
	if err != nil {
		return "", err
	}
	defer db.Close()
	var result string
	err = db.QueryRow(sqlText).Scan(&result)
	return result, err
}

func main() {
	dir, err := os.MkdirTemp("", "vfs-readonly-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	rollback := filepath.Join(dir, "rollback.db")
	if err := build(rollback, "delete", 500, false); err != nil {
		panic(err)
	}
	walClean := filepath.Join(dir, "walclean.db")
	if err := build(walClean, "wal", 500, true); err != nil {
		panic(err)
	}
	info, _ := os.Stat(rollback)
	fmt.Printf("rollback.db size=%d pages=%d\n", info.Size(), info.Size()/4096)

	log := &readLog{}
	result, err := query("rollback.db", "", loggingFS{inner: os.DirFS(dir), log: log}, "SELECT v FROM t WHERE id=377")
	fmt.Printf("logging os.DirFS point lookup: result_prefix=%q err=%v reads=%d\n", prefix(result), err, len(log.reads))
	for _, line := range log.reads {
		fmt.Println("   ", line)
	}
	log.reads = nil
	result, err = query("rollback.db", "", loggingFS{inner: os.DirFS(dir), log: log}, "SELECT count(*) FROM t")
	fmt.Printf("logging os.DirFS full count: result=%s err=%v reads=%d\n", result, err, len(log.reads))

	image, _ := os.ReadFile(rollback)
	memory := fstest.MapFS{"mem.db": &fstest.MapFile{Data: image}}
	result, err = query("mem.db", "", memory, "PRAGMA integrity_check")
	fmt.Printf("fstest.MapFS in-memory: integrity=%s err=%v\n", result, err)

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := os.ReadFile(filepath.Join(dir, filepath.Base(r.URL.Path)))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, r.URL.Path, time.Time{}, bytes.NewReader(data))
	}))
	defer server.Close()
	remote := httpFS{base: server.URL, requests: &requests, mu: &sync.Mutex{}}
	result, err = query("rollback.db", "", remote, "SELECT v FROM t WHERE id=377")
	fmt.Printf("HTTP range reader point lookup: result_prefix=%q err=%v http_requests=%d\n", prefix(result), err, requests)
	requests = 0
	result, err = query("rollback.db", "&_pragma=cache_size(-64000)", remote, "SELECT count(*) FROM t")
	fmt.Printf("HTTP range reader full count: result=%s err=%v http_requests=%d\n", result, err, requests)

	header := make([]byte, 20)
	file, _ := os.Open(walClean)
	file.Read(header)
	file.Close()
	fmt.Printf("walclean.db header bytes 18,19 = %d,%d (2,2 = WAL)\n", header[18], header[19])
	for _, suffix := range []string{"", "&mode=ro", "&immutable=1", "&mode=ro&immutable=1"} {
		result, err = query("walclean.db", suffix, os.DirFS(dir), "SELECT count(*) FROM t")
		fmt.Printf("WAL-format file via fs VFS dsn=%q: result=%q err=%v\n", suffix, result, err)
	}
	patched, _ := os.ReadFile(walClean)
	patched[18], patched[19] = 1, 1
	result, err = query("patched.db", "", fstest.MapFS{"patched.db": &fstest.MapFile{Data: patched}}, "SELECT count(*) FROM t")
	fmt.Printf("WAL-format file with header bytes 18,19 rewritten to 1,1: result=%q err=%v\n", result, err)

	walDirty := filepath.Join(dir, "waldirty.db")
	if err := build(walDirty, "wal", 50, true); err != nil {
		panic(err)
	}
	writer, _ := sql.Open("sqlite", "file:"+walDirty+"?_pragma=wal_autocheckpoint(0)")
	writer.SetMaxOpenConns(1)
	writer.Exec("INSERT INTO t VALUES(1001, 'only-in-wal')")
	walInfo, _ := os.Stat(walDirty + "-wal")
	fmt.Printf("waldirty.db: 50 rows checkpointed, 1 row only in the -wal (%d bytes), writer still open\n", walInfo.Size())
	for _, suffix := range []string{"&immutable=1"} {
		result, err = query("waldirty.db", suffix, os.DirFS(dir), "SELECT count(*) FROM t")
		fmt.Printf("WAL with uncheckpointed frames via fs VFS dsn=%q: result=%q err=%v\n", suffix, result, err)
	}
	writer.Close()
	_, err = query("rollback.db", "", os.DirFS(dir), "SELECT 1")
	fmt.Printf("sanity SELECT 1: err=%v\n", err)
	vfsName, handle, _ := vfs.New(os.DirFS(dir))
	db, _ := sql.Open("sqlite", "file:rollback.db?vfs="+vfsName)
	_, err = db.Exec("INSERT INTO t VALUES(9999, 'x')")
	fmt.Printf("write through fs VFS: err=%v\n", err)
	db.Close()
	handle.Close()
}

func prefix(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
