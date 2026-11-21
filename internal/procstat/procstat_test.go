package procstat

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestDescriptorCountSeesOpenAndClose confirms DescriptorCount tracks three newly created files and returns to baseline on close.
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

// TestDescriptorCountExcludesItsOwnDirectory checks that consecutive counts are identical and match len(Descriptors()).
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

// TestDescriptorsUnder verifies DescriptorsUnder filters to dir, excludes a sibling prefix directory, and returns ErrTargetsUnsupported off Linux.
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

// TestDescriptorsUnderRelativeAndRootDirectories checks DescriptorsUnder and RelativeTargets with relative paths and "/".
func TestDescriptorsUnderRelativeAndRootDirectories(t *testing.T) {
	if !TargetsSupported {
		t.Skip("descriptor targets are Linux-only")
	}
	base := t.TempDir()
	t.Chdir(base)
	if err := os.MkdirAll(filepath.Join("rel", "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join("rel", "inner", "probe.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, dir := range []string{"rel", "./rel/", filepath.Join("rel", "inner", "..")} {
		under, err := DescriptorsUnder(dir)
		if err != nil {
			t.Fatal(err)
		}
		got := RelativeTargets(dir, under)
		if len(got) != 1 || got[0] != filepath.Join("inner", "probe.txt") {
			t.Fatalf("DescriptorsUnder(%q): got %v, want [inner/probe.txt]", dir, got)
		}
	}
	root, err := DescriptorsUnder("/")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range root {
		if strings.HasSuffix(d.Target, filepath.Join("rel", "inner", "probe.txt")) {
			found = true
		}
	}
	if !found {
		t.Fatalf("DescriptorsUnder(\"/\") missed the open probe file: %v", root)
	}
}

// TestReadMemory verifies ReadMemory returns a positive RSS and includes the Linux breakdown.
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

// TestParseStatus verifies valid, partial, malformed, duplicate, and int64-overflowing /proc/self/status inputs.
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
		"VmRSS: 9007199254740992 kB\n",
		"VmRSS: 4 kB\nRssAnon: 18014398509481984 kB\n",
	} {
		if _, err := ParseStatus(bad); err == nil {
			t.Errorf("ParseStatus(%q) accepted malformed input", bad)
		}
	}
}

// TestParsePSRSS checks KiB-to-bytes conversion and rejection of malformed or overflowing ps output.
func TestParsePSRSS(t *testing.T) {
	m, err := ParsePSRSS("  12345\n")
	if err != nil {
		t.Fatal(err)
	}
	if m.RSS != 12345*1024 || m.HasBreakdown() {
		t.Fatalf("got %+v", m)
	}
	for _, bad := range []string{"", "\n", "abc", "12 34", "-5", "1.5", "9007199254740992", "18014398509481984"} {
		if _, err := ParsePSRSS(bad); err == nil {
			t.Errorf("ParsePSRSS(%q) accepted malformed input", bad)
		}
	}
}

// TestFileLimit verifies FileLimit returns a nonzero soft RLIMIT_NOFILE that does not exceed the hard limit.
func TestFileLimit(t *testing.T) {
	soft, hard, err := FileLimit()
	if err != nil {
		t.Fatal(err)
	}
	if soft == 0 || soft > hard {
		t.Fatalf("soft %d hard %d", soft, hard)
	}
}

// TestDescriptorDirMatchesPlatform confirms DescriptorDir selects /proc/self/fd on Linux and /dev/fd elsewhere.
func TestDescriptorDirMatchesPlatform(t *testing.T) {
	if runtime.GOOS == "linux" && DescriptorDir != "/proc/self/fd" {
		t.Fatal(DescriptorDir)
	}
	if runtime.GOOS != "linux" && !strings.HasPrefix(DescriptorDir, "/dev/fd") {
		t.Fatal(DescriptorDir)
	}
}
