package bounds

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLimits(t *testing.T) {
	b := Bounds{OpenDatabases: 4, ConnectionsPerDatabase: 3, CacheBytes: CacheBytes(-256, 4096), ReserveDescriptors: 32}
	if b.DescriptorLimit() != 68 {
		t.Fatal(b.DescriptorLimit())
	}
	if want := int64(12*(2*256*1024+256*1024) + 32<<20); b.MemoryLimit() != want {
		t.Fatal(b.MemoryLimit(), want)
	}
	if CacheBytes(100, 4096) != 409600 {
		t.Fatal(CacheBytes(100, 4096))
	}
}

func TestCheckFailsWhenDescriptorsExceedTheBound(t *testing.T) {
	a, err := Start(Bounds{OpenDatabases: 1, ConnectionsPerDatabase: 1, ReserveDescriptors: 0})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Check(); err != nil {
		t.Fatalf("fresh assertion failed: %v", err)
	}
	var files []*os.File
	for i := range 4 {
		f, err := os.Create(filepath.Join(t.TempDir(), "f"+string(rune('a'+i))))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	sample, err := a.Check()
	if !errors.Is(err, ErrExceeded) || sample.Descriptors < 4 || a.Peak.Descriptors < 4 || a.Checks != 2 {
		t.Fatalf("%+v %v", sample, err)
	}
	for _, f := range files {
		f.Close()
	}
	if _, err := a.Check(); err != nil {
		t.Fatalf("after closing: %v", err)
	}
}
