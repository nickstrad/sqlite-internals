package repocheck

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestMissingSourcesFileIsAProblem(t *testing.T) {
	root := t.TempDir()
	chapter := Chapter{Number: "07", Dir: "ch07-x", Path: filepath.Join(root, "ch07-x")}
	writeFile(t, filepath.Join(chapter.Path, "ch07-x.md"), "# x\n")
	problems, _, err := CheckSources(chapter, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(joinProblems(problems), "missing SOURCES.sha256") {
		t.Fatalf("problems %v", problems)
	}
	writeFile(t, filepath.Join(chapter.Path, "SOURCES.sha256"), "")
	problems, _, err = CheckSources(chapter, t.TempDir())
	if err != nil || len(problems) != 0 {
		t.Fatalf("empty SOURCES.sha256: %v %v", problems, err)
	}
}

func TestCheckEvidenceRejectsNonPositiveRuns(t *testing.T) {
	root := t.TempDir()
	chapter := Chapter{Number: "01", Dir: "ch01-x", Path: filepath.Join(root, "ch01-x")}
	writeFile(t, filepath.Join(chapter.Path, "experiments", "1.1-fails", "main.go"), "package main\n\nimport \"os\"\n\nfunc main() { os.Exit(3) }\n")
	writeFile(t, filepath.Join(chapter.Path, "evidence", "1.1-fails.txt"), "")
	experiments, err := Experiments(chapter)
	if err != nil || len(experiments) != 1 {
		t.Fatalf("experiments %v %v", experiments, err)
	}
	for _, runs := range []int{0, -1} {
		outcome := CheckEvidence(context.Background(), root, experiments[0], runs)
		if outcome.Passed() || outcome.Status != "ERROR" {
			t.Errorf("runs=%d: %s %s", runs, outcome.Status, outcome.Detail)
		}
	}
}

func TestIncludeMarkersInsideFencesAreIgnored(t *testing.T) {
	dir := t.TempDir()
	code := "package main\n"
	writeFile(t, filepath.Join(dir, "real.go"), code)
	markdown := strings.Join([]string{
		"# Chapter",
		"````markdown",
		"<!-- include: missing.go -->",
		"```go",
		"```",
		"````",
		"~~~",
		"<!-- include: alsomissing.go -->",
		"~~~",
		"<!-- include: real.go -->",
		"```go",
		"package main",
		"```",
		"",
	}, "\n")
	result := ProcessInline(filepath.Join(dir, "ch01-x.md"), markdown)
	if len(result.Problems) != 0 || len(result.Includes) != 1 || result.Includes[0].Target != "real.go" {
		t.Fatalf("problems %v includes %+v", result.Problems, result.Includes)
	}
	if result.Output != markdown {
		t.Fatalf("output changed:\n%s", result.Output)
	}
	blocks := FindIncludeBlocks(markdown)
	if len(blocks) != 1 || blocks[0].Target != "real.go" || blocks[0].MarkerLine != 10 {
		t.Fatalf("blocks %+v", blocks)
	}
}
