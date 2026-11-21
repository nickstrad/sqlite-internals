package main

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nickstrad/sqlite-internals/internal/equal"
	"github.com/nickstrad/sqlite-internals/internal/procs"
	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
	"github.com/nickstrad/sqlite-internals/internal/walfile"
	"github.com/nickstrad/sqlite-internals/internal/walship"
)

const (
	quietStep           = 6
	lastRefStep         = 22
	pageSize            = 4096
	backupPages         = 5
	giveUpAfter         = 8
	schemaCookieLowByte = 43
	methodCopy          = "closed-file copy"
	methodBackup        = "backup API"
	methodVacuum        = "VACUUM INTO"
	methodPages         = "pages in a read transaction"
	methodFile          = "file + WAL under a held transaction"
)

var errGaveUp = errors.New("gave up")

type levels struct{ e1, e2, e3, e3ExceptJournalMode, canonical bool }

func (l levels) String() string {
	mark := func(b bool) string {
		if b {
			return "yes"
		}
		return "no "
	}
	return fmt.Sprintf("E1 %s  E2 %s  E3 %s  E3-except-journal_mode %s  E1-after-VACUUM-INTO %s", mark(l.e1), mark(l.e2), mark(l.e3), mark(l.e3ExceptJournalMode), mark(l.canonical))
}

type run struct {
	ctx    context.Context
	lab    *procs.Lab
	r      recipe.Recipe
	dir    string
	live   string
	writer *procs.Child
	source *sqlitex.DB
	conn   *sql.Conn
	next   int
}

func (x *run) ref(k int) string { return filepath.Join(x.dir, "ref", fmt.Sprintf("step-%02d.db", k)) }

func (x *run) canonicalRef(k int) string {
	path := filepath.Join(x.dir, "ref", fmt.Sprintf("canonical-%02d.db", k))
	if _, err := os.Stat(path); err == nil {
		return path
	}
	db, err := x.r.Open(x.ctx, x.ref(k))
	if err != nil {
		x.lab.Fatal("open reference", err)
	}
	_, err = db.ExecContext(x.ctx, "VACUUM INTO ?", path)
	if err := errors.Join(err, db.Close()); err != nil {
		x.lab.Fatal("canonical reference", err)
	}
	return path
}

func (x *run) buildReferences() {
	build := filepath.Join(x.dir, "ref", "build.db")
	if err := os.MkdirAll(filepath.Dir(build), 0o755); err != nil {
		x.lab.Fatal("mkdir", err)
	}
	err := x.r.BuildEach(x.ctx, build, lastRefStep, func(k int) error { return copyFile(build, x.ref(k)) })
	if err != nil {
		x.lab.Fatal("references", err)
	}
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	return errors.Join(copyErr, out.Sync(), out.Close())
}

func (x *run) compare(capture string, k int, vacuumed bool) levels {
	e1, err := equal.CompareFiles(capture, x.ref(k), pageSize)
	if err != nil {
		x.lab.Fatal("E1", err)
	}
	e2, err := equal.ComparePageFiles(capture, x.ref(k), pageSize)
	if err != nil {
		x.lab.Fatal("E2", err)
	}
	e3, _, _, err := equal.CompareLogicalFiles(x.ctx, capture, x.ref(k))
	if err != nil {
		x.lab.Fatal("E3", err)
	}
	result := levels{e1: e1.Equal, e2: e2.Equal, e3: e3.Equal, e3ExceptJournalMode: e3.Equal || onlyJournalMode(e3.Differences)}
	if !result.e3ExceptJournalMode {
		return result
	}
	canonicalCapture := capture
	if !vacuumed {
		canonicalCapture = x.canonicalCapture(capture)
	}
	canonical, err := equal.CompareFiles(canonicalCapture, x.canonicalRef(k), pageSize)
	if err != nil {
		x.lab.Fatal("canonical", err)
	}
	result.canonical = canonical.Equal
	return result
}

func (x *run) canonicalCapture(capture string) string {
	path := capture + ".canonical"
	if _, err := os.Stat(path); err == nil {
		return path
	}
	scratch := filepath.Join(x.dir, "canonical-scratch", filepath.Base(capture))
	if err := os.MkdirAll(filepath.Dir(scratch), 0o755); err != nil {
		x.lab.Fatal("mkdir", err)
	}
	if err := copyFile(capture, scratch); err != nil {
		x.lab.Fatal("canonical scratch copy", err)
	}
	db, err := x.r.Open(x.ctx, scratch)
	if err != nil {
		x.lab.Fatal("open capture copy", err)
	}
	_, err = db.ExecContext(x.ctx, "VACUUM INTO ?", path)
	if err := errors.Join(err, db.Close()); err != nil {
		x.lab.Fatal("canonical capture", err)
	}
	return path
}

func onlyJournalMode(differences []string) bool {
	for _, d := range differences {
		if !strings.Contains(d, "journal_mode") {
			return false
		}
	}
	return len(differences) > 0
}

func (x *run) stateOf(capture string, vacuumed bool) (int, levels) {
	for k := 0; k <= lastRefStep; k++ {
		if l := x.compare(capture, k, vacuumed); l.e3ExceptJournalMode {
			return k, l
		}
	}
	return -1, levels{}
}

func (x *run) commit() int {
	x.lab.Must(x.writer, "recipe-session", strconv.Itoa(x.next))
	x.next++
	return x.next - 1
}

func (x *run) begin() int {
	if _, err := x.conn.ExecContext(x.ctx, "BEGIN"); err != nil {
		x.lab.Fatal("begin", err)
	}
	step, err := recipe.CurrentStep(x.ctx, x.conn)
	if err != nil {
		x.lab.Fatal("current step", err)
	}
	return step
}

func (x *run) end() {
	if _, err := x.conn.ExecContext(x.ctx, "COMMIT"); err != nil {
		x.lab.Fatal("commit", err)
	}
}

func (x *run) readPages(first, last int) []byte {
	rows, err := x.conn.QueryContext(x.ctx, "SELECT data FROM sqlite_dbpage WHERE pgno BETWEEN ? AND ? ORDER BY pgno", first, last)
	if err != nil {
		x.lab.Fatal("sqlite_dbpage", err)
	}
	defer rows.Close()
	var image []byte
	for rows.Next() {
		var page []byte
		if err := rows.Scan(&page); err != nil {
			x.lab.Fatal("scan", err)
		}
		image = append(image, page...)
	}
	if err := rows.Err(); err != nil || len(image) != (last-first+1)*pageSize {
		x.lab.Fatal("sqlite_dbpage", fmt.Errorf("%d bytes for pages %d-%d: %v", len(image), first, last, err))
	}
	return image
}

func (x *run) pageCount() int {
	var pages int
	if err := x.conn.QueryRowContext(x.ctx, "PRAGMA page_count").Scan(&pages); err != nil {
		x.lab.Fatal("page_count", err)
	}
	return pages
}

func (x *run) copyLive(suffix, name string) {
	if _, err := walfile.CopyLiveFiles(x.ctx, []walfile.CopyPair{{From: x.live + suffix, To: filepath.Join(x.dir, name)}}); err != nil {
		x.lab.Fatal("copy", err)
	}
}

func (x *run) path(name string) string { return filepath.Join(x.dir, name) }

func (x *run) quiet() {
	fmt.Printf("== part A: a quiet database (recipe step %d, no writer)\n", quietStep)
	if err := copyFile(x.ref(quietStep), x.live); err != nil {
		x.lab.Fatal("live copy", err)
	}
	if err := copyFile(x.live, x.path("a-copy.db")); err != nil {
		x.lab.Fatal("closed copy", err)
	}
	x.openSource()
	if _, err := sqlitex.Backup(x.ctx, x.conn, x.path("a-backup.db"), -1, nil); err != nil {
		x.lab.Fatal("backup", err)
	}
	if _, err := x.conn.ExecContext(x.ctx, "VACUUM INTO ?", x.path("a-vacuum.db")); err != nil {
		x.lab.Fatal("vacuum into", err)
	}
	x.begin()
	image := x.readPages(1, x.pageCount())
	x.copyLive("", "a-file.db")
	x.copyLive("-wal", "a-file.wal")
	x.end()
	if err := walship.WriteDatabase(x.path("a-pages.db"), image); err != nil {
		x.lab.Fatal("write pages", err)
	}
	if _, err := walfile.ReplayFiles(x.path("a-file.db"), x.path("a-file.wal"), x.path("a-replayed.db")); err != nil {
		x.lab.Fatal("replay", err)
	}
	x.closeSource()
	want := map[string]levels{
		methodCopy:   {true, true, true, true, true},
		methodBackup: backupLevels,
		methodVacuum: {false, false, false, true, true},
		methodPages:  {true, true, true, true, true},
		methodFile:   {true, true, true, true, true},
	}
	for _, c := range []struct{ method, file string }{
		{methodCopy, "a-copy.db"}, {methodBackup, "a-backup.db"}, {methodVacuum, "a-vacuum.db"}, {methodPages, "a-pages.db"}, {methodFile, "a-replayed.db"},
	} {
		got := x.compare(x.path(c.file), quietStep, c.method == methodVacuum)
		fmt.Printf("%-36s vs the source: %s\n", c.method, got)
		x.lab.Check(got == want[c.method], fmt.Sprintf("%s reaches exactly the expected levels on a quiet database", c.method))
	}
	backup, err := equal.ComparePageFiles(x.path("a-backup.db"), x.ref(quietStep), pageSize)
	if err != nil {
		x.lab.Fatal("E2", err)
	}
	fmt.Printf("backup API: %s\n", backup)
	backupCanonical, err := equal.ComparePageFiles(x.canonicalCapture(x.path("a-backup.db")), x.canonicalRef(quietStep), pageSize)
	if err != nil {
		x.lab.Fatal("E2 canonical", err)
	}
	fmt.Printf("backup API after VACUUM INTO vs the source after VACUUM INTO: %s\n", backupCanonical)
	backupCookie, sourceCookie := schemaCookie(x, x.path("a-backup.db")), schemaCookie(x, x.ref(quietStep))
	fmt.Printf("schema cookie: backup %d, source %d\n", backupCookie, sourceCookie)
	x.lab.Check(backupCanonical.Equal && slices.Equal(backupCanonical.IgnoredDiffering, []int{schemaCookieLowByte}) && backupCookie != sourceCookie,
		fmt.Sprintf("the backup's VACUUM INTO form differs from the source's only at byte %d, the schema cookie", schemaCookieLowByte))
	_, logical, vacuumed, err := equal.CompareLogicalFiles(x.ctx, x.path("a-vacuum.db"), x.ref(quietStep))
	if err != nil {
		x.lab.Fatal("E3", err)
	}
	fmt.Printf("VACUUM INTO: E3 differs only in journal_mode: %s for the copy, %s for the source\n", pragma(logical, "journal_mode"), pragma(vacuumed, "journal_mode"))
}

func schemaCookie(x *run, path string) uint32 {
	file, err := os.Open(path)
	if err != nil {
		x.lab.Fatal("open", err)
	}
	defer file.Close()
	header := make([]byte, 100)
	if _, err := io.ReadFull(file, header); err != nil {
		x.lab.Fatal("read header", err)
	}
	return binary.BigEndian.Uint32(header[40:44])
}

func pragma(l equal.Logical, name string) string {
	for _, p := range l.Pragmas {
		if p.Name == name {
			return p.Value
		}
	}
	return "missing"
}

func (x *run) openSource() {
	options := x.r.SQLiteOptions()
	options.Conn.WALAutocheckpoint = 0
	db, err := sqlitex.Open(x.ctx, x.live, options)
	if err != nil {
		x.lab.Fatal("open", err)
	}
	conn, err := db.Conn(x.ctx)
	if err != nil {
		x.lab.Fatal("conn", err)
	}
	x.source, x.conn = db, conn
}

func (x *run) closeSource() {
	if err := errors.Join(x.conn.Close(), x.source.Close()); err != nil {
		x.lab.Fatal("close", err)
	}
}

var (
	everyLevel   = levels{true, true, true, true, true}
	backupLevels = levels{false, true, true, true, false}
	vacuumLevels = levels{false, false, false, true, true}
)

func (x *run) report(method, file string, vacuumed bool, want int, wantLevels levels) bool {
	k, l := x.stateOf(x.path(file), vacuumed)
	fmt.Printf("%-36s equals recipe step %d (want %d): %s\n", method, k, want, l)
	ok := k == want && l == wantLevels
	x.lab.Check(ok, fmt.Sprintf("under the writer, %s equals recipe step %d at exactly the expected levels", method, want))
	return ok
}

func (x *run) underWriter() map[string]bool {
	fmt.Println()
	fmt.Println("== part B: the same database while a writer child commits recipe steps (one fresh connection per step, wal_autocheckpoint=0)")
	x.writer = x.lab.SpawnSQL("writer", procs.SQLOptions{Path: x.live, JournalMode: sqlitex.JournalWAL, BusyTimeout: 5 * time.Second})
	x.openSource()
	x.next = quietStep + 1
	safe := map[string]bool{}

	fmt.Println("-- plain copy of the live database file (the -wal is not copied)")
	current := x.commit()
	x.copyLive("", "b-copy.db")
	stale, staleLevels := x.stateOf(x.path("b-copy.db"), false)
	fmt.Printf("%-36s equals recipe step %d (the writer has committed %d): %s\n", "plain copy of the live file", stale, current, staleLevels)
	x.lab.Check(stale >= 0 && stale < current && staleLevels == everyLevel,
		fmt.Sprintf("the plain copy is byte-identical (E1) to an older recipe step (%d), not to the committed step %d", stale, current))
	safe[methodCopy] = stale == current

	fmt.Println("-- every page through sqlite_dbpage in one read transaction, in two halves; the writer commits between them")
	seen := x.begin()
	pages := x.pageCount()
	image := x.readPages(1, pages/2)
	x.commit()
	image = append(image, x.readPages(pages/2+1, pages)...)
	x.end()
	if err := walship.WriteDatabase(x.path("b-pages.db"), image); err != nil {
		x.lab.Fatal("write", err)
	}
	safe[methodPages] = x.report(methodPages, "b-pages.db", false, seen, everyLevel)

	fmt.Println("-- copy of the database file, a commit, then a copy of the -wal, all inside one read transaction; rebuilt by WAL replay")
	seen = x.begin()
	x.copyLive("", "b-file.db")
	afterCopy := x.commit()
	x.copyLive("-wal", "b-file.wal")
	x.end()
	if _, err := walfile.ReplayFiles(x.path("b-file.db"), x.path("b-file.wal"), x.path("b-replayed.db")); err != nil {
		x.lab.Fatal("replay", err)
	}
	safe[methodFile] = x.report(methodFile, "b-replayed.db", false, afterCopy, everyLevel) && afterCopy >= seen

	fmt.Println("-- VACUUM INTO while the writer child holds the write lock with step uncommitted, then the writer commits")
	before := x.next - 1
	x.lab.Must(x.writer, "begin", "immediate")
	x.lab.Must(x.writer, "recipe-exec", strconv.Itoa(x.next))
	if _, err := x.conn.ExecContext(x.ctx, "VACUUM INTO ?", x.path("b-vacuum.db")); err != nil {
		x.lab.Fatal("vacuum into", err)
	}
	fmt.Println("VACUUM INTO finished while the writer held its write transaction open")
	x.lab.Must(x.writer, "commit")
	x.next++
	safe[methodVacuum] = x.report(methodVacuum, "b-vacuum.db", true, before, vacuumLevels)

	safe[methodBackup] = x.backups()
	x.closeSource()
	x.lab.StopChildren()
	return safe
}

func (x *run) backups() bool {
	fmt.Printf("-- backup API, %d pages per step; after step 2 and step 3 the backup's own connection commits a recipe step, after step 5 the writer child does\n", backupPages)
	var copied []int
	steps, err := sqlitex.BackupWithSourceExec(x.ctx, x.conn, x.path("b-backup.db"), backupPages, func(step sqlitex.BackupStep, exec sqlitex.SourceExecer) error {
		copied = append(copied, step.PageCount-step.Remaining)
		note := ""
		switch step.Step {
		case 2, 3:
			planned, err := x.r.Step(x.next)
			if err != nil {
				return err
			}
			if err := applyThrough(x.ctx, exec, planned); err != nil {
				return err
			}
			note = fmt.Sprintf("; the backup's own connection commits step %d", x.next)
			x.next++
		case 5:
			note = fmt.Sprintf("; the writer child commits step %d", x.next)
			x.commit()
		}
		fmt.Printf("   backup step %2d: remaining %3d of %3d, copied %3d%s\n", step.Step, step.Remaining, step.PageCount, step.PageCount-step.Remaining, note)
		return nil
	})
	if err != nil {
		x.lab.Fatal("backup", err)
	}
	finished := steps[len(steps)-1].Remaining == 0
	x.lab.Check(copied[0] == backupPages && copied[1] == 2*backupPages && copied[2] == 3*backupPages && copied[3] == 4*backupPages && copied[4] == 5*backupPages,
		fmt.Sprintf("through the backup's own commits the copy keeps advancing %d pages per step (no restart), although the remaining count rose when those commits grew the database", backupPages))
	x.lab.Check(copied[5] == backupPages, "after the writer child's commit the next step starts again from page 1: the backup restarted")
	var remaining []int
	ok := x.report(methodBackup, "b-backup.db", false, x.next-1, backupLevels) && finished

	fmt.Printf("-- backup API, %d pages per step, the writer child commits after every step; give up after %d steps\n", backupPages, giveUpAfter)
	_, err = sqlitex.Backup(x.ctx, x.conn, x.path("b-backup-never.db"), backupPages, func(step sqlitex.BackupStep) error {
		remaining = append(remaining, step.Remaining)
		fmt.Printf("   backup step %2d: remaining %2d of %2d; the writer child commits step %d\n", step.Step, step.Remaining, step.PageCount, x.next)
		x.commit()
		if step.Step == giveUpAfter {
			return errGaveUp
		}
		return nil
	})
	x.lab.Check(errors.Is(err, errGaveUp) && minimum(remaining) > 0, fmt.Sprintf("with a commit by another connection after every step the backup never finished: after %d steps the smallest remaining count was %d", giveUpAfter, minimum(remaining)))
	return ok
}

func applyThrough(ctx context.Context, exec sqlitex.SourceExecer, step recipe.Step) error {
	if _, err := exec.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	if err := step.ExecIn(ctx, exec); err != nil {
		_, rollbackErr := exec.ExecContext(ctx, "ROLLBACK")
		return errors.Join(err, rollbackErr)
	}
	_, err := exec.ExecContext(ctx, "COMMIT")
	return err
}

func minimum(values []int) int {
	m := values[0]
	for _, v := range values {
		m = min(m, v)
	}
	return m
}

func main() {
	if procs.Serve(procs.Roles) || walfile.ServeLiveCopyChild(os.Args) {
		return
	}
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("9.1")
	fmt.Printf("INFO: work dir %s\n", sqlitex.DisplayPath(dir))
	x := &run{ctx: ctx, lab: procs.NewLab(os.Stdout), r: recipe.Default(), dir: dir, live: filepath.Join(dir, "live.db")}
	x.buildReferences()
	x.quiet()
	safe := x.underWriter()

	fmt.Println()
	fmt.Println("== summary")
	fmt.Printf("%-36s | %-34s | %-17s | %s\n", "method", "strongest level", "safe under writer", "blocks the writer")
	rows := []struct{ method, level, blocks string }{
		{methodCopy, "E1", "the database must be closed"},
		{methodBackup, "E2", "no (commits ran between steps)"},
		{methodVacuum, "E3 but journal_mode; E1 canonical", "no (ran while the writer held its write transaction)"},
		{methodPages, "E1", "no (a commit ran inside the read transaction)"},
		{methodFile, "E1 after WAL replay", "no (a commit ran inside the read transaction)"},
	}
	for _, row := range rows {
		fmt.Printf("%-36s | %-34s | %-17s | %s\n", row.method, row.level, yesNo(safe[row.method]), row.blocks)
	}
	x.lab.Check(!safe[methodCopy] && safe[methodBackup] && safe[methodVacuum] && safe[methodPages] && safe[methodFile],
		"under the writer every capture except the plain file copy equals the exact recipe state it should; the plain copy equals an older one")
	x.lab.Report(strings.ToUpper("capture comparison"))
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
