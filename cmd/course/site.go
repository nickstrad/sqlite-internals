package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickstrad/sqlite-internals/internal/repocheck"
	"github.com/nickstrad/sqlite-internals/internal/site"
)

const (
	defaultRepoURL = "https://github.com/nickstrad/sqlite-internals"
	defaultBranch  = "main"
)

// runSite implements "course site" to render the static book site or run the
// temporary-directory link and anchor check with -check.
func runSite(root string, args []string) error {
	flags := flag.NewFlagSet("site", flag.ContinueOnError)
	out := flags.String("out", "_site", "output directory")
	base := flags.String("base", "/", "URL path the site is served under")
	repoURL := flags.String("repo", defaultRepoURL, "GitHub repository URL for code and evidence links")
	branch := flags.String("branch", defaultBranch, "branch used in GitHub links")
	check := flags.Bool("check", false, "render to a temporary directory and fail on any broken internal link or missing anchor")
	if err := flags.Parse(args); err != nil {
		return exitError{2}
	}
	if flags.NArg() != 0 {
		return usageError("site takes no positional arguments")
	}
	files, err := siteFiles(root)
	if err != nil {
		return err
	}
	opts := site.Options{Root: root, Files: files, Base: *base, RepoURL: *repoURL, Branch: *branch}
	if *check {
		return checkSite(opts)
	}
	dir, err := prepareSiteDir(root, *out)
	if err != nil {
		return err
	}
	result, err := site.Build(opts, dir)
	if err != nil {
		return err
	}
	for _, problem := range result.Problems {
		fmt.Printf("WARN site: %s\n", problem)
	}
	fmt.Printf("PASS site: %d page(s) written to %s (base %s)\n", len(result.Pages), *out, site.NormalizeBase(*base))
	return nil
}

// checkSite builds the site into a disposable temporary directory, runs
// site.CheckSite over the rendered HTML, and reports combined build and HTML
// link problems.
func checkSite(opts site.Options) error {
	dir, err := os.MkdirTemp("", "course-site-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	built, err := site.Build(opts, dir)
	if err != nil {
		return err
	}
	checked, err := site.CheckSite(dir, opts.Base)
	if err != nil {
		return err
	}
	var r report
	var problems []repocheck.Problem
	for _, problem := range append(built.Problems, checked.Problems...) {
		problems = append(problems, repocheck.Problem{Where: problem.Where, Message: problem.Message})
	}
	r.result("site -check", problems, fmt.Sprintf("%d page(s), %d link(s) resolve", checked.Pages, checked.Links))
	return r.exit()
}

// siteFiles returns tracked and untracked non-ignored paths that exist on disk
// under root, matching the file set visible on GitHub while omitting ignored
// directories such as _src/ and _work/.
func siteFiles(root string) ([]string, error) {
	out, err := repocheck.Git(root, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var files []string
	for _, name := range strings.Split(out, "\x00") {
		if name == "" || seen[name] {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(name))); err != nil {
			continue
		}
		seen[name] = true
		files = append(files, name)
	}
	sort.Strings(files)
	return files, nil
}

// prepareSiteDir validates and prepares out as the site build destination.
// It refuses to overwrite any directory containing root (including through a
// symlink) and refuses to replace a non-empty directory that lacks a .nojekyll
// marker from a previous site build.
func prepareSiteDir(root, out string) (string, error) {
	dir, err := filepath.Abs(out)
	if err != nil {
		return "", err
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	// Resolve symlinks on the longest existing prefix and compare against root's
	// ancestors by inode so a symlinked -out path cannot wipe the repository.
	if containsPath(resolveExisting(dir), resolveExisting(absRoot)) || sameFileAsAncestor(dir, absRoot) {
		return "", fmt.Errorf("refusing to write the site into %s: it contains the repository", dir)
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return dir, os.MkdirAll(dir, 0o755)
	}
	if err != nil {
		return "", err
	}
	if len(entries) > 0 {
		if _, err := os.Stat(filepath.Join(dir, ".nojekyll")); err != nil {
			return "", fmt.Errorf("refusing to replace %s: it is not empty and holds no .nojekyll from an earlier site build", dir)
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return "", err
	}
	return dir, os.MkdirAll(dir, 0o755)
}

// resolveExisting resolves symlinks on the longest existing ancestor of name
// and reattaches any not-yet-created trailing path segments.
func resolveExisting(name string) string {
	var missing []string
	for {
		if resolved, err := filepath.EvalSymlinks(name); err == nil {
			return filepath.Join(append([]string{resolved}, missing...)...)
		}
		parent := filepath.Dir(name)
		if parent == name {
			return filepath.Join(append([]string{name}, missing...)...)
		}
		missing = append([]string{filepath.Base(name)}, missing...)
		name = parent
	}
}

func containsPath(outer, inner string) bool {
	rel, err := filepath.Rel(outer, inner)
	return err == nil && (rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func sameFileAsAncestor(dir, root string) bool {
	dirInfo, err := os.Stat(dir)
	if err != nil {
		return false
	}
	for current := root; ; current = filepath.Dir(current) {
		if info, err := os.Stat(current); err == nil && os.SameFile(dirInfo, info) {
			return true
		}
		if filepath.Dir(current) == current {
			return false
		}
	}
}
