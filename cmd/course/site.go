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

func prepareSiteDir(root, out string) (string, error) {
	dir, err := filepath.Abs(out)
	if err != nil {
		return "", err
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if rel, err := filepath.Rel(dir, absRoot); err == nil && (rel == "." || !strings.HasPrefix(rel, "..")) {
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
