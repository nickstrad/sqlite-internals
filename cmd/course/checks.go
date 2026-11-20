package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickstrad/sqlite-internals/internal/repocheck"
)

// report collects PASS/FAIL output across one or more repository checks so
// "check all" runs every check before exiting 1 on failure.
type report struct {
	failed bool
}

func (r *report) result(name string, problems []repocheck.Problem, summary string) {
	if len(problems) == 0 {
		fmt.Printf("PASS %s: %s\n", name, summary)
		return
	}
	r.failed = true
	fmt.Printf("FAIL %s: %d problem(s)\n", name, len(problems))
	for _, problem := range problems {
		fmt.Printf("  %s\n", problem)
	}
}

func (r *report) err(name string, err error) {
	r.failed = true
	fmt.Printf("FAIL %s: %v\n", name, err)
}

func (r *report) exit() error {
	if r.failed {
		return exitError{1}
	}
	return nil
}

// runInline implements "course inline [-check] <chapter.md>...".
func runInline(root string, args []string) error {
	flags := flag.NewFlagSet("inline", flag.ContinueOnError)
	check := flags.Bool("check", false, "fail if any block differs from its file instead of rewriting")
	if err := flags.Parse(args); err != nil {
		return exitError{2}
	}
	if flags.NArg() == 0 {
		return usageError("inline needs at least one chapter markdown file")
	}
	var r report
	for _, name := range flags.Args() {
		inlineOne(&r, name, name, *check)
	}
	return r.exit()
}

func inlineOne(r *report, name, display string, check bool) {
	result, err := repocheck.InlineFile(name, check)
	label := "inline " + display
	if check {
		label = "inline -check " + display
	}
	if err != nil {
		r.err(label, err)
		return
	}
	summary := fmt.Sprintf("%d include block(s) match their files", len(result.Includes))
	if !check && result.Changed > 0 && len(result.Problems) == 0 {
		summary = fmt.Sprintf("%d include block(s), %d rewritten from their files", len(result.Includes), result.Changed)
	}
	r.result(label, result.Problems, summary)
}

// chapterArg consumes a leading positional chapter selector (such as "ch01")
// before flag parsing so flags may appear after it.
func chapterArg(args []string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

// runCheck dispatches every "course check <subcommand>" check.
func runCheck(ctx context.Context, root string, args []string) error {
	if len(args) == 0 {
		return usageError("check needs a subcommand")
	}
	var r report
	sub, rest := args[0], args[1:]
	switch sub {
	case "sources", "spans":
		only, extra := chapterArg(rest)
		if len(extra) > 0 {
			return usageError("check %s takes at most one chapter argument", sub)
		}
		checkTree(&r, root, sub, only)
	case "comments":
		if len(rest) != 2 {
			return usageError("check comments needs <old-rev> <new-rev>")
		}
		problems, count, err := repocheck.CheckComments(root, rest[0], rest[1], repocheck.CommentsOptions{})
		if err != nil {
			r.err("check comments", err)
			break
		}
		r.result("check comments "+rest[0]+" "+rest[1], problems, fmt.Sprintf("%d Go file(s) differ only in comments", count))
	case "prose":
		flags := flag.NewFlagSet("check prose", flag.ContinueOnError)
		var excludes stringList
		flags.Var(&excludes, "exclude", "path prefix to skip (repeatable)")
		if err := flags.Parse(rest); err != nil {
			return exitError{2}
		}
		if flags.NArg() != 1 {
			return usageError("check prose needs <base-rev>..<head-rev>")
		}
		base, head, err := repocheck.ParseRange(flags.Arg(0))
		if err != nil {
			return usageError("%v", err)
		}
		problems, counts, err := repocheck.CheckProse(root, base, head, repocheck.ProseOptions{Exclude: excludes})
		if err != nil {
			r.err("check prose", err)
			break
		}
		r.result("check prose "+flags.Arg(0), problems, fmt.Sprintf("%d prose line(s) introduced in range, all by %s; %d line(s) predate %s and were not checked", counts.InRange, repocheck.ProseAuthorPrefix, counts.BeforeRange, base))
	case "evidence":
		only, extra := chapterArg(rest)
		flags := flag.NewFlagSet("check evidence", flag.ContinueOnError)
		runs := flags.Int("runs", 2, "runs per experiment")
		if err := flags.Parse(extra); err != nil {
			return exitError{2}
		}
		if *runs < 1 {
			return usageError("check evidence -runs must be at least 1, got %d", *runs)
		}
		if only == "" && flags.NArg() == 1 {
			only = flags.Arg(0)
		}
		checkEvidence(ctx, &r, root, only, *runs)
	case "gofmt":
		problems, count, err := repocheck.CheckGofmt(root)
		if err != nil {
			r.err("check gofmt", err)
			break
		}
		r.result("check gofmt", problems, fmt.Sprintf("%d Go file(s) formatted", count))
	case "paths":
		sweep, err := repocheck.SweepPaths(root)
		if err != nil {
			r.err("check paths", err)
			break
		}
		printPathSweepReport(sweep)
		r.result("check paths", sweep.Problems, fmt.Sprintf("%d tracked text file(s) free of machine-local paths and unresolved relative links (%d binary file(s) skipped, %d external link(s) reported)", sweep.Checked, len(sweep.Binary), len(sweep.External)))
	case "all":
		flags := flag.NewFlagSet("check all", flag.ContinueOnError)
		runs := flags.Int("runs", 2, "runs per experiment")
		skipEvidence := flags.Bool("skip-evidence", false, "do not run experiments")
		if err := flags.Parse(rest); err != nil {
			return exitError{2}
		}
		if *runs < 1 {
			return usageError("check all -runs must be at least 1, got %d", *runs)
		}
		checkTree(&r, root, "sources", "")
		checkTree(&r, root, "spans", "")
		chapters, err := repocheck.Chapters(root, "")
		if err != nil {
			r.err("chapters", err)
		}
		for _, chapter := range chapters {
			if _, err := os.Stat(chapter.Markdown()); err == nil {
				inlineOne(&r, chapter.Markdown(), relative(root, chapter.Markdown()), true)
			}
		}
		if !*skipEvidence {
			checkEvidence(ctx, &r, root, "", *runs)
		}
		for _, name := range []string{"gofmt", "paths"} {
			if err := runCheck(ctx, root, []string{name}); err != nil {
				r.failed = true
			}
		}
	default:
		return usageError("unknown check %q", sub)
	}
	return r.exit()
}

// stringList implements flag.Value for repeatable flags such as -exclude.
type stringList []string

// String formats the collected values as a comma-separated list for flag.Value.
func (s *stringList) String() string { return strings.Join(*s, ",") }

// Set appends one flag value to the list for flag.Value.
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func relative(root, name string) string {
	if rel, err := filepath.Rel(root, name); err == nil {
		return rel
	}
	return name
}

// checkTree runs either CheckSources or CheckSpans against the pinned tree
// ($SQLITE_SRC or _src/sqlite-3.53.4) for matching chapter directories.
func checkTree(r *report, root, kind, only string) {
	label := "check " + kind
	chapters, err := repocheck.Chapters(root, only)
	if err != nil {
		r.err(label, err)
		return
	}
	if len(chapters) == 0 {
		fmt.Printf("PASS %s: no chapter directories yet\n", label)
		return
	}
	tree, err := repocheck.PinnedTree(root)
	if err != nil {
		r.err(label, err)
		return
	}
	for _, chapter := range chapters {
		var problems []repocheck.Problem
		var count int
		var err error
		if kind == "sources" {
			problems, count, err = repocheck.CheckSources(chapter, tree)
		} else {
			problems, count, err = repocheck.CheckSpans(chapter, tree)
		}
		if err != nil {
			r.err(label+" "+chapter.Dir, err)
			continue
		}
		unit := "copied file(s) match the pin"
		if kind == "spans" {
			unit = "span(s) start at their symbol"
		}
		r.result(label+" "+chapter.Dir, problems, fmt.Sprintf("%d %s", count, unit))
	}
}

func checkEvidence(ctx context.Context, r *report, root, only string, runs int) {
	chapters, err := repocheck.Chapters(root, only)
	if err != nil {
		r.err("check evidence", err)
		return
	}
	total := 0
	for _, chapter := range chapters {
		experiments, err := repocheck.Experiments(chapter)
		if err != nil {
			r.err("check evidence "+chapter.Dir, err)
			continue
		}
		for _, experiment := range experiments {
			total++
			outcome := repocheck.CheckEvidence(ctx, root, experiment, runs)
			line := fmt.Sprintf("%s %s: %s", outcome.Status, experiment.Package, outcome.Detail)
			if !outcome.Passed() {
				r.failed = true
			}
			fmt.Println(line)
		}
	}
	if total == 0 {
		fmt.Println("PASS check evidence: no experiments yet")
	}
}

func printPathSweepReport(sweep repocheck.PathSweep) {
	for _, name := range sweep.Binary {
		fmt.Printf("INFO check paths: binary file skipped: %s\n", name)
	}
	for _, name := range sweep.IgnoredButTracked {
		fmt.Printf("INFO check paths: tracked although .gitignore matches it: %s\n", name)
	}
	for _, link := range sweep.External {
		fmt.Printf("EXTERNAL %s (%d occurrence(s), first at %s)\n", link.URL, link.Count, link.Where)
	}
}
