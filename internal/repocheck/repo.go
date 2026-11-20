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

	"github.com/nickstrad/sqlite-internals/sqlite"
)

type Problem struct {
	Where   string
	Message string
}

func (p Problem) String() string {
	if p.Where == "" {
		return p.Message
	}
	return p.Where + ": " + p.Message
}

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

type Chapter struct {
	Number string
	Dir    string
	Path   string
}

func (c Chapter) Markdown() string {
	return filepath.Join(c.Path, c.Dir+".md")
}

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

const SourceTreeEnv = "SQLITE_SRC"

func PinnedTreeCandidates(root string) []string {
	var candidates []string
	if value := os.Getenv(SourceTreeEnv); value != "" {
		candidates = append(candidates, value)
	}
	return append(candidates, filepath.Join(root, "_src", sqlite.Current().SourceDirName()))
}

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
