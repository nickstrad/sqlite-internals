// Command atrest-and-fence-edges attacks internal/walship/atrest.go (AdoptAtRest)
// and boundary conditions in service/fence and service/s3store.
package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/fence"
	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/s3store"
	"github.com/nickstrad/sqlite-internals/internal/rangefs"
	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
	"github.com/nickstrad/sqlite-internals/internal/walship"
)

func report(id, tried, expect, got string, held bool) {
	status := "BROKEN"
	if held {
		status = "HELD"
	}
	fmt.Printf("ATTACK %s | %s | EXPECT %s | GOT %s | %s\n", id, tried, expect, got, status)
}

func fatal(what string, err error) {
	fmt.Fprintf(os.Stderr, "FATAL %s: %v\n", what, err)
	os.Exit(1)
}

func scratchDir(root, name string) string {
	dir := filepath.Join(root, "_work", "adv-ch10a", "atrest-and-fence-edges", name)
	if err := os.RemoveAll(dir); err != nil {
		fatal("clean scratch "+name, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("mkdir scratch "+name, err)
	}
	return dir
}

func restoreAllAndCompare(ctx context.Context, store walship.Store, dbPath string, lastPos int64) (bool, string) {
	cat, err := walship.LoadCatalog(ctx, store)
	if err != nil {
		return false, fmt.Sprintf("LoadCatalog: %v", err)
	}
	var lastImg []byte
	for p := int64(1); p <= lastPos; p++ {
		plan, err := cat.Plan(p)
		if err != nil {
			return false, fmt.Sprintf("Plan(%d): %v", p, err)
		}
		img, _, err := plan.Execute()
		if err != nil {
			return false, fmt.Sprintf("Execute(%d): %v", p, err)
		}
		lastImg = img
	}
	diskBytes, err := os.ReadFile(dbPath)
	if err != nil {
		return false, fmt.Sprintf("ReadFile: %v", err)
	}
	if !bytes.Equal(diskBytes, lastImg) {
		return false, fmt.Sprintf("E1 mismatch: disk=%s restored=%s", walship.SHA256Hex(diskBytes)[:12], walship.SHA256Hex(lastImg)[:12])
	}
	return true, "ok"
}

func attackATREST01(ctx context.Context, root string) {
	// Normal clean-close reopen AND post-VACUUM shrink + close + AdoptAtRest + subsequent commit.
	work := scratchDir(root, "atrest01")
	dbPath := filepath.Join(work, "shrink.db")
	workload := recipe.Default()
	if err := workload.Build(ctx, dbPath, 10); err != nil {
		fatal("atrest01 build", err)
	}
	store, err := walship.NewDirStore(filepath.Join(work, "store"))
	if err != nil {
		fatal("atrest01 store", err)
	}
	opts := workload.SQLiteOptions()
	opts.Conn.WALAutocheckpoint = 0

	s1, err := walship.Open(ctx, walship.Config{Database: dbPath, Store: store, Options: opts, Origin: 1})
	if err != nil {
		fatal("atrest01 open s1", err)
	}
	// Delete most rows and VACUUM to shrink the database substantially, then ship and close.
	w1, err := sqlitex.Open(ctx, dbPath, opts)
	if err != nil {
		fatal("atrest01 open w1", err)
	}
	if _, err := w1.ExecContext(ctx, "DELETE FROM events WHERE id > 5; DELETE FROM documents; VACUUM;"); err != nil {
		fatal("atrest01 shrink", err)
	}
	_ = w1.Close()
	rep1, err := s1.Cycle(ctx)
	if err != nil {
		fatal("atrest01 cycle 1", err)
	}
	if err := s1.Close(); err != nil {
		fatal("atrest01 close s1", err)
	}

	// Reopen, AdoptAtRest, commit one more step, verify no snapshot uploaded and full E1 restore.
	s2, err := walship.Open(ctx, walship.Config{Database: dbPath, Store: store, Options: opts})
	if err != nil {
		fatal("atrest01 open s2", err)
	}
	adopted, adoptErr := s2.AdoptAtRest(ctx)
	w2, err := sqlitex.Open(ctx, dbPath, opts)
	if err != nil {
		fatal("atrest01 open w2", err)
	}
	if _, err := w2.ExecContext(ctx, "INSERT INTO accounts (name, balance) VALUES ('post-vacuum', 42); PRAGMA user_version = 11;"); err != nil {
		fatal("atrest01 post-vacuum write", err)
	}
	_ = w2.Close()
	rep2, cycleErr := s2.Cycle(ctx)
	lastPos := s2.Position()
	if err := s2.Close(); err != nil {
		fatal("atrest01 close s2", err)
	}

	e1OK, e1Msg := restoreAllAndCompare(ctx, store, dbPath, lastPos)
	held := adoptErr == nil && adopted && cycleErr == nil && rep1.Segment != nil &&
		rep2.Snapshot == nil && rep2.Segment != nil && rep2.Change == walship.WALNewGeneration &&
		rep2.Segment.FirstFrame == 1 && e1OK
	report(
		"ATREST-01",
		"shrink DB via DELETE+VACUUM, ship, close, reopen with AdoptAtRest, commit again",
		"AdoptAtRest=true, next Cycle ships WALNewGeneration FirstFrame=1 (no snapshot), E1 restore matches",
		fmt.Sprintf("adopted=%t adoptErr=%v change=%q snap=%v firstFrame=%d pos=%d e1=%s",
			adopted, adoptErr, rep2.Change, rep2.Snapshot != nil, func() int {
				if rep2.Segment != nil {
					return rep2.Segment.FirstFrame
				}
				return -1
			}(), lastPos, e1Msg),
		held,
	)
}

func attackATREST02(ctx context.Context, root string) {
	// Tricky non-committed WAL states for AdoptAtRest:
	// (a) rolled-back spilled write leaving a live -wal with header + uncommitted frames (lastCommit==0);
	// (b) 10-byte truncated -wal (ErrWALIgnored);
	// (c) outside TRUNCATE checkpoint while shipper holds slot 0.
	workload := recipe.Default()
	opts := workload.SQLiteOptions()
	opts.Conn.WALAutocheckpoint = 0

	// (a) Rolled-back spilled transaction leaving uncommitted frames in -wal.
	workA := scratchDir(root, "atrest02a")
	dbA := filepath.Join(workA, "a.db")
	if err := workload.Build(ctx, dbA, 1); err != nil {
		fatal("atrest02a build", err)
	}
	storeA, err := walship.NewDirStore(filepath.Join(workA, "store"))
	if err != nil {
		fatal("atrest02a store", err)
	}
	sA1, err := walship.Open(ctx, walship.Config{Database: dbA, Store: storeA, Options: opts, Origin: 1})
	if err != nil {
		fatal("atrest02a open s1", err)
	}
	if err := sA1.Close(); err != nil {
		fatal("atrest02a close s1", err)
	}

	// Open a writer with tiny cache_size=2, spill frames into -wal, ROLLBACK, and keep a reader open
	// so -wal stays on disk with a valid 32-byte header + uncommitted frames and 0 commit frames!
	spillOpts := opts
	spillOpts.Conn.CacheSize = 2
	wSpill, err := sqlitex.Open(ctx, dbA, spillOpts)
	if err != nil {
		fatal("atrest02a open spill", err)
	}
	keeper, err := sqlitex.Open(ctx, dbA, opts)
	if err != nil {
		fatal("atrest02a open keeper", err)
	}
	if _, err := keeper.ExecContext(ctx, "SELECT count(*) FROM sqlite_schema"); err != nil {
		fatal("atrest02a keeper select", err)
	}
	tx, err := wSpill.BeginTx(ctx, nil)
	if err != nil {
		fatal("atrest02a begin spill", err)
	}
	big := strings.Repeat("Z", 12000)
	for i := 0; i < 12; i++ {
		if _, err := tx.ExecContext(ctx, "INSERT INTO documents (account_id, title, body) VALUES (1, ?, ?)", fmt.Sprintf("rb-%d", i), big); err != nil {
			fatal("atrest02a spill insert", err)
		}
	}
	if err := tx.Rollback(); err != nil {
		fatal("atrest02a rollback", err)
	}
	_ = wSpill.Close()
	walInfoA, _ := os.Stat(dbA + "-wal")
	walSizeA := int64(0)
	if walInfoA != nil {
		walSizeA = walInfoA.Size()
	}

	sA2, err := walship.Open(ctx, walship.Config{Database: dbA, Store: storeA, Options: opts})
	if err != nil {
		fatal("atrest02a reopen", err)
	}
	_ = keeper.Close()
	adoptedA, adoptErrA := sA2.AdoptAtRest(ctx)
	wNextA, err := sqlitex.Open(ctx, dbA, opts)
	if err != nil {
		fatal("atrest02a open next writer", err)
	}
	if err := workload.ApplyStep(ctx, wNextA, 2); err != nil {
		fatal("atrest02a apply step 2", err)
	}
	_ = wNextA.Close()
	repA, cycleErrA := sA2.Cycle(ctx)
	posA := sA2.Position()
	_ = sA2.Close()
	e1A, e1MsgA := restoreAllAndCompare(ctx, storeA, dbA, posA)

	// (b) 10-byte truncated -wal (ErrWALIgnored) beside at-rest .db.
	workB := scratchDir(root, "atrest02b")
	dbB := filepath.Join(workB, "b.db")
	if err := workload.Build(ctx, dbB, 0); err != nil {
		fatal("atrest02b build", err)
	}
	storeB, err := walship.NewDirStore(filepath.Join(workB, "store"))
	if err != nil {
		fatal("atrest02b store", err)
	}
	sB1, err := walship.Open(ctx, walship.Config{Database: dbB, Store: storeB, Options: opts, Origin: 1})
	if err != nil {
		fatal("atrest02b open s1", err)
	}
	_ = sB1.Close()
	if err := os.WriteFile(dbB+"-wal", []byte("0123456789"), 0o644); err != nil {
		fatal("atrest02b write stub wal", err)
	}
	sB2, err := walship.Open(ctx, walship.Config{Database: dbB, Store: storeB, Options: opts})
	if err != nil {
		fatal("atrest02b reopen", err)
	}
	adoptedB, adoptErrB := sB2.AdoptAtRest(ctx)
	wNextB, err := sqlitex.Open(ctx, dbB, opts)
	if err != nil {
		fatal("atrest02b open writer", err)
	}
	if err := workload.ApplyStep(ctx, wNextB, 1); err != nil {
		fatal("atrest02b step 1", err)
	}
	_ = wNextB.Close()
	repB, cycleErrB := sB2.Cycle(ctx)
	posB := sB2.Position()
	_ = sB2.Close()
	e1B, e1MsgB := restoreAllAndCompare(ctx, storeB, dbB, posB)

	// (c) Outside TRUNCATE checkpoint while live shipper holds slot 0 after full backfill.
	workC := scratchDir(root, "atrest02c")
	dbC := filepath.Join(workC, "c.db")
	if err := workload.Build(ctx, dbC, 0); err != nil {
		fatal("atrest02c build", err)
	}
	storeC, err := walship.NewDirStore(filepath.Join(workC, "store"))
	if err != nil {
		fatal("atrest02c store", err)
	}
	sC, err := walship.Open(ctx, walship.Config{Database: dbC, Store: storeC, Options: opts, Origin: 1})
	if err != nil {
		fatal("atrest02c open", err)
	}
	wC, err := sqlitex.Open(ctx, dbC, opts)
	if err != nil {
		fatal("atrest02c writer", err)
	}
	if err := workload.ApplyStep(ctx, wC, 1); err != nil {
		fatal("atrest02c step 1", err)
	}
	if _, err := sC.Cycle(ctx); err != nil {
		fatal("atrest02c cycle 1", err)
	}
	// Now shipper holds slot 0 (fully backfilled). Outside TRUNCATE truncates -wal to 0 bytes.
	if _, err := sqlitex.Checkpoint(ctx, wC, "", sqlitex.CheckpointTruncate); err != nil {
		fatal("atrest02c truncate", err)
	}
	adoptedC, adoptErrC := sC.AdoptAtRest(ctx)
	if err := workload.ApplyStep(ctx, wC, 2); err != nil {
		fatal("atrest02c step 2", err)
	}
	_ = wC.Close()
	repC, cycleErrC := sC.Cycle(ctx)
	posC := sC.Position()
	_ = sC.Close()
	e1C, e1MsgC := restoreAllAndCompare(ctx, storeC, dbC, posC)

	held := adoptErrA == nil && adoptedA && cycleErrA == nil && repA.Snapshot == nil && e1A &&
		adoptErrB == nil && adoptedB && cycleErrB == nil && repB.Snapshot == nil && e1B &&
		adoptErrC == nil && adoptedC && cycleErrC == nil && repC.Snapshot == nil && e1C
	report(
		"ATREST-02",
		"AdoptAtRest with (a) rolled-back spilled -wal (lastCommit=0), (b) 10-byte stub -wal, (c) outside TRUNCATE on slot 0",
		"all three adopt cleanly without discontinuity snapshot and restore E1 byte-for-byte",
		fmt.Sprintf("rollback(walBytes=%d,adopted=%t,snap=%v,e1=%s) stub10B(adopted=%t,snap=%v,e1=%s) extTrunc(adopted=%t,snap=%v,e1=%s)",
			walSizeA, adoptedA, repA.Snapshot != nil, e1MsgA,
			adoptedB, repB.Snapshot != nil, e1MsgB,
			adoptedC, repC.Snapshot != nil, e1MsgC),
		held,
	)
}

func attackATREST03(ctx context.Context, root string) {
	// Must-refuse & error cases for AdoptAtRest:
	// (a) 1-bit flip in unused reserved header byte (page 1 offset 80; integrity_check still ok) -> adopted=false;
	// (b) unshipped committed frame in -wal -> adopted=false;
	// (c) canceled context -> error AND marks shipper broken.
	workload := recipe.Default()
	opts := workload.SQLiteOptions()
	opts.Conn.WALAutocheckpoint = 0

	// (a) 1-bit flip in page 1 reserved byte 80 while closed.
	workA := scratchDir(root, "atrest03a")
	dbA := filepath.Join(workA, "a.db")
	if err := workload.Build(ctx, dbA, 0); err != nil {
		fatal("atrest03a build", err)
	}
	storeA, err := walship.NewDirStore(filepath.Join(workA, "store"))
	if err != nil {
		fatal("atrest03a store", err)
	}
	sA1, err := walship.Open(ctx, walship.Config{Database: dbA, Store: storeA, Options: opts, Origin: 1})
	if err != nil {
		fatal("atrest03a open", err)
	}
	_ = sA1.Close()

	raw, err := os.ReadFile(dbA)
	if err != nil {
		fatal("atrest03a read", err)
	}
	raw[80] ^= 0x01 // reserved zero byte; SQLite integrity_check ignores it
	if err := os.WriteFile(dbA, raw, 0o644); err != nil {
		fatal("atrest03a write flipped", err)
	}
	// Verify SQLite integrity_check still reports ok on the flipped file.
	chkDB, err := sqlitex.Open(ctx, dbA, opts)
	if err != nil {
		fatal("atrest03a open chk", err)
	}
	var ic string
	_ = chkDB.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&ic)
	_ = chkDB.Close()

	sA2, err := walship.Open(ctx, walship.Config{Database: dbA, Store: storeA, Options: opts})
	if err != nil {
		fatal("atrest03a reopen", err)
	}
	adoptedFlip, errFlip := sA2.AdoptAtRest(ctx)
	_ = sA2.Close()

	// (b) Unshipped committed frame in -wal (lastCommit > 0).
	workB := scratchDir(root, "atrest03b")
	dbB := filepath.Join(workB, "b.db")
	if err := workload.Build(ctx, dbB, 0); err != nil {
		fatal("atrest03b build", err)
	}
	storeB, err := walship.NewDirStore(filepath.Join(workB, "store"))
	if err != nil {
		fatal("atrest03b store", err)
	}
	sB1, err := walship.Open(ctx, walship.Config{Database: dbB, Store: storeB, Options: opts, Origin: 1})
	if err != nil {
		fatal("atrest03b open", err)
	}
	_ = sB1.Close()
	wB, err := sqlitex.Open(ctx, dbB, opts)
	if err != nil {
		fatal("atrest03b writer", err)
	}
	if err := workload.ApplyStep(ctx, wB, 1); err != nil {
		fatal("atrest03b step 1", err)
	}
	// Keep wB open so -wal stays on disk with committed frames when sB2 opens.
	sB2, err := walship.Open(ctx, walship.Config{Database: dbB, Store: storeB, Options: opts})
	if err != nil {
		fatal("atrest03b reopen", err)
	}
	adoptedWAL, errWAL := sB2.AdoptAtRest(ctx)
	_ = wB.Close()

	// (c) Canceled context passed to AdoptAtRest marks shipper broken.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, errCancel := sB2.AdoptAtRest(canceled)
	_, errAfterBroken := sB2.AdoptAtRest(ctx)
	_, errCycleBroken := sB2.Cycle(ctx)
	_ = sB2.Close()

	held := ic == "ok" && errFlip == nil && !adoptedFlip &&
		errWAL == nil && !adoptedWAL &&
		errCancel != nil && errAfterBroken != nil && errCycleBroken != nil &&
		strings.Contains(errAfterBroken.Error(), "earlier cycle failed")
	report(
		"ATREST-03",
		"AdoptAtRest must-refuse: 1-bit flip in unused header byte 80 (integrity_check=ok), unshipped -wal, canceled ctx",
		"1-bit flip refused (adopted=false), unshipped -wal refused (adopted=false), canceled ctx marks shipper broken",
		fmt.Sprintf("flip(ic=%s,adopted=%t,err=%v) wal(adopted=%t,err=%v) cancel(err=%v,brokenAdopt=%v,brokenCycle=%v)",
			ic, adoptedFlip, errFlip, adoptedWAL, errWAL, errCancel != nil, errAfterBroken != nil, errCycleBroken != nil),
		held,
	)
}

func attackFENCEEDGE01(ctx context.Context, root string) {
	// fence.Adapter & s3store boundary checks:
	// (a) same-epoch different-bytes collision -> ErrExists (not nil, not ErrFenced);
	// (b) same-epoch identical-bytes retry -> nil & advances end;
	// (c) unreachable S3 endpoint on s3store.Connect AND 10.1-one-writer subprocess.
	work := scratchDir(root, "fence-edge01")
	store, err := walship.NewDirStore(filepath.Join(work, "store"))
	if err != nil {
		fatal("edge01 store", err)
	}
	makeSnap := func(tag byte) []byte {
		img := make([]byte, 4096)
		img[0] = tag
		obj, err := walship.EncodeSnapshot(walship.SnapshotMeta{
			Position: 1, Reason: "fence", PageSize: 4096, Pages: 1, StateSHA256: walship.SHA256Hex(img),
		}, img)
		if err != nil {
			fatal("edge01 encode snap", err)
		}
		return obj
	}
	snap1 := makeSnap(1)
	snap2 := makeSnap(2)

	a1, err := fence.New(store, "db-edge", 2)
	if err != nil {
		fatal("edge01 new a1", err)
	}
	if err := a1.PutIfAbsent(ctx, walship.SnapshotKey(1), snap1); err != nil {
		fatal("edge01 put snap1", err)
	}

	// Build a second epoch-2 adapter with a stale empty view (listed via fresh struct or stale store)
	// by hiding slot 1 on its first List call.
	hideOne := &hideOnceStore{Store: store}
	a2Diff, err := fence.New(hideOne, "db-edge", 2)
	if err != nil {
		fatal("edge01 new a2Diff", err)
	}
	errDiff := a2Diff.PutIfAbsent(ctx, walship.SnapshotKey(1), snap2)

	hideTwo := &hideOnceStore{Store: store}
	a2Same, err := fence.New(hideTwo, "db-edge", 2)
	if err != nil {
		fatal("edge01 new a2Same", err)
	}
	errSame := a2Same.PutIfAbsent(ctx, walship.SnapshotKey(1), snap1)

	// Extra envelope/Decode malformation checks beyond fence_test.go.
	key1 := fence.SlotKey("db-edge", 1)
	malformedOK := true
	for _, bad := range [][]byte{
		append([]byte("fenced-slot v1 epoch=1 kind=snapshot first=1 last=1 \n"), snap1...),
		append([]byte("fenced-slot v1 epoch=1 kind=snapshot first=-1 last=1\n"), snap1...),
		fence.Encode(1, fence.KindSegment, 1, 1, func() []byte {
			// Segment claiming positions 1..1 with empty commits list
			meta := walship.SegmentMeta{
				FirstPosition: 1, LastPosition: 1,
				Generation: walship.Generation{Header: hex.EncodeToString(make([]byte, 32))},
				FirstFrame: 1, LastFrame: 1, PageSize: 4096,
			}
			b, _ := walship.EncodeSegment(meta, make([]byte, 4096+24))
			return b
		}()),
	} {
		if _, err := fence.Decode(key1, bad); !errors.Is(err, fence.ErrDamaged) {
			malformedOK = false
		}
	}

	// Unreachable S3 endpoint check (closed port 127.0.0.1:1).
	badCfg := rangefs.S3ConfigFromEnv()
	badCfg.Endpoint = "http://127.0.0.1:1"
	_, unreachableErr := s3store.Connect(ctx, badCfg, "course-ch10a", "adv-unreachable")

	// Also verify 10.1-one-writer subprocess prints STORE UNREACHABLE and exits non-zero.
	bin := filepath.Join(work, "exp10-1")
	build := exec.Command("go", "build", "-o", bin, "./ch10-multi-database-platform-capstone/experiments/10.1-one-writer")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		fatal("edge01 build exp", fmt.Errorf("%v: %s", err, out))
	}
	cmd := exec.Command(bin)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "COURSE_S3_ENDPOINT=http://127.0.0.1:1")
	out, cmdErr := cmd.CombinedOutput()
	expUnreachOK := cmdErr != nil && strings.Contains(string(out), "STORE UNREACHABLE:")

	held := errors.Is(errDiff, walship.ErrExists) && !errors.Is(errDiff, fence.ErrFenced) &&
		errSame == nil && malformedOK &&
		errors.Is(unreachableErr, s3store.ErrUnreachable) && expUnreachOK
	report(
		"FENCE-EDGE-01",
		"same-epoch diff-bytes vs identical retry, malformed envelopes, and unreachable S3 endpoint (127.0.0.1:1)",
		"diff-bytes->ErrExists(!ErrFenced), identical->nil, malformed->ErrDamaged, closed port->ErrUnreachable + exp exit 1",
		fmt.Sprintf("diffErr=(exists=%t,fenced=%t) sameErr=%v malformedOK=%t s3Unreach=%t expUnreach=%t",
			errors.Is(errDiff, walship.ErrExists), errors.Is(errDiff, fence.ErrFenced),
			errSame, malformedOK, errors.Is(unreachableErr, s3store.ErrUnreachable), expUnreachOK),
		held,
	)
}

type hideOnceStore struct {
	walship.Store
	hid bool
}

func (h *hideOnceStore) List(ctx context.Context, prefix string) ([]string, error) {
	if !h.hid {
		h.hid = true
		return nil, nil
	}
	return h.Store.List(ctx, prefix)
}

func main() {
	ctx := context.Background()
	root, err := sqlitex.RepoRoot()
	if err != nil {
		fatal("repo root", err)
	}
	attackATREST01(ctx, root)
	attackATREST02(ctx, root)
	attackATREST03(ctx, root)
	attackFENCEEDGE01(ctx, root)
}
