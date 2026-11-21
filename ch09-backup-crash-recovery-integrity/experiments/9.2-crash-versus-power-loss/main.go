package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/nickstrad/sqlite-internals/internal/procs"
	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
	"github.com/nickstrad/sqlite-internals/internal/trace"
	"github.com/nickstrad/sqlite-internals/internal/walfile"
)

const (
	childMode       = "child-writer"
	baseStep        = 3
	lastStep        = 11
	checkpointAfter = 7
	pwriteKills     = 6
	packagePath     = "ch09-backup-crash-recovery-integrity/experiments/9.2-crash-versus-power-loss"
)

var levels = []string{"OFF", "NORMAL", "FULL"}

func runChild(args []string) error {
	flags := flag.NewFlagSet(childMode, flag.ContinueOnError)
	db := flags.String("db", "", "")
	level := flags.String("sync", "", "")
	if err := flags.Parse(args); err != nil {
		return err
	}
	runtime.LockOSThread()
	ctx := context.Background()
	r := recipe.Default()
	options := r.SQLiteOptions()
	options.Conn.Synchronous = *level
	options.Conn.WALAutocheckpoint = 0
	handle, err := sqlitex.Open(ctx, *db, options)
	if err != nil {
		return err
	}
	for k := baseStep + 1; k <= lastStep; k++ {
		if err := r.ApplyStep(ctx, handle, k); err != nil {
			return err
		}
		wal, err := walfile.ParseFile(*db + "-wal")
		if err != nil {
			return err
		}
		fmt.Printf("commit %d end=%d generation=%d\n", k, walfile.HeaderSize+wal.LastCommitFrame*wal.FrameSize(), wal.Header.Salt1)
		fmt.Printf("ack %d\n", k)
		if k == checkpointAfter {
			result, err := sqlitex.Checkpoint(ctx, handle, "", sqlitex.CheckpointPassive)
			if err != nil {
				return err
			}
			fmt.Printf("checkpoint %d %s\n", k, result)
		}
	}
	os.Exit(0)
	return nil
}

type writerRun struct {
	lastAck    int
	commitEnd  map[int]int64
	generation map[int]uint32
	checkpoint string
	lastEvent  string
}

func parseRun(stdout string) (writerRun, error) {
	run := writerRun{lastAck: baseStep, commitEnd: map[int]int64{}, generation: map[int]uint32{}}
	scanner := bufio.NewScanner(strings.NewReader(stdout))
	for scanner.Scan() {
		line := scanner.Text()
		var k int
		var end int64
		var generation uint32
		switch {
		case strings.HasPrefix(line, "ack "):
			if _, err := fmt.Sscanf(line, "ack %d", &k); err != nil || k != run.lastAck+1 {
				return run, fmt.Errorf("bad ack line %q after %d", line, run.lastAck)
			}
			run.lastAck, run.lastEvent = k, "commit"
		case strings.HasPrefix(line, "commit "):
			if _, err := fmt.Sscanf(line, "commit %d end=%d generation=%d", &k, &end, &generation); err != nil {
				return run, fmt.Errorf("bad commit line %q", line)
			}
			run.commitEnd[k], run.generation[k] = end, generation
		case strings.HasPrefix(line, "checkpoint "):
			run.checkpoint, run.lastEvent = line, "checkpoint"
		default:
			return run, fmt.Errorf("unexpected child output %q", line)
		}
	}
	return run, scanner.Err()
}

func ruleCut(level string, run writerRun) (cut int64, hasCut bool, outside string) {
	switch level {
	case "OFF":
		if run.checkpoint != "" {
			return 0, true, "the checkpoint wrote the database file and nothing was ever synced"
		}
		return 0, true, ""
	case "NORMAL":
		if run.lastEvent == "checkpoint" {
			return 0, false, ""
		}
		return walfile.HeaderSize, true, ""
	}
	return 0, false, ""
}

type lab struct {
	traced bool
	ctx    context.Context
	lab    *procs.Lab
	r      recipe.Recipe
	dir    string
	base   string
}

func copyFile(from, to string, limit int64) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	var reader io.Reader = in
	if limit >= 0 {
		reader = io.LimitReader(in, limit)
	}
	_, copyErr := io.Copy(out, reader)
	return errors.Join(copyErr, out.Sync(), out.Close())
}

func (x *lab) recover(path string) (string, recipe.Match) {
	db, err := x.r.Open(x.ctx, path)
	if err != nil {
		x.lab.Fatal("reopen", err)
	}
	var integrity string
	err = db.QueryRowContext(x.ctx, "PRAGMA integrity_check").Scan(&integrity)
	if err := errors.Join(err, db.Close()); err != nil {
		x.lab.Fatal("integrity_check", err)
	}
	match, err := x.r.MatchStep(x.ctx, path)
	if err != nil {
		x.lab.Fatal("match", err)
	}
	return integrity, match
}

func (x *lab) runWriter(dir, level string, kill *trace.KillAt) (string, *trace.Result) {
	db := filepath.Join(dir, "r.db")
	if err := copyFile(x.base, db, -1); err != nil {
		x.lab.Fatal("copy base", err)
	}
	args := []string{childMode, "-db", db, "-sync", level}
	if !x.traced {
		self, err := os.Executable()
		if err != nil {
			x.lab.Fatal("executable", err)
		}
		out, err := exec.Command(self, args...).Output()
		if err != nil {
			x.lab.Fatal("writer", err)
		}
		return string(out), nil
	}
	command, err := trace.SelfCommand(args...)
	if err != nil {
		x.lab.Fatal("command", err)
	}
	result, err := trace.Run(x.ctx, trace.Options{Command: command, KillAt: kill})
	if err != nil {
		x.lab.Fatal("strace", err)
	}
	if kill == nil && result.ExitCode != 0 {
		x.lab.Fatal("writer", fmt.Errorf("exit %d: %s", result.ExitCode, result.Stderr))
	}
	return result.Stdout, result
}

func (x *lab) model(level string) *trace.Result {
	dir := filepath.Join(x.dir, "model-"+strings.ToLower(level))
	stdout, result := x.runWriter(dir, level, nil)
	run, err := parseRun(stdout)
	if err != nil {
		x.lab.Fatal("parse", err)
	}
	db := filepath.Join(dir, "r.db")
	fmt.Printf("-- synchronous=%s: the writer acknowledged steps %d-%d; %s; it exited without closing\n", level, baseStep+1, run.lastAck, run.checkpoint)
	for _, suffix := range []string{"", "-wal"} {
		if err := copyFile(db+suffix, filepath.Join(dir, "crash", "r.db"+suffix), -1); err != nil {
			x.lab.Fatal("copy", err)
		}
	}
	integrity, match := x.recover(filepath.Join(dir, "crash", "r.db"))
	fmt.Printf("   process crash (files as the kernel holds them): integrity_check %s, recovered %s\n", integrity, match)
	x.lab.Check(integrity == "ok" && match.Step == run.lastAck, fmt.Sprintf("%s: a process crash keeps every acknowledged step (%d)", level, run.lastAck))

	cut, hasCut, outside := ruleCut(level, run)
	if result != nil {
		real, err := filepath.EvalSymlinks(db)
		if err != nil {
			x.lab.Fatal("path", err)
		}
		walSyncs := trace.SyncsOf(result.Log.Calls, real+"-wal")
		dbSyncs := trace.SyncsOf(result.Log.Calls, real)
		fmt.Printf("   strace: -wal %s\n", walSyncs)
		fmt.Printf("   strace: database file %s\n", dbSyncs)
		traceOutside := dbSyncs.HasUnsynced
		x.lab.Check(walSyncs.HasUnsynced == hasCut && (!hasCut || walSyncs.FirstUnsynced == cut) && traceOutside == (outside != ""),
			fmt.Sprintf("%s: the strace record agrees with the rule read from wal.c and pager.c about which bytes were synced", level))
	} else {
		fmt.Println(trace.LinuxOnlyMessage("the strace confirmation of the synced bytes (the model below uses the rule read from wal.c and pager.c)", packagePath))
	}
	if outside != "" {
		fmt.Printf("   power-cut model: not applicable at %s: %s, so a power cut can leave any mix of old and new database pages\n", level, outside)
		x.lab.Check(level == "OFF", "only synchronous=OFF leaves database-file writes that were never synced")
		return result
	}
	walData, err := os.ReadFile(db + "-wal")
	if err != nil {
		x.lab.Fatal("read wal", err)
	}
	keep := int64(len(walData))
	if hasCut {
		keep = cut
	}
	lost := 0
	for k := baseStep + 1; k <= run.lastAck; k++ {
		if run.generation[k] == run.generation[run.lastAck] && run.commitEnd[k] > keep {
			lost++
		}
	}
	power := filepath.Join(dir, "power", "r.db")
	if err := errors.Join(copyFile(db, power, -1), copyFile(db+"-wal", power+"-wal", keep)); err != nil {
		x.lab.Fatal("copy", err)
	}
	integrity, match = x.recover(power)
	fmt.Printf("   power-cut model: -wal kept up to byte %d of %d (everything after the last sync removed), no -shm: integrity_check %s, recovered %s; acknowledged commits past the cut: %d\n",
		keep, len(walData), integrity, match, lost)
	x.lab.Check(integrity == "ok" && match.Step == run.lastAck-lost, fmt.Sprintf("%s: the model recovers step %d, the last acknowledged step minus the %d commit(s) written after the last sync", level, run.lastAck-lost, lost))
	if level == "NORMAL" {
		x.lab.Check(lost > 0 && match.Step < run.lastAck && match.Step == checkpointAfter, fmt.Sprintf("NORMAL: the acknowledged commits after the checkpoint at step %d are lost in the model, yet the database is consistent", checkpointAfter))
	}
	if level == "FULL" {
		x.lab.Check(lost == 0 && match.Step == run.lastAck, "FULL: no acknowledged commit is lost in the model")
	}
	return result
}

func writerThread(result *trace.Result) int {
	counts := map[int]int{}
	for _, call := range result.Log.Calls {
		if call.Name == "pwrite64" {
			counts[call.PID]++
		}
	}
	best, most := 0, 0
	for pid, n := range counts {
		if n > most {
			best, most = pid, n
		}
	}
	return best
}

func (x *lab) killSweep(level string, uninterrupted *trace.Result) {
	tid := writerThread(uninterrupted)
	syncs, writes := 0, 0
	for _, call := range uninterrupted.Log.Calls {
		if call.PID != tid {
			continue
		}
		switch call.Name {
		case "fsync", "fdatasync":
			syncs++
		case "pwrite64":
			writes++
		}
	}
	fmt.Printf("-- synchronous=%s: the uninterrupted writer made %d sync call(s) and %d pwrite64 call(s); kill on entry to each sync and to %d of the writes\n", level, syncs, writes, pwriteKills)
	var points []trace.KillAt
	for i := 1; i <= syncs; i++ {
		points = append(points, trace.KillAt{Syscalls: []string{"fsync", "fdatasync"}, When: i})
	}
	for j := 0; j < pwriteKills; j++ {
		points = append(points, trace.KillAt{Syscalls: []string{"pwrite64"}, When: 1 + j*(writes-1)/(pwriteKills-1)})
	}
	for i, point := range points {
		dir := filepath.Join(x.dir, fmt.Sprintf("kill-%s-%02d", strings.ToLower(level), i))
		stdout, result := x.runWriter(dir, level, &point)
		run, err := parseRun(stdout)
		if err != nil {
			x.lab.Fatal("parse", err)
		}
		killed := "nothing (the run finished)"
		for _, call := range trace.RelativeTo(dir, result.Log.Calls) {
			if call.Unfinished {
				killed = call.String()
			}
		}
		integrity, match := x.recover(filepath.Join(dir, "r.db"))
		fmt.Printf("   kill at %s #%d (%s): acknowledged %d, recovered %s, integrity_check %s\n", point.Syscalls[0], point.When, killed, run.lastAck, match, integrity)
		x.lab.Check(result.Killed == "SIGKILL" && integrity == "ok" && match.Found() && match.Step >= run.lastAck && match.Step <= run.lastAck+1,
			fmt.Sprintf("%s kill %d: integrity ok, recovered step %d is the last acknowledged step or the one being committed", level, i+1, match.Step))
	}
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == childMode {
		if err := runChild(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	portable := flag.Bool("portable", false, "run the path used where strace is missing, even on Linux")
	flag.Parse()
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("9.2")
	fmt.Printf("INFO: work dir %s\n", sqlitex.DisplayPath(dir))
	x := &lab{traced: trace.Supported && !*portable, ctx: ctx, lab: procs.NewLab(os.Stdout), r: recipe.Default(), dir: dir, base: filepath.Join(dir, "base", "r.db")}
	if err := os.MkdirAll(filepath.Dir(x.base), 0o755); err != nil {
		x.lab.Fatal("mkdir", err)
	}
	if err := x.r.Build(ctx, x.base, baseStep); err != nil {
		x.lab.Fatal("build", err)
	}
	fmt.Printf("== recipe built to step %d; a writer child commits steps %d-%d on one connection (wal_autocheckpoint=0) and runs one PASSIVE checkpoint after step %d\n", baseStep, baseStep+1, lastStep, checkpointAfter)
	fmt.Println()
	fmt.Println("== part B: what a process crash keeps, and what the simplest power cut keeps")
	traces := map[string]*trace.Result{}
	for _, level := range levels {
		traces[level] = x.model(level)
	}
	fmt.Println()
	fmt.Println("== part A: kill the writer at chosen syscalls, reopen, identify the state")
	if !x.traced {
		fmt.Println(trace.LinuxOnlyMessage("killing the writer at a chosen syscall with strace", packagePath))
	} else {
		for _, level := range levels {
			x.killSweep(level, traces[level])
		}
	}
	x.lab.Report("CRASH VERSUS POWER LOSS")
}
