package procstat

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDescriptorCountSeesOpenAndClose(t *testing.T) {
	before, err := DescriptorCount()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	var files []*os.File
	for i := range 3 {
		f, err := os.Create(filepath.Join(dir, "f"+string(rune('a'+i))))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	during, err := DescriptorCount()
	if err != nil {
		t.Fatal(err)
	}
	if during != before+3 {
		t.Fatalf("with 3 files open: got %d descriptors, want %d", during, before+3)
	}
	for _, f := range files {
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	after, err := DescriptorCount()
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("after closing: got %d descriptors, want %d", after, before)
	}
}

func TestDescriptorCountExcludesItsOwnDirectory(t *testing.T) {
	first, err := DescriptorCount()
	if err != nil {
		t.Fatal(err)
	}
	second, err := DescriptorCount()
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("two counts in a row differ: %d then %d", first, second)
	}
	descriptors, err := Descriptors()
	if err != nil {
		t.Fatal(err)
	}
	if len(descriptors) != second {
		t.Fatalf("Descriptors lists %d entries, DescriptorCount says %d", len(descriptors), second)
	}
}

func TestDescriptorsUnder(t *testing.T) {
	dir := t.TempDir()
	if !TargetsSupported {
		if _, err := DescriptorsUnder(dir); err != ErrTargetsUnsupported {
			t.Fatalf("got %v, want ErrTargetsUnsupported", err)
		}
		return
	}
	f, err := os.Create(filepath.Join(dir, "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sibling := dir + "-sibling"
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sibling)
	g, err := os.Create(filepath.Join(sibling, "y.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	under, err := DescriptorsUnder(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := RelativeTargets(dir, under)
	if len(got) != 1 || got[0] != "x.db" {
		t.Fatalf("got %v, want [x.db]", got)
	}
}

func TestReadMemory(t *testing.T) {
	m, err := ReadMemory()
	if err != nil {
		t.Fatal(err)
	}
	if m.RSS <= 0 {
		t.Fatalf("RSS %d", m.RSS)
	}
	if runtime.GOOS == "linux" && !m.HasBreakdown() {
		t.Fatalf("linux memory without breakdown: %+v", m)
	}
}

func TestParseStatus(t *testing.T) {
	text := "Name:\tx\nVmRSS:\t   2048 kB\nRssAnon:\t  1024 kB\nRssFile:\t   1000 kB\nRssShmem:\t  24 kB\n"
	m, err := ParseStatus(text)
	if err != nil {
		t.Fatal(err)
	}
	want := Memory{RSS: 2048 * 1024, Anon: 1024 * 1024, File: 1000 * 1024, Shmem: 24 * 1024}
	if m != want {
		t.Fatalf("got %+v want %+v", m, want)
	}
	partial, err := ParseStatus("VmRSS: 4 kB\n")
	if err != nil {
		t.Fatal(err)
	}
	if partial.RSS != 4096 || partial.HasBreakdown() {
		t.Fatalf("partial: %+v", partial)
	}
	for _, bad := range []string{
		"",
		"Name: x\n",
		"VmRSS: x kB\n",
		"VmRSS: 4 MB\n",
		"VmRSS: -4 kB\n",
		"VmRSS:\n",
		"VmRSS: 4 kB\nVmRSS: 5 kB\n",
		"VmRSS: 4 kB\nRssAnon: four kB\n",
	} {
		if _, err := ParseStatus(bad); err == nil {
			t.Errorf("ParseStatus(%q) accepted malformed input", bad)
		}
	}
}

func TestParsePSRSS(t *testing.T) {
	m, err := ParsePSRSS("  12345\n")
	if err != nil {
		t.Fatal(err)
	}
	if m.RSS != 12345*1024 || m.HasBreakdown() {
		t.Fatalf("got %+v", m)
	}
	for _, bad := range []string{"", "\n", "abc", "12 34", "-5", "1.5"} {
		if _, err := ParsePSRSS(bad); err == nil {
			t.Errorf("ParsePSRSS(%q) accepted malformed input", bad)
		}
	}
}

func TestFileLimit(t *testing.T) {
	soft, hard, err := FileLimit()
	if err != nil {
		t.Fatal(err)
	}
	if soft == 0 || soft > hard {
		t.Fatalf("soft %d hard %d", soft, hard)
	}
}

func TestDescriptorDirMatchesPlatform(t *testing.T) {
	if runtime.GOOS == "linux" && DescriptorDir != "/proc/self/fd" {
		t.Fatal(DescriptorDir)
	}
	if runtime.GOOS != "linux" && !strings.HasPrefix(DescriptorDir, "/dev/fd") {
		t.Fatal(DescriptorDir)
	}
}
