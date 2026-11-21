// Command fleet-corruption-and-fencing attacks Chapter 10b's Experiment 10.4
// (rebuild-the-fleet), fence.Inspect, and lab.InspectFleet against real
// SeaweedFS S3: deleting or corrupting one segment of one database, injecting
// partial puts (and verifying they are never listed as restore points),
// rebuilding while old writer processes are still running (both deterministic
// and live concurrent race), and 10.4's scaled end-to-end + descriptor-limit
// guard.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/catalog"
	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/faultstore"
	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/fence"
	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/lab"
	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/node"
	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/s3store"
	"github.com/nickstrad/sqlite-internals/internal/connmgr"
	"github.com/nickstrad/sqlite-internals/internal/rangefs"
	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
	"github.com/nickstrad/sqlite-internals/internal/walship"
)

const s3Bucket = "course-ch10b"

var (
	role    = flag.String("role", "", "internal: run as a service node or checker child process")
	workers = flag.Int("checker-workers", 4, "internal: checker worker count")
	procCfg node.ProcessConfig
)

func init() {
	flag.StringVar(&procCfg.Name, "name", "", "internal: node name")
	flag.StringVar(&procCfg.Work, "work", "", "internal: work directory")
	flag.StringVar(&procCfg.Fault, "fault", "", "internal: fault rule")
	flag.StringVar(&procCfg.Bucket, "bucket", s3Bucket, "S3 bucket")
	flag.StringVar(&procCfg.Prefix, "prefix", "adv-ch10b-fleet", "S3 key prefix")
	flag.IntVar(&procCfg.Capacity, "capacity", 8, "node open database capacity")
	flag.BoolVar(&procCfg.AckBeforePut, "ack-before-put", false, "internal: unused")
}

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

func runPrefix(tag string) string {
	return fmt.Sprintf("adv-ch10b-%s-%d", tag, os.Getpid())
}

func scratchDir(root, name string) string {
	dir := filepath.Join(root, "_work", "adv-ch10b", fmt.Sprintf("fleet-corruption-and-fencing-%d", os.Getpid()), name)
	if err := os.RemoveAll(dir); err != nil {
		fatal("clean scratch "+name, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("mkdir scratch "+name, err)
	}
	return dir
}

func connectStore(ctx context.Context, prefix string) *s3store.Store {
	s, err := s3store.Connect(ctx, rangefs.S3ConfigFromEnv(), s3Bucket, prefix)
	if err != nil {
		fatal("connect s3 "+prefix, err)
	}
	return s
}

func nextStep(ctx context.Context, db *sqlitex.DB) error {
	_, err := recipe.Default().ApplyNext(ctx, db)
	return err
}

func newNode(ctx context.Context, work, name string, cat *catalog.Catalog, store walship.Store, capacity int) *node.Node {
	workload := recipe.Default()
	n, err := node.Start(node.Config{
		Name:     name,
		Dir:      filepath.Join(work, name),
		Catalog:  cat,
		Store:    store,
		Capacity: capacity,
		File:     workload.SQLiteOptions().File,
		Create:   func(ctx context.Context, path string) error { return workload.Build(ctx, path, 0) },
	})
	if err != nil {
		fatal("start node "+name, err)
	}
	return n
}

// buildFleet builds a clean N-database fleet on real SeaweedFS matching 10.4's
// shape (claim + steps 1..4 store-durable + half rolled out to step 5), closes
// the writer, and returns each database's quiesced at-rest SHA-256.
func buildFleet(ctx context.Context, work string, store *s3store.Store, count int) (*catalog.Catalog, []string, map[string]string) {
	if _, err := store.WipeInBatches(ctx); err != nil {
		fatal("wipe fleet store", err)
	}
	ids := lab.Ids("db", count)
	cat, err := catalog.Open(ctx, filepath.Join(work, "catalog.db"))
	if err != nil {
		fatal("open catalog", err)
	}
	if err := cat.RegisterAll(ctx, ids, node.PlatformSettings()); err != nil {
		fatal("register fleet", err)
	}
	w := newNode(ctx, work, "w1", cat, store, 4)
	for i, id := range ids {
		if _, err := w.Claim(ctx, id); err != nil {
			fatal("claim "+id, err)
		}
		writes := 4
		if i < count/2 {
			writes = 5
		}
		for range writes {
			if _, err := w.Write(ctx, id, node.StoreDurable, nextStep); err != nil {
				fatal("write "+id, err)
			}
		}
	}
	if err := w.Close(); err != nil {
		fatal("close fleet writer", err)
	}
	shas := map[string]string{}
	for _, id := range ids {
		q, err := lab.TruncateAndHash(ctx, filepath.Join(work, "w1", id+".db"))
		if err != nil {
			fatal("truncate "+id, err)
		}
		shas[id] = q.SHA256
	}
	return cat, ids, shas
}

// attackFleet01 builds a 16-database fleet on SeaweedFS S3 (scaled from 500)
// and tests deleting a segment (01a), corrupting a segment in both envelope and
// payload (01b), and deleting slot 1 (01c).
func attackFleet01(ctx context.Context, root string, hashes []recipe.FileHashes) {
	prefix := runPrefix("f01")
	work := scratchDir(root, "fleet01")
	store := connectStore(ctx, prefix)
	defer store.WipeInBatches(ctx)

	cat, ids, shas := buildFleet(ctx, work, store, 16)
	defer cat.Close()

	// Save original slot 3 of db-0007 so we can restore it between sub-attacks.
	targetID := "db-0007"
	slot3Key := fence.SlotKey(targetID, 3)
	origSlot3, err := store.Get(ctx, slot3Key)
	if err != nil {
		fatal("get orig slot 3", err)
	}

	// FLEET-01a: Delete segment 3 of db-0007.
	if err := store.Delete(ctx, slot3Key); err != nil {
		fatal("delete slot 3", err)
	}
	repA, err := lab.InspectFleet(ctx, store, hashes, 4)
	if err != nil {
		fatal("inspect fleet 01a", err)
	}
	// Also attempt rebuilding all 16 databases onto empty disks: db-0007 must fail
	// with ErrBadChain, while all other 15 rebuild byte-identical (E1).
	freshA := newNode(ctx, work, "fresh-a", cat, store, 4)
	rebuiltOK, targetClaimErr := 0, error(nil)
	for _, id := range ids {
		_, cerr := freshA.Claim(ctx, id)
		if id == targetID {
			targetClaimErr = cerr
			continue
		}
		if cerr != nil {
			continue
		}
		st, serr := connmgr.CheckAtRest(filepath.Join(work, "fresh-a", id+".db"))
		if serr == nil && st.AtRest && st.SHA256 == shas[id] {
			rebuiltOK++
		}
	}
	freshA.Close()

	isolatedA := repA.Databases == 16 && len(repA.Failed) == 1 &&
		repA.Failed[0].Database == targetID &&
		repA.Failed[0].LastGood == 2 &&
		repA.Failed[0].RestorePoints == 2 &&
		strings.Contains(repA.Failed[0].Problem, "position 4 follows 2")
	heldA := isolatedA && errors.Is(targetClaimErr, fence.ErrBadChain) && rebuiltOK == 15
	report("FLEET-01a",
		"delete slot 3 of db-0007 in 16-DB SeaweedFS fleet (scaled from 500), inspect fleet and rebuild all 16",
		"InspectFleet isolates db-0007 (LastGood=2, gap reported); fresh Claim rejects db-0007 (ErrBadChain) and rebuilds 15/15 others E1",
		fmt.Sprintf("failed=%d(%v) targetClaimErr=%v othersRebuiltE1=%d/15",
			len(repA.Failed), repA.Failed, targetClaimErr, rebuiltOK),
		heldA)

	// FLEET-01b: Corrupt slot 3 of db-0007 in two ways: (1) envelope header byte,
	// (2) deep WAL-frame page payload bit-flip (last byte of the object).
	var corruptProblems []string
	for _, mode := range []string{"envelope", "payload"} {
		_ = store.Delete(ctx, slot3Key)
		corrupted := append([]byte(nil), origSlot3...)
		if mode == "envelope" {
			// Flip a digit in the envelope's epoch/first line.
			corrupted[0] ^= 0x01
		} else {
			// Flip 1 bit in the last byte (WAL frame page payload).
			corrupted[len(corrupted)-1] ^= 0x01
		}
		if err := store.PutIfAbsent(ctx, slot3Key, corrupted); err != nil {
			fatal("put corrupted "+mode, err)
		}
		repB, err := lab.InspectFleet(ctx, store, hashes, 4)
		if err != nil {
			corruptProblems = append(corruptProblems, fmt.Sprintf("%s: InspectFleet err=%v", mode, err))
			continue
		}
		if repB.Databases != 16 || len(repB.Failed) != 1 ||
			repB.Failed[0].Database != targetID ||
			repB.Failed[0].LastGood != 2 ||
			repB.Failed[0].RestorePoints != 2 ||
			!strings.Contains(repB.Failed[0].String(), "unrecoverable past position 2 (last good restore point 2)") {
			corruptProblems = append(corruptProblems, fmt.Sprintf("%s: unexpected report failed=%v", mode, repB.Failed))
		}
	}
	// Restore clean slot 3 before FLEET-01c.
	_ = store.Delete(ctx, slot3Key)
	if err := store.PutIfAbsent(ctx, slot3Key, origSlot3); err != nil {
		fatal("restore slot 3", err)
	}
	report("FLEET-01b",
		"corrupt slot 3 of db-0007 (1: envelope byte, 2: deep WAL-frame page-payload bit-flip) in 16-DB fleet",
		"both corruptions isolate db-0007 as unrecoverable past position 2 (LastGood=2) while 15 other DBs pass",
		fmt.Sprintf("modes=2 problems=%d detail=%q", len(corruptProblems), strings.Join(corruptProblems, "; ")),
		len(corruptProblems) == 0)

	// FLEET-01c: Delete slot 1 (opening fence snapshot) of db-0003.
	slot1Key := fence.SlotKey("db-0003", 1)
	origSlot1, err := store.Get(ctx, slot1Key)
	if err != nil {
		fatal("get orig slot 1", err)
	}
	if err := store.Delete(ctx, slot1Key); err != nil {
		fatal("delete slot 1", err)
	}
	repC, err := lab.InspectFleet(ctx, store, hashes, 4)
	if err != nil {
		fatal("inspect fleet 01c", err)
	}
	heldC := repC.Databases == 16 && len(repC.Failed) == 1 &&
		repC.Failed[0].Database == "db-0003" &&
		repC.Failed[0].LastGood == 0 &&
		repC.Failed[0].RestorePoints == 0
	report("FLEET-01c",
		"delete opening fence snapshot (slot 1) of db-0003 in 16-DB fleet",
		"InspectFleet isolates db-0003 with LastGood=0, RestorePoints=0 while 15 other DBs pass",
		fmt.Sprintf("databases=%d failed=%d(%v)", repC.Databases, len(repC.Failed), repC.Failed),
		heldC)

	// Restore db-0003 slot 1 before FLEET-01d.
	if err := store.PutIfAbsent(ctx, slot1Key, origSlot1); err != nil {
		fatal("restore slot 1", err)
	}

	// FLEET-01d (must-be-able-to-fail on recipe-state check): ship a valid SQLite
	// commit NOT in the recipe on db-0005. fence.Audit must pass (valid envelope,
	// WAL checksums, segment SHA-256, and lineage), while lab.InspectFleet must
	// still isolate db-0005 with LastGood equal to the last valid recipe position.
	rogueID := "db-0005"
	rogueNode := newNode(ctx, work, "rogue", cat, store, 2)
	claimRes, err := rogueNode.Claim(ctx, rogueID)
	if err != nil {
		rogueNode.Close()
		fatal("rogue claim "+rogueID, err)
	}
	wantGoodD := claimRes.Fence
	_, rogueErr := rogueNode.Write(ctx, rogueID, node.StoreDurable, func(ctx context.Context, db *sqlitex.DB) error {
		_, err := db.ExecContext(ctx, "CREATE TABLE rogue_non_recipe (x INTEGER); INSERT INTO rogue_non_recipe VALUES (1)")
		return err
	})
	rogueNode.Close()
	if rogueErr != nil {
		fatal("rogue write "+rogueID, rogueErr)
	}
	_, _, auditErrD := fence.Audit(ctx, store, rogueID)
	repD, err := lab.InspectFleet(ctx, store, hashes, 4)
	if err != nil {
		fatal("inspect fleet 01d", err)
	}
	heldD := auditErrD == nil &&
		repD.Databases == 16 && len(repD.Failed) == 1 &&
		repD.Failed[0].Database == rogueID &&
		repD.Failed[0].LastGood == wantGoodD &&
		int64(repD.Failed[0].RestorePoints) == wantGoodD &&
		strings.Contains(repD.Failed[0].Problem, "not a recipe state")
	report("FLEET-01d",
		"ship a valid SQLite non-recipe commit on db-0005 in 16-DB fleet (valid envelope, WAL checksums, and lineage)",
		fmt.Sprintf("fence.Audit succeeds, while InspectFleet isolates db-0005 with LastGood=%d (not a recipe state) and 15/15 others pass", wantGoodD),
		fmt.Sprintf("auditErr=%v failed=%d(%v)", auditErrD, len(repD.Failed), repD.Failed),
		heldD)
}

// attackFleet02 injects faultstore.PartialPut on a segment (db-0004 slot 3) and
// on an opening snapshot (db-0009 slot 1) in a 12-DB SeaweedFS fleet, verifying
// via both lab.InspectFleet and a real -role checker subprocess that partial
// puts are never listed as restore points and that a stray .part key under log/
// is isolated to its database.
func attackFleet02(ctx context.Context, root string, hashes []recipe.FileHashes) {
	prefix := runPrefix("f02")
	work := scratchDir(root, "fleet02")
	store := connectStore(ctx, prefix)
	defer store.WipeInBatches(ctx)
	if _, err := store.WipeInBatches(ctx); err != nil {
		fatal("wipe f02", err)
	}
	ids := lab.Ids("db", 12)
	cat, err := catalog.Open(ctx, filepath.Join(work, "catalog.db"))
	if err != nil {
		fatal("open cat", err)
	}
	defer cat.Close()
	if err := cat.RegisterAll(ctx, ids, node.PlatformSettings()); err != nil {
		fatal("register", err)
	}

	faultyStore := faultstore.New(store,
		faultstore.Rule{Fault: faultstore.PartialPut, Match: "db-0004/log/000000000003", Nth: 1},
		faultstore.Rule{Fault: faultstore.PartialPut, Match: "db-0009/log/000000000001", Nth: 1},
	)
	w := newNode(ctx, work, "w1", cat, faultyStore, 4)
	var err04, err09 error
	for _, id := range ids {
		if _, err := w.Claim(ctx, id); err != nil {
			if id == "db-0009" {
				err09 = err
				continue
			}
			fatal("claim "+id, err)
		}
		for range 3 {
			if _, err := w.Write(ctx, id, node.StoreDurable, nextStep); err != nil {
				if id == "db-0004" {
					err04 = err
					break
				}
				fatal("write "+id, err)
			}
		}
	}
	w.Close()

	// Check via lab.InspectFleet.
	rep, err := lab.InspectFleet(ctx, store, hashes, 4)
	if err != nil {
		fatal("inspect fleet 02", err)
	}
	// Expected restore points: 10 healthy DBs * 4 positions (slots 1..4) + db-0004 * 2 (slots 1..2) + db-0009 * 0 = 42.
	wantPoints := 10*4 + 2 + 0
	byDB := map[string]fence.Inspection{}
	for _, f := range rep.Failed {
		byDB[f.Database] = f
	}
	f04, ok04 := byDB["db-0004"]
	f09, ok09 := byDB["db-0009"]

	// Also verify via a real -role checker subprocess (line protocol + ParsePass + standing bounds).
	checkerArgs := []string{"-role", "checker", "-bucket", s3Bucket, "-prefix", prefix, "-checker-workers", "4"}
	checker, err := node.StartProcess(ctx, "checker", checkerArgs...)
	if err != nil {
		fatal("start checker", err)
	}
	passReply, err := checker.Send("pass")
	if err != nil {
		checker.Kill()
		fatal("checker pass", err)
	}
	parsedPass, parseErr := lab.ParsePass(passReply)
	closeReply, closeErr := checker.Close()

	// Also put a stray incomplete key under db-0004/log/ and verify InspectFleet isolates it to db-0004.
	if err := store.PutIfAbsent(ctx, "dbs/db-0004/log/000000000003-part", []byte("partial-garbage")); err != nil {
		fatal("put stray -part", err)
	}
	repStray, strayErr := lab.InspectFleet(ctx, store, hashes, 4)

	held := errors.Is(err04, faultstore.ErrInjected) && errors.Is(err09, faultstore.ErrInjected) &&
		rep.Databases == 12 && rep.RestorePoints == wantPoints && len(rep.Failed) == 2 &&
		ok04 && f04.LastGood == 2 && f04.RestorePoints == 2 &&
		ok09 && f09.LastGood == 0 && f09.RestorePoints == 0 &&
		parseErr == nil && parsedPass.Databases == 12 && parsedPass.RestorePoints == wantPoints && parsedPass.Failed == 2 &&
		passReply.WithinBounds() && closeErr == nil && closeReply.WithinBounds() &&
		strayErr == nil && len(repStray.Failed) == 2 && repStray.RestorePoints == wantPoints
	report("FLEET-02",
		"inject PartialPut on db-0004 slot 3 and db-0009 slot 1 (+ stray -part key under log/) in 12-DB fleet; verify via InspectFleet and checker subprocess",
		"partial puts never counted as restore points (total=42: 10*4 + 2 + 0); db-0004 LastGood=2, db-0009 LastGood=0; checker bounds ok",
		fmt.Sprintf("fired=%d points=%d/%d failed=%d(db04=%d,db09=%d) checker=(pts=%d,failed=%d,bounds=%t) strayErr=%v",
			len(faultyStore.Fired()), rep.RestorePoints, wantPoints, len(rep.Failed), f04.LastGood, f09.LastGood,
			parsedPass.RestorePoints, parsedPass.Failed, passReply.WithinBounds(), strayErr),
		held)
}

// attackFleet03a rebuilds 8 databases onto empty disks while two old writer node
// processes (w1, w2) are STILL RUNNING with all 8 databases open in memory, then
// proves fencing stops both StoreDurable writes and LocalDurable+ship on w1/w2.
func attackFleet03a(ctx context.Context, root string, hashes []recipe.FileHashes) {
	prefix := runPrefix("f03a")
	work := scratchDir(root, "fleet03a")
	store := connectStore(ctx, prefix)
	defer store.WipeInBatches(ctx)
	if _, err := store.WipeInBatches(ctx); err != nil {
		fatal("wipe f03a", err)
	}
	ids := lab.Ids("db", 8)
	cat, err := catalog.Open(ctx, filepath.Join(work, "catalog.db"))
	if err != nil {
		fatal("open cat", err)
	}
	defer cat.Close()
	if err := cat.RegisterAll(ctx, ids, node.PlatformSettings()); err != nil {
		fatal("register", err)
	}

	// Start 2 writer processes, Capacity=4 each, owning 4 DBs each so ALL 8 stay OPEN.
	startWriter := func(name string) *node.Process {
		cfg := node.ProcessConfig{Name: name, Work: work, Bucket: s3Bucket, Prefix: prefix, Capacity: 4}
		p, err := node.StartProcess(ctx, name, cfg.Args()...)
		if err != nil {
			fatal("start writer "+name, err)
		}
		return p
	}
	w1 := startWriter("w1")
	w2 := startWriter("w2")
	defer func() {
		w1.Signal()
		w1.Reap()
		w2.Signal()
		w2.Reap()
	}()
	ownerProc := func(i int) *node.Process {
		if i%2 == 0 {
			return w1
		}
		return w2
	}

	for i, id := range ids {
		p := ownerProc(i)
		if r, err := p.Send("claim " + id); err != nil || !strings.HasPrefix(r.Line, "ok claim ") {
			fatal("claim "+id, fmt.Errorf("%q %v", r.Line, err))
		}
		for range 3 {
			if r, err := p.Send("write " + id + " store-durable"); err != nil || !strings.HasPrefix(r.Line, "ok write ") {
				fatal("write "+id, fmt.Errorf("%q %v", r.Line, err))
			}
		}
	}

	// WITHOUT closing or stopping w1/w2, rebuild all 8 DBs on fresh node with empty disks!
	fresh := newNode(ctx, work, "fresh", cat, store, 4)
	defer fresh.Close()
	for _, id := range ids {
		cr, err := fresh.Claim(ctx, id)
		if err != nil || cr.Epoch != 2 {
			fatal("fresh claim "+id, fmt.Errorf("%+v %v", cr, err))
		}
	}

	// Now attack the still-running old writers w1 and w2:
	// First 4 DBs: StoreDurable write must be refused with "fenced".
	// Last 4 DBs: LocalDurable write + ship -> ship must be refused with "fenced".
	fencedStoreDur, fencedShip, staleStatusOK := 0, 0, 0
	for i, id := range ids {
		p := ownerProc(i)
		if i < 4 {
			r, err := p.Send("write " + id + " store-durable")
			if err == nil && strings.HasPrefix(r.Line, "refused ") && strings.Contains(r.Line, "fenced:") {
				fencedStoreDur++
			}
		} else {
			_, _ = p.Send("write " + id + " local-durable")
			r, err := p.Send("ship " + id)
			if err == nil && strings.HasPrefix(r.Line, "refused ") && strings.Contains(r.Line, "fenced:") {
				fencedShip++
			}
		}
		st, err := p.Send("status " + id)
		if err == nil && st.Line == "ok status "+id+": fenced" {
			staleStatusOK++
		}
	}

	// Fresh node continues writing step 4 StoreDurable on all 8 DBs cleanly.
	freshWrote := 0
	for _, id := range ids {
		if ack, err := fresh.Write(ctx, id, node.StoreDurable, nextStep); err == nil && ack.Epoch == 2 && ack.UserVersion == 4 {
			freshWrote++
		}
	}
	rep, err := lab.InspectFleet(ctx, store, hashes, 4)
	if err != nil {
		fatal("inspect f03a", err)
	}

	held := fencedStoreDur == 4 && fencedShip == 4 && staleStatusOK == 8 && freshWrote == 8 &&
		rep.Databases == 8 && len(rep.Failed) == 0
	report("FLEET-03a",
		"rebuild 8 DBs onto empty disks while old writers w1/w2 keep all 8 open, then send StoreDurable and LocalDurable+ship to w1/w2",
		"all 4 StoreDurable and all 4 ship calls on old writers are fenced, status=fenced on 8/8, fresh writes 8/8 at epoch 2, InspectFleet 0 failures",
		fmt.Sprintf("fencedStoreDur=%d/4 fencedShip=%d/4 statusFenced=%d/8 freshWrote=%d/8 inspectFailed=%d",
			fencedStoreDur, fencedShip, staleStatusOK, freshWrote, len(rep.Failed)),
		held)
}

// attackFleet03b runs a live concurrent race: fresh rebuilds 16 databases onto
// empty disks WHILE old writer processes w1/w2 actively hammer StoreDurable writes
// across those same 16 databases (with LRU evictions, Capacity=4).
func attackFleet03b(ctx context.Context, root string, hashes []recipe.FileHashes) {
	prefix := runPrefix("f03b")
	work := scratchDir(root, "fleet03b")
	store := connectStore(ctx, prefix)
	defer store.WipeInBatches(ctx)
	if _, err := store.WipeInBatches(ctx); err != nil {
		fatal("wipe f03b", err)
	}
	ids := lab.Ids("db", 16)
	cat, err := catalog.Open(ctx, filepath.Join(work, "catalog.db"))
	if err != nil {
		fatal("open cat", err)
	}
	defer cat.Close()
	if err := cat.RegisterAll(ctx, ids, node.PlatformSettings()); err != nil {
		fatal("register", err)
	}

	startWriter := func(name string) *node.Process {
		cfg := node.ProcessConfig{Name: name, Work: work, Bucket: s3Bucket, Prefix: prefix, Capacity: 4}
		p, err := node.StartProcess(ctx, name, cfg.Args()...)
		if err != nil {
			fatal("start writer "+name, err)
		}
		return p
	}
	w1 := startWriter("w1")
	w2 := startWriter("w2")
	defer func() {
		w1.Signal()
		w1.Reap()
		w2.Signal()
		w2.Reap()
	}()
	shares := [][]string{{}, {}}
	for i, id := range ids {
		shares[i%2] = append(shares[i%2], id)
	}
	procs := []*node.Process{w1, w2}

	// Claim all 16 DBs and write step 1 StoreDurable on w1/w2.
	var mu sync.Mutex
	ackedOld := map[string]int{}
	for wi, p := range procs {
		for _, id := range shares[wi] {
			if r, err := p.Send("claim " + id); err != nil || !strings.HasPrefix(r.Line, "ok claim ") {
				fatal("claim "+id, fmt.Errorf("%q %v", r.Line, err))
			}
			if r, err := p.Send("write " + id + " store-durable"); err != nil || !strings.HasPrefix(r.Line, "ok write ") {
				fatal("seed write "+id, fmt.Errorf("%q %v", r.Line, err))
			}
			ackedOld[id] = 1
		}
	}

	// Concurrently: w1 and w2 each try 5 more rounds of StoreDurable writes across
	// their 8 DBs WHILE fresh rebuilds all 16 DBs (4 concurrent rebuild workers).
	// Wait until old writers land at least 2 live writes (or 500ms) before starting
	// the rebuild workers so both sides collide mid-stream.
	var writerWg sync.WaitGroup
	fencedSeen := map[string]bool{}
	oldExtraAcks := 0
	writersActive := make(chan struct{})
	var activeOnce sync.Once
	for wi, p := range procs {
		writerWg.Add(1)
		go func(proc *node.Process, share []string) {
			defer writerWg.Done()
			for range 5 {
				for _, id := range share {
					r, err := proc.Send("write " + id + " store-durable")
					if err != nil {
						return
					}
					mu.Lock()
					if strings.HasPrefix(r.Line, "ok write ") {
						if _, ack, perr := node.ParseAck(r.Line); perr == nil {
							ackedOld[id] = max(ackedOld[id], ack.UserVersion)
							oldExtraAcks++
							if oldExtraAcks >= 2 {
								activeOnce.Do(func() { close(writersActive) })
							}
						}
					} else if strings.Contains(r.Line, "fenced:") {
						fencedSeen[id] = true
					}
					mu.Unlock()
				}
			}
		}(p, shares[wi])
	}

	select {
	case <-writersActive:
	case <-time.After(500 * time.Millisecond):
	}

	// Start 4 concurrent fresh rebuilders claiming all 16 DBs while old writers hammer them.
	rebuilders := make([]*node.Node, 4)
	for i := range rebuilders {
		rebuilders[i] = newNode(ctx, work, fmt.Sprintf("fresh-%d", i+1), cat, store, 2)
		defer rebuilders[i].Close()
	}
	var rebuildWg sync.WaitGroup
	var rebuildErrs []string
	rebuiltPaths := map[string]string{}
	for wi := range 4 {
		rebuildWg.Add(1)
		go func(worker int) {
			defer rebuildWg.Done()
			for i := worker; i < len(ids); i += 4 {
				id := ids[i]
				// Retry Claim if active writer contended past claimAttempts (expected under live hammering).
				var err error
				for range 5 {
					_, err = rebuilders[worker].Claim(ctx, id)
					if err == nil {
						break
					}
				}
				mu.Lock()
				if err != nil {
					rebuildErrs = append(rebuildErrs, fmt.Sprintf("%s: %v", id, err))
				} else {
					rebuiltPaths[id] = filepath.Join(work, fmt.Sprintf("fresh-%d", worker+1), id+".db")
				}
				mu.Unlock()
			}
		}(wi)
	}
	rebuildWg.Wait()
	writerWg.Wait()

	// After both finish, send one final StoreDurable write to w1/w2 for every DB to confirm all 16 are fenced.
	finalFenced := 0
	for wi, p := range procs {
		for _, id := range shares[wi] {
			r, err := p.Send("write " + id + " store-durable")
			if err == nil && strings.HasPrefix(r.Line, "refused ") && strings.Contains(r.Line, "fenced:") {
				finalFenced++
			}
		}
	}

	// Verify every rebuilt DB has user_version >= ackedOld[id] and InspectFleet reports 0 failures.
	lostAcks := 0
	for _, id := range ids {
		p, ok := rebuiltPaths[id]
		if !ok {
			lostAcks++
			continue
		}
		img, err := os.ReadFile(p)
		if err != nil {
			lostAcks++
			continue
		}
		st, err := lab.MatchImage(hashes, img)
		if err != nil || !st.RecipeState || st.UserVersion < ackedOld[id] {
			lostAcks++
		}
	}
	rep, err := lab.InspectFleet(ctx, store, hashes, 4)
	if err != nil {
		fatal("inspect f03b", err)
	}

	held := len(rebuildErrs) == 0 && finalFenced == 16 && lostAcks == 0 && rep.Databases == 16 && len(rep.Failed) == 0
	report("FLEET-03b",
		"live concurrent race: rebuild 16 DBs across 4 workers while w1/w2 actively hammer StoreDurable writes",
		"all 16 old-writer DBs end up fenced, 0 acked StoreDurable writes lost, InspectFleet reports 0 failures across 16 DBs",
		fmt.Sprintf("oldExtraAcks=%d fencedDuringRace=%d finalFenced=%d/16 rebuildErrs=%d lostAcks=%d inspectFailed=%d",
			oldExtraAcks, len(fencedSeen), finalFenced, len(rebuildErrs), lostAcks, len(rep.Failed)),
		held)
}

// attackFleet04 runs Experiment 10.4 at a scaled size (32 databases, 4 writers,
// capacity 8 — scaled from 500 to keep suite runtime well under 10 minutes) and
// verifies its descriptor-limit guard (FATAL descriptor limit).
func attackFleet04(ctx context.Context, root string) {
	binDir := scratchDir(root, "fleet04-bin")
	expWork := scratchDir(root, "fleet04-work")
	binPath := filepath.Join(binDir, "exp10.4")
	prefix := runPrefix("f04")
	store := connectStore(ctx, prefix)
	defer store.WipeInBatches(ctx)

	build := exec.CommandContext(ctx, "go", "build", "-o", binPath, "./ch10-multi-database-platform-capstone/experiments/10.4-rebuild-the-fleet")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		fatal("build 10.4", fmt.Errorf("%v: %s", err, out))
	}

	run := func(args ...string) (int, string) {
		cmd := exec.CommandContext(ctx, binPath, args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "COURSE_WORK_DIR="+expWork)
		var buf bytes.Buffer
		cmd.Stdout = &buf
		cmd.Stderr = &buf
		err := cmd.Run()
		code := 0
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code = ee.ExitCode()
			} else {
				fatal("run 10.4", err)
			}
		}
		return code, buf.String()
	}

	// 1. Scaled 10.4 run (32 DBs, 4 writers, capacity 8, checker pause 200ms).
	code32, out32 := run("-databases", "32", "-writers", "4", "-capacity", "8", "-checker-pause", "200ms", "-prefix", prefix)
	run32OK := code32 == 0 &&
		strings.Contains(out32, "check ok: 32 of 32 rebuilt databases are byte-identical") &&
		strings.Contains(out32, "check ok: every restore point the checker found while the writers ran rebuilt to a recipe state") &&
		strings.Contains(out32, "check ok: the rebuild read each of the") &&
		strings.Contains(out32, "all checks passed")

	// 2. Descriptor-limit guard: -capacity 200000 needs 1,800,032 descriptors > 1,048,576 soft limit.
	codeLim, outLim := run("-capacity", "200000", "-prefix", prefix+"-lim")
	limOK := codeLim != 0 && strings.Contains(outLim, "FATAL descriptor limit:")

	held := run32OK && limOK
	report("FLEET-04",
		"run 10.4 at scaled size (32 DBs, 4 writers, capacity 8) and test descriptor-limit guard (-capacity 200000)",
		"scaled 10.4 exits 0 with 32/32 E1, concurrent checker ok, double-read check ok; -capacity 200000 exits 1 with FATAL descriptor limit",
		fmt.Sprintf("scaled32Exit=%d(ok=%t) limitGuardExit=%d(ok=%t)", code32, run32OK, codeLim, limOK),
		held)
}

func main() {
	flag.Parse()
	ctx := context.Background()
	switch *role {
	case "node":
		if err := node.RunProcess(ctx, procCfg); err != nil {
			fatal("node "+procCfg.Name, err)
		}
		return
	case "checker":
		store, err := lab.Connect(ctx, procCfg)
		if err == nil {
			err = lab.ServeChecker(ctx, store, *workers, os.Stdin, os.Stdout)
		}
		if err != nil {
			fatal("checker", err)
		}
		return
	}
	root, err := sqlitex.RepoRoot()
	if err != nil {
		fatal("repo root", err)
	}
	defer os.RemoveAll(filepath.Join(root, "_work", "adv-ch10b", fmt.Sprintf("fleet-corruption-and-fencing-%d", os.Getpid())))
	hashes, err := recipe.Default().StepHashes(ctx)
	if err != nil {
		fatal("recipe hashes", err)
	}

	attackFleet01(ctx, root, hashes)
	attackFleet02(ctx, root, hashes)
	attackFleet03a(ctx, root, hashes)
	attackFleet03b(ctx, root, hashes)
	attackFleet04(ctx, root)
}
