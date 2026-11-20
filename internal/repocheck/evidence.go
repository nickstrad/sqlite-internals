package repocheck

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

// Experiment locates one numbered Go experiment package under a chapter and its
// stored expected output file under evidence/.
type Experiment struct {
	Chapter  Chapter
	Name     string
	Package  string
	Evidence string
}

var experimentDirPattern = regexp.MustCompile(`^\d+\.\d+-[a-z0-9-]+$`)

// Experiments lists all experiments matching "N.M-slug" under chapter/experiments/,
// sorted numerically by chapter and experiment number so 1.2 precedes 1.10.
func Experiments(chapter Chapter) ([]Experiment, error) {
	dir := filepath.Join(chapter.Path, "experiments")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var experiments []Experiment
	for _, entry := range entries {
		if !entry.IsDir() || !experimentDirPattern.MatchString(entry.Name()) {
			continue
		}
		experiments = append(experiments, Experiment{
			Chapter:  chapter,
			Name:     entry.Name(),
			Package:  "./" + chapter.Dir + "/experiments/" + entry.Name(),
			Evidence: filepath.Join(chapter.Path, "evidence", entry.Name()+".txt"),
		})
	}
	sort.Slice(experiments, func(i, j int) bool { return experimentLess(experiments[i].Name, experiments[j].Name) })
	return experiments, nil
}

func experimentLess(a, b string) bool {
	var aMajor, aMinor, bMajor, bMinor int
	fmt.Sscanf(a, "%d.%d", &aMajor, &aMinor)
	fmt.Sscanf(b, "%d.%d", &bMajor, &bMinor)
	if aMajor != bMajor {
		return aMajor < bMajor
	}
	if aMinor != bMinor {
		return aMinor < bMinor
	}
	return a < b
}

// StripInfo removes lines prefixed with "INFO:" so machine-local paths or
// per-run PIDs printed for diagnostics do not break exact evidence comparisons.
func StripInfo(text string) string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "INFO:") {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// EvidenceOutcome holds the verification status (PASS, DIFF, FAIL, BUILD-FAIL,
// MISSING, LINUX-ONLY, or ERROR) and explanation for one experiment.
type EvidenceOutcome struct {
	Experiment Experiment
	Status     string
	Detail     string
}

// Passed reports whether the experiment matched its stored evidence or skipped
// cleanly as LINUX-ONLY on a non-Linux host.
func (o EvidenceOutcome) Passed() bool {
	return o.Status == "PASS" || o.Status == "LINUX-ONLY"
}

// CheckEvidence builds experiment once and runs it runs times from root,
// comparing stdout (minus INFO: lines) against experiment.Evidence.
func CheckEvidence(ctx context.Context, root string, experiment Experiment, runs int) EvidenceOutcome {
	outcome := EvidenceOutcome{Experiment: experiment}
	if runs < 1 {
		outcome.Status, outcome.Detail = "ERROR", fmt.Sprintf("runs must be at least 1, got %d", runs)
		return outcome
	}
	expected, err := os.ReadFile(experiment.Evidence)
	if err != nil {
		outcome.Status, outcome.Detail = "MISSING", "no evidence file "+relativeTo(root, experiment.Evidence)
		return outcome
	}
	binDir, err := os.MkdirTemp("", "course-evidence-")
	if err != nil {
		outcome.Status, outcome.Detail = "ERROR", err.Error()
		return outcome
	}
	defer os.RemoveAll(binDir)
	binary := filepath.Join(binDir, "experiment")
	// -buildvcs=false avoids git metadata lookup errors inside mounted worktrees or containers.
	build := exec.CommandContext(ctx, "go", "build", "-buildvcs=false", "-o", binary, experiment.Package)
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		outcome.Status, outcome.Detail = "BUILD-FAIL", strings.TrimSpace(string(out))
		return outcome
	}
	want := StripInfo(string(expected))
	for run := 1; run <= runs; run++ {
		cmd := exec.CommandContext(ctx, binary)
		cmd.Dir = root
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		if err != nil {
			outcome.Status = "FAIL"
			outcome.Detail = fmt.Sprintf("run %d: %v: %s", run, err, lastLines(stderr.String(), 5))
			return outcome
		}
		got := StripInfo(stdout.String())
		if got != want {
			if runtime.GOOS != "linux" && strings.Contains(stdout.String(), "LINUX-ONLY:") {
				outcome.Status, outcome.Detail = "LINUX-ONLY", "output differs from the Linux evidence where Linux-only parts were skipped"
				return outcome
			}
			outcome.Status = "DIFF"
			outcome.Detail = fmt.Sprintf("run %d: %s", run, firstDifference(got, want))
			return outcome
		}
	}
	outcome.Status = "PASS"
	outcome.Detail = fmt.Sprintf("%d runs identical to the evidence", runs)
	return outcome
}

func lastLines(text string, n int) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

func relativeTo(root, name string) string {
	if rel, err := filepath.Rel(root, name); err == nil {
		return rel
	}
	return name
}
