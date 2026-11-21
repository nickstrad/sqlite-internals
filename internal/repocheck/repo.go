// Package repocheck implements the repository verification checks invoked by
// cmd/course: copied SQLite sources, cited line spans, inlined code blocks,
// comment-only diffs, prose authorship, experiment evidence, gofmt, and
// machine-local path sweeps.
package repocheck

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/nickstrad/walfleet/sqlite"
)

// Problem records a single check failure with an optional "file:line" location.
type Problem struct {
	Where   string
	Message string
}

// String formats p as "Where: Message", or just Message when Where is empty.
func (p Problem) String() string {
	if p.Where == "" {
		return p.Message
	}
	return p.Where + ": " + p.Message
}

// Git runs a git subcommand inside dir and returns its stdout.
func Git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// TrackedFiles returns every git-tracked path under root using NUL-delimited output.
func TrackedFiles(root string) ([]string, error) {
	out, err := Git(root, "ls-files", "-z")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, name := range strings.Split(out, "\x00") {
		if name != "" {
			files = append(files, name)
		}
	}
	return files, nil
}

// FilesAtRevision lists all tracked file paths present in the git tree at revision.
func FilesAtRevision(root, revision string) ([]string, error) {
	out, err := Git(root, "ls-tree", "-r", "-z", "--name-only", revision)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, name := range strings.Split(out, "\x00") {
		if name != "" {
			files = append(files, name)
		}
	}
	return files, nil
}

// FileAtRevision returns the raw contents of path at the given git revision.
func FileAtRevision(root, revision, path string) ([]byte, error) {
	out, err := Git(root, "show", revision+":"+path)
	return []byte(out), err
}

func hasPathPrefix(path string, prefixes []string) bool {
	for _, prefix := range prefixes {
		prefix = strings.TrimSuffix(prefix, "/")
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

var chapterDirPattern = regexp.MustCompile(`^ch(\d\d)-[a-z0-9-]+$`)

// Chapter identifies one chapter directory (chNN-slug) at the repository root.
type Chapter struct {
	Number string
	Dir    string
	Path   string
}

// Markdown returns the path to the chapter's main markdown file (<dir>/<dir>.md).
func (c Chapter) Markdown() string {
	return filepath.Join(c.Path, c.Dir+".md")
}

// Chapters discovers chapter directories under root, sorted by directory name.
// When only is non-empty (such as "ch01" or the full directory name), only the
// matching chapter is returned and a missing match is an error.
func Chapters(root, only string) ([]Chapter, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var chapters []Chapter
	for _, entry := range entries {
		m := chapterDirPattern.FindStringSubmatch(entry.Name())
		if !entry.IsDir() || m == nil {
			continue
		}
		if only != "" && only != "ch"+m[1] && only != entry.Name() {
			continue
		}
		chapters = append(chapters, Chapter{Number: m[1], Dir: entry.Name(), Path: filepath.Join(root, entry.Name())})
	}
	sort.Slice(chapters, func(i, j int) bool { return chapters[i].Dir < chapters[j].Dir })
	if only != "" && len(chapters) == 0 {
		return nil, fmt.Errorf("no chapter directory matches %q", only)
	}
	return chapters, nil
}

// SourceTreeEnv names the environment variable pointing to a local checkout of
// the pinned SQLite source tree ($SQLITE_SRC).
const SourceTreeEnv = "SQLITE_SRC"

// PinnedTreeCandidates returns the search paths for the pinned SQLite source
// tree: $SQLITE_SRC (when set) followed by <root>/_src/sqlite-<release>.
func PinnedTreeCandidates(root string) []string {
	var candidates []string
	if value := os.Getenv(SourceTreeEnv); value != "" {
		candidates = append(candidates, value)
	}
	return append(candidates, filepath.Join(root, "_src", sqlite.Current().SourceDirName()))
}

// PinnedTree finds the first existing candidate directory containing
// manifest.uuid and verifies that it matches sqlite/PIN.
func PinnedTree(root string) (string, error) {
	for _, candidate := range PinnedTreeCandidates(root) {
		if _, err := os.Stat(filepath.Join(candidate, "manifest.uuid")); err != nil {
			continue
		}
		if err := VerifyTree(candidate); err != nil {
			return "", err
		}
		return candidate, nil
	}
	return "", errors.New("pinned SQLite tree not found ($SQLITE_SRC or _src/" + sqlite.Current().SourceDirName() + "); run: go run ./cmd/course fetch-source")
}

// VerifyTree checks that dir/manifest.uuid equals sqlite.Current().FossilID.
func VerifyTree(dir string) error {
	data, err := os.ReadFile(filepath.Join(dir, "manifest.uuid"))
	if err != nil {
		return err
	}
	got := strings.TrimSpace(string(data))
	if want := sqlite.Current().FossilID; got != want {
		return fmt.Errorf("%s/manifest.uuid is %s, the pin is %s", dir, got, want)
	}
	return nil
}
