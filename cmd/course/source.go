package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/nickstrad/sqlite-internals/internal/repocheck"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
	"github.com/nickstrad/sqlite-internals/sqlite"
)

func runCommand(ctx context.Context, dir string, log io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Stdout = log
	cmd.Stderr = log
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, joinArgs(args), err)
	}
	return nil
}

func verifyGitCheckout(ctx context.Context, dir string) error {
	pin := sqlite.Current()
	if err := repocheck.VerifyTree(dir); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return nil
	}
	head, err := repocheck.Git(dir, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if got := strings.TrimSpace(head); got != pin.GitCommit {
		return fmt.Errorf("%s is at git commit %s, the pin is %s", dir, got, pin.GitCommit)
	}
	return nil
}

func runFetchSource(ctx context.Context, root string, args []string) error {
	pin := sqlite.Current()
	flags := flag.NewFlagSet("fetch-source", flag.ContinueOnError)
	destFlag := flags.String("dest", filepath.Join(root, "_src", pin.SourceDirName()), "directory to clone into")
	if err := flags.Parse(args); err != nil {
		return exitError{2}
	}
	dest, err := filepath.Abs(*destFlag)
	if err != nil {
		return err
	}
	if _, err := os.Stat(dest); err == nil {
		if err := verifyGitCheckout(ctx, dest); err != nil {
			return fmt.Errorf("existing %s: %w (delete it and run fetch-source again)", dest, err)
		}
		fmt.Printf("OK %s already present: tag %s, git %s, manifest.uuid %s\n", relative(root, dest), pin.Tag, pin.GitCommit, pin.FossilID)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	staging := dest + ".partial"
	if err := os.RemoveAll(staging); err != nil {
		return err
	}
	fmt.Printf("cloning %s tag %s (shallow)\n", pin.GitRepo, pin.Tag)
	if err := runCommand(ctx, root, os.Stderr, "git", "-c", "advice.detachedHead=false", "clone", "--quiet", "--depth", "1", "--single-branch", "--branch", pin.Tag, pin.GitRepo, staging); err != nil {
		os.RemoveAll(staging)
		return err
	}
	if err := verifyGitCheckout(ctx, staging); err != nil {
		os.RemoveAll(staging)
		return err
	}
	if err := os.Rename(staging, dest); err != nil {
		return err
	}
	fmt.Printf("OK %s: tag %s, git %s, manifest.uuid %s\n", relative(root, dest), pin.Tag, pin.GitCommit, pin.FossilID)
	return nil
}

var builtTools = []string{"sqlite3", "showwal", "dbhash"}

func copyTree(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if rel == ".git" || strings.HasPrefix(rel, ".git"+string(filepath.Separator)) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(destination, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case entry.IsDir():
			return os.MkdirAll(target, 0o755)
		case info.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case info.Mode().IsRegular():
			return copyFile(path, target, info.Mode().Perm())
		}
		return nil
	})
}

func copyFile(source, destination string, mode fs.FileMode) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode|0o200)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func runBuildTools(ctx context.Context, root string, args []string) error {
	flags := flag.NewFlagSet("build-tools", flag.ContinueOnError)
	reuse := flags.Bool("reuse", false, "reuse an already configured build copy")
	jobs := flags.Int("j", min(runtime.NumCPU(), 4), "parallel make jobs")
	if err := flags.Parse(args); err != nil {
		return exitError{2}
	}
	tree, err := repocheck.PinnedTree(root)
	if err != nil {
		return err
	}
	pin := sqlite.Current()
	tools, err := sqlitex.ToolsDir()
	if err != nil {
		return err
	}
	buildRoot := filepath.Join(tools, "build")
	buildDir := filepath.Join(buildRoot, pin.SourceDirName())
	binDir := filepath.Join(tools, "bin")
	if err := os.MkdirAll(buildRoot, 0o755); err != nil {
		return err
	}
	logPath := filepath.Join(buildRoot, "build.log")
	log, err := os.Create(logPath)
	if err != nil {
		return err
	}
	defer log.Close()
	_, makefileErr := os.Stat(filepath.Join(buildDir, "Makefile"))
	if !*reuse || makefileErr != nil {
		fmt.Printf("copying the pinned tree %s to %s\n", tree, buildDir)
		if err := os.RemoveAll(buildDir); err != nil {
			return err
		}
		if err := copyTree(tree, buildDir); err != nil {
			return err
		}
		fmt.Println("running ./configure")
		if err := runCommand(ctx, buildDir, log, "./configure"); err != nil {
			return fmt.Errorf("%w (log: %s)", err, logPath)
		}
	}
	fmt.Printf("running make %s\n", joinArgs(builtTools))
	makeArgs := append([]string{fmt.Sprintf("-j%d", max(*jobs, 1))}, builtTools...)
	if err := runCommand(ctx, buildDir, log, "make", makeArgs...); err != nil {
		return fmt.Errorf("%w (log: %s)", err, logPath)
	}
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return err
	}
	for _, tool := range builtTools {
		source := filepath.Join(buildDir, tool)
		if _, err := os.Stat(source); err != nil {
			return fmt.Errorf("make did not produce %s", tool)
		}
		if err := copyFile(source, filepath.Join(binDir, tool), 0o755); err != nil {
			return err
		}
		info, _ := os.Stat(filepath.Join(binDir, tool))
		fmt.Printf("OK %s (%d bytes)\n", filepath.Join(binDir, tool), info.Size())
	}
	cli := sqlitex.CLI{Path: filepath.Join(binDir, "sqlite3")}
	id, err := cli.QuerySourceID(ctx)
	if err != nil {
		return err
	}
	if err := sqlitex.CheckSourceID(cli.Path, id); err != nil {
		return err
	}
	probeDir, err := os.MkdirTemp("", "course-vfstrace-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(probeDir)
	result, err := cli.Run(ctx, sqlitex.CLIRun{Args: []string{"-vfstrace", "probe.db", "CREATE TABLE t(x);"}, Dir: probeDir})
	if err != nil {
		return err
	}
	if !strings.Contains(result.Stdout, "trace.x") {
		return fmt.Errorf("%s -vfstrace printed no VFS trace", cli.Path)
	}
	fmt.Printf("OK %s reports the pinned source id %s and supports -vfstrace\n", cli.Path, id)
	return nil
}
