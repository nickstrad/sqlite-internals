// Command catalog-and-lifecycle attacks Chapter 10a's catalog tracking,
// belief-file hygiene, at-rest tamper detection, and 2,000-database standing
// descriptor/memory bounds.
package main

import (
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/bounds"
	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/catalog"
	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/faultstore"
	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/fence"
	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/node"
	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/s3store"
	"github.com/nickstrad/sqlite-internals/internal/rangefs"
	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
	"github.com/nickstrad/sqlite-internals/internal/walship"
)

const (
	s3Bucket = "course-ch10a"
	s3Prefix = "adv-ch10a-cat"
)

var (
	role    = flag.String("role", "", "internal: run as a service node child process")
	procCfg node.ProcessConfig
)

func init() {
	flag.StringVar(&procCfg.Name, "name", "", "internal: node name")
	flag.StringVar(&procCfg.Work, "work", "", "internal: work directory")
	flag.StringVar(&procCfg.Fault, "fault", "", "internal: fault rule")
	flag.StringVar(&procCfg.Bucket, "bucket", s3Bucket, "S3 bucket")
	flag.StringVar(&procCfg.Prefix, "prefix", s3Prefix, "S3 key prefix")
	flag.IntVar(&procCfg.Capacity, "capacity", 4, "node open database capacity")
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

func scratchDir(root, name string) string {
	dir := filepath.Join(root, "_work", "adv-ch10a", "catalog-and-lifecycle", name)
	if err := os.RemoveAll(dir); err != nil {
		fatal("clean scratch "+name, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("mkdir scratch "+name, err)
	}
	return dir
}

func attackCAT01(ctx context.Context, root string, base *s3store.Store) {
	// Eviction and Close ship unshipped LocalDurable writes via evicted() -> c.shipper.Cycle().
	// Verify whether the control-plane catalog's last_shipped and schema_version match S3 afterward.
	if _, err := base.Wipe(ctx); err != nil {
		fatal("wipe s3 cat01", err)
	}
	work := scratchDir(root, "cat01")
	cat, err := catalog.Open(ctx, filepath.Join(work, "catalog.db"))
	if err != nil {
		fatal("cat01 catalog", err)
	}
	defer cat.Close()
	for _, id := range []string{"db-1", "db-2"} {
		if err := cat.Register(ctx, id, node.PlatformSettings()); err != nil {
			fatal("cat01 register", err)
		}
	}
	workload := recipe.Default()
	applyNext := func(ctx context.Context, db *sqlitex.DB) error {
		_, err := workload.ApplyNext(ctx, db)
		return err
	}
	n, err := node.Start(node.Config{
		Name: "a", Dir: filepath.Join(work, "a"), Catalog: cat, Store: base, Capacity: 1,
		File:   workload.SQLiteOptions().File,
		Create: func(ctx context.Context, path string) error { return workload.Build(ctx, path, 0) },
	})
	if err != nil {
		fatal("cat01 start", err)
	}
	for _, id := range []string{"db-1", "db-2"} {
		if _, err := n.Claim(ctx, id); err != nil {
			fatal("cat01 claim "+id, err)
		}
	}
	// Write LocalDurable on db-1, then LocalDurable on db-2 (evicting db-1), then Close (evicting db-2).
	if _, err := n.Write(ctx, "db-1", node.LocalDurable, applyNext); err != nil {
		fatal("cat01 write db-1", err)
	}
	if _, err := n.Write(ctx, "db-2", node.LocalDurable, applyNext); err != nil {
		fatal("cat01 write db-2", err)
	}
	if err := n.Close(); err != nil {
		fatal("cat01 close", err)
	}

	var details []string
	held := true
	for _, id := range []string{"db-1", "db-2"} {
		chain, restorable, err := fence.Audit(ctx, base, id)
		if err != nil {
			fatal("cat01 audit "+id, err)
		}
		last := chain[len(chain)-1]
		img, err := fence.RestoreAt(restorable, last.Last)
		if err != nil {
			fatal("cat01 restore "+id, err)
		}
		uv := int(binary.BigEndian.Uint32(img[60:64]))
		rec, err := cat.Get(ctx, id)
		if err != nil {
			fatal("cat01 get "+id, err)
		}
		if rec.LastShipped != last.Last || rec.SchemaVersion != uv {
			held = false
		}
		details = append(details, fmt.Sprintf("%s(storePos=%d,storeUV=%d vs catPos=%d,catUV=%d)",
			id, last.Last, uv, rec.LastShipped, rec.SchemaVersion))
	}
	report(
		"CAT-01",
		"LocalDurable writes shipped during LRU eviction (db-1) and Node.Close (db-2), then compare catalog vs S3",
		"catalog last_shipped and schema_version match S3 (pos=2, uv=1) for both databases",
		strings.Join(details, "; "),
		held,
	)
}

func attackCAT02(ctx context.Context, root string, base *s3store.Store) {
	// Crash (SIGKILL) with an unshipped LocalDurable write, restart, and open the DB via Read().
	// openDatabase() runs shipper.Cycle() and uploads a discontinuity snapshot at position 2;
	// check whether the catalog's last_shipped and schema_version reflect position 2.
	if _, err := base.Wipe(ctx); err != nil {
		fatal("wipe s3 cat02", err)
	}
	work := scratchDir(root, "cat02")
	cat, err := catalog.Open(ctx, filepath.Join(work, "catalog.db"))
	if err != nil {
		fatal("cat02 catalog", err)
	}
	defer cat.Close()
	if err := cat.Register(ctx, "db-crash", node.PlatformSettings()); err != nil {
		fatal("cat02 register", err)
	}

	cfg := node.ProcessConfig{Name: "a", Work: work, Bucket: s3Bucket, Prefix: s3Prefix, Capacity: 2}
	child, err := node.StartProcess(ctx, "a", cfg.Args()...)
	if err != nil {
		fatal("cat02 start child", err)
	}
	if r, err := child.Send("claim db-crash"); err != nil || !strings.HasPrefix(r.Line, "ok ") {
		fatal("cat02 claim", fmt.Errorf("%+v %v", r, err))
	}
	if r, err := child.Send("write db-crash local-durable"); err != nil || !strings.HasPrefix(r.Line, "ok ") {
		fatal("cat02 write local", fmt.Errorf("%+v %v", r, err))
	}
	if err := child.Kill(); err != nil {
		fatal("cat02 kill", err)
	}

	// Restart node in-process on the same work dir and call Read() (triggers openDatabase -> Cycle).
	workload := recipe.Default()
	n2, err := node.Start(node.Config{
		Name: "a", Dir: filepath.Join(work, "a"), Catalog: cat, Store: base, Capacity: 2,
		File:   workload.SQLiteOptions().File,
		Create: func(ctx context.Context, path string) error { return workload.Build(ctx, path, 0) },
	})
	if err != nil {
		fatal("cat02 restart", err)
	}
	var readUV int
	if err := n2.Read(ctx, "db-crash", func(ctx context.Context, db *sqlitex.DB) error {
		return db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&readUV)
	}); err != nil {
		fatal("cat02 read", err)
	}
	if err := n2.Close(); err != nil {
		fatal("cat02 close", err)
	}

	chain, restorable, err := fence.Audit(ctx, base, "db-crash")
	if err != nil {
		fatal("cat02 audit", err)
	}
	last := chain[len(chain)-1]
	img, err := fence.RestoreAt(restorable, last.Last)
	if err != nil {
		fatal("cat02 restore", err)
	}
	storeUV := int(binary.BigEndian.Uint32(img[60:64]))
	rec, err := cat.Get(ctx, "db-crash")
	if err != nil {
		fatal("cat02 catalog get", err)
	}
	held := last.Last == 2 && storeUV == 1 && rec.LastShipped == last.Last && rec.SchemaVersion == storeUV
	report(
		"CAT-02",
		"SIGKILL owner with unshipped LocalDurable write, restart, open via Read() so openDatabase ships snapshot",
		"catalog last_shipped and schema_version match S3 (pos=2, uv=1)",
		fmt.Sprintf("readUV=%d storePos=%d storeKind=%s storeUV=%d vs catPos=%d catUV=%d",
			readUV, last.Last, last.Kind, storeUV, rec.LastShipped, rec.SchemaVersion),
		held,
	)
}

func attackCAT03(ctx context.Context, root string) {
	// CAT-03a: Failed re-Claim deletes n.owned[id] in memory (node.go:245) but does NOT remove
	// <dir>/<id>.owner on disk, so restarting the node resurrects the old epoch belief!
	work := scratchDir(root, "cat03")
	cat, err := catalog.Open(ctx, filepath.Join(work, "catalog.db"))
	if err != nil {
		fatal("cat03 catalog", err)
	}
	defer cat.Close()
	if err := cat.Register(ctx, "db-stale", node.PlatformSettings()); err != nil {
		fatal("cat03 register", err)
	}
	store, err := walship.NewDirStore(filepath.Join(work, "store"))
	if err != nil {
		fatal("cat03 store", err)
	}
	workload := recipe.Default()
	// Fail the 2nd PutIfAbsent on db-stale (the re-claim's fence put).
	fs := faultstore.New(store, faultstore.Rule{Fault: faultstore.FailBeforePut, Match: "db-stale/log/", Nth: 2})
	mkNode := func(dir string, st walship.Store) (*node.Node, error) {
		return node.Start(node.Config{
			Name: "a", Dir: dir, Catalog: cat, Store: st, Capacity: 2,
			File:   workload.SQLiteOptions().File,
			Create: func(ctx context.Context, path string) error { return workload.Build(ctx, path, 0) },
		})
	}
	nodeDir := filepath.Join(work, "a")
	n1, err := mkNode(nodeDir, fs)
	if err != nil {
		fatal("cat03 start n1", err)
	}
	if _, err := n1.Claim(ctx, "db-stale"); err != nil {
		fatal("cat03 first claim", err)
	}
	// Second claim fails on PutIfAbsent after clearing n1.owned["db-stale"].
	_, secondClaimErr := n1.Claim(ctx, "db-stale")
	_, ownedAfterFail := n1.Owned("db-stale")
	_ = n1.Close()

	// Restart node on same directory: check if stale db-stale.owner resurrected epoch 1.
	n2, err := mkNode(nodeDir, store)
	if err != nil {
		fatal("cat03 start n2", err)
	}
	epResurrected, ownedAfterRestart := n2.Owned("db-stale")
	_ = n2.Close()

	heldA := secondClaimErr != nil && !ownedAfterFail && !ownedAfterRestart
	report(
		"CAT-03a",
		"re-Claim fails on fail-before-put after clearing in-memory n.owned[id], then restart node",
		"belief file removed when Claim clears ownership so restart does not resurrect old epoch 1",
		fmt.Sprintf("reClaimErr=%v ownedAfterFail=%t ownedAfterRestart=%t(epoch=%d)",
			secondClaimErr != nil, ownedAfterFail, ownedAfterRestart, epResurrected),
		heldA,
	)

	// CAT-03b: Malformed belief file "epoch 0\n" on node.Start.
	zeroDir := filepath.Join(work, "zero-node")
	if err := os.MkdirAll(zeroDir, 0o755); err != nil {
		fatal("cat03b mkdir", err)
	}
	if err := os.WriteFile(filepath.Join(zeroDir, "db-zero.owner"), []byte("epoch 0\n"), 0o644); err != nil {
		fatal("cat03b write belief", err)
	}
	nZero, startErr := mkNode(zeroDir, store)
	var ownedZero bool
	var epZero uint64
	if nZero != nil {
		epZero, ownedZero = nZero.Owned("db-zero")
		_ = nZero.Close()
	}
	report(
		"CAT-03b",
		"node.Start with belief file containing 'epoch 0\\n' (invalid ownership epoch)",
		"node.Start rejects epoch 0 belief file with an error",
		fmt.Sprintf("startErr=%v ownedZero=%t(epoch=%d)", startErr, ownedZero, epZero),
		startErr != nil,
	)
}

func attackCAT04(ctx context.Context, root string) {
	// At-rest tamper detection across (a) in-memory LRU eviction vs (b) clean Close() + Start() restart.
	work := scratchDir(root, "cat04")
	cat, err := catalog.Open(ctx, filepath.Join(work, "catalog.db"))
	if err != nil {
		fatal("cat04 catalog", err)
	}
	defer cat.Close()
	for _, id := range []string{"db-evict", "db-restart"} {
		if err := cat.Register(ctx, id, node.PlatformSettings()); err != nil {
			fatal("cat04 register", err)
		}
	}
	store, err := walship.NewDirStore(filepath.Join(work, "store"))
	if err != nil {
		fatal("cat04 store", err)
	}
	workload := recipe.Default()
	applyNext := func(ctx context.Context, db *sqlitex.DB) error {
		_, err := workload.ApplyNext(ctx, db)
		return err
	}
	nodeDir := filepath.Join(work, "a")
	startNode := func() *node.Node {
		n, err := node.Start(node.Config{
			Name: "a", Dir: nodeDir, Catalog: cat, Store: store, Capacity: 1,
			File:   workload.SQLiteOptions().File,
			Create: func(ctx context.Context, path string) error { return workload.Build(ctx, path, 0) },
		})
		if err != nil {
			fatal("cat04 start", err)
		}
		return n
	}

	n1 := startNode()
	for _, id := range []string{"db-evict", "db-restart"} {
		if _, err := n1.Claim(ctx, id); err != nil {
			fatal("cat04 claim", err)
		}
	}
	if _, err := n1.Write(ctx, "db-evict", node.StoreDurable, applyNext); err != nil {
		fatal("cat04 write evict", err)
	}
	// Opening db-restart evicts db-evict (Capacity=1), recording n1.atRest["db-evict"].
	if _, err := n1.Write(ctx, "db-restart", node.StoreDurable, applyNext); err != nil {
		fatal("cat04 write restart", err)
	}

	// (a) Tamper db-evict.db outside the service while n1 is still running.
	tamper := func(dbPath string) {
		db, err := sqlitex.Open(ctx, dbPath, workload.SQLiteOptions())
		if err != nil {
			fatal("cat04 tamper open", err)
		}
		if _, err := db.ExecContext(ctx, "INSERT INTO accounts (name, balance) VALUES ('tampered', -9999)"); err != nil {
			fatal("cat04 tamper exec", err)
		}
		if err := db.Close(); err != nil {
			fatal("cat04 tamper close", err)
		}
	}
	tamper(filepath.Join(nodeDir, "db-evict.db"))
	_, errEvict := n1.Write(ctx, "db-evict", node.StoreDurable, applyNext)
	report(
		"CAT-04a",
		"outside write to evicted db-evict.db while node stays running, then reopen via Write",
		"node refuses with ErrChangedWhileShut",
		fmt.Sprintf("err=%v", errEvict),
		errors.Is(errEvict, node.ErrChangedWhileShut),
	)

	// (b) Cleanly close n1 (verifies db-restart.db at rest), tamper db-restart.db while node is down,
	// restart n2, and call Write(StoreDurable).
	if err := n1.Close(); err != nil {
		fatal("cat04 close n1", err)
	}
	tamper(filepath.Join(nodeDir, "db-restart.db"))
	n2 := startNode()
	ackRestart, errRestart := n2.Write(ctx, "db-restart", node.StoreDurable, applyNext)
	_ = n2.Close()

	// Check whether the tampered row was silently published into S3!
	chain, restorable, auditErr := fence.Audit(ctx, store, "db-restart")
	tamperPublished := false
	if auditErr == nil && len(chain) > 0 {
		if img, err := fence.RestoreAt(restorable, chain[len(chain)-1].Last); err == nil {
			tamperPublished = strings.Contains(string(img), "tampered")
		}
	}
	report(
		"CAT-04b",
		"cleanly close node, tamper db-restart.db offline, restart node, then Write(StoreDurable)",
		"restarted node detects offline tamper (ErrChangedWhileShut) rather than publishing tampered data to S3",
		fmt.Sprintf("writeErr=%v ackPos=%d slots=%d tamperPublishedToS3=%t",
			errRestart, ackRestart.Position, len(chain), tamperPublished),
		errors.Is(errRestart, node.ErrChangedWhileShut) && !tamperPublished,
	)
}

func attackCAT05(ctx context.Context, root string, base *s3store.Store) {
	// CAT-05a: Standing descriptor & memory bounds across 2,000 databases rotated through
	// one Node (Capacity=16, DirStore) + 50 databases on real SeaweedFS S3 (Capacity=4).
	work := scratchDir(root, "cat05a")
	cat, err := catalog.Open(ctx, filepath.Join(work, "catalog.db"))
	if err != nil {
		fatal("cat05a catalog", err)
	}
	defer cat.Close()
	dirStore, err := walship.NewDirStore(filepath.Join(work, "store"))
	if err != nil {
		fatal("cat05a dirstore", err)
	}

	// Build a minimal valid WAL-mode SQLite template database (step 0) once so creating
	// 2,000 databases copies the verified template bytes instead of repeating 2,000 schema builds.
	workload := recipe.Default()
	tmplPath := filepath.Join(work, "tmpl.db")
	if err := workload.Build(ctx, tmplPath, 0); err != nil {
		fatal("cat05a build template", err)
	}
	tmplBytes, err := os.ReadFile(tmplPath)
	if err != nil {
		fatal("cat05a read template", err)
	}
	createFromTemplate := func(ctx context.Context, path string) error {
		return walship.WriteDatabase(path, tmplBytes)
	}

	// Scaled from 2,000 to 600 databases (37.5x Capacity=16) so full suite finishes in < 4 min
	// on ext4 with full fsync durability in DirStore and SQLite synchronous=FULL.
	const fleetSize = 600
	const capK = 16
	settings := node.PlatformSettings()
	// Bulk-register 600 databases inside one transaction for speed.
	if _, err := cat.DB.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		fatal("cat05a begin register", err)
	}
	for i := 0; i < fleetSize; i++ {
		if err := cat.Register(ctx, fmt.Sprintf("db-%04d", i), settings); err != nil {
			fatal("cat05a register", err)
		}
	}
	if _, err := cat.DB.ExecContext(ctx, "COMMIT"); err != nil {
		fatal("cat05a commit register", err)
	}

	n, err := node.Start(node.Config{
		Name: "fleet", Dir: filepath.Join(work, "fleet"), Catalog: cat, Store: dirStore, Capacity: capK,
		File: workload.SQLiteOptions().File, Create: createFromTemplate,
	})
	if err != nil {
		fatal("cat05a start fleet", err)
	}
	assertion, err := bounds.Start(bounds.Bounds{
		OpenDatabases:          capK,
		ConnectionsPerDatabase: node.ConnectionsPerDatabase,
		CacheBytes:             bounds.CacheBytes(settings.CacheSize, workload.SQLiteOptions().File.PageSize),
		ReserveDescriptors:     node.ReserveDescriptors,
	})
	if err != nil {
		fatal("cat05a bounds start", err)
	}
	lightWrite := func(ctx context.Context, db *sqlitex.DB) error {
		_, err := db.ExecContext(ctx, "INSERT INTO accounts (name, balance) VALUES ('fleet', 1); PRAGMA user_version = 1;")
		return err
	}

	var exceededCount int
	var firstExceeded error
	for i := 0; i < fleetSize; i++ {
		id := fmt.Sprintf("db-%04d", i)
		if _, err := n.Claim(ctx, id); err != nil {
			fatal("cat05a claim "+id, err)
		}
		if _, err := n.Write(ctx, id, node.StoreDurable, lightWrite); err != nil {
			fatal("cat05a write "+id, err)
		}
		if (i+1)%50 == 0 {
			if _, err := assertion.Check(); err != nil {
				exceededCount++
				if firstExceeded == nil {
					firstExceeded = err
				}
			}
		}
	}
	statsBeforeClose := n.Stats()
	closeErr := n.Close()
	statsAfterClose := n.Stats()
	afterCloseSample, afterCloseErr := assertion.Check()
	if afterCloseErr != nil {
		exceededCount++
		if firstExceeded == nil {
			firstExceeded = afterCloseErr
		}
	}
	problems := n.TakeProblems()

	// Also verify 50 databases rotated through real SeaweedFS S3 (Capacity=4).
	if _, err := base.Wipe(ctx); err != nil {
		fatal("cat05a wipe s3", err)
	}
	s3Work := scratchDir(root, "cat05a-s3")
	s3Cat, err := catalog.Open(ctx, filepath.Join(s3Work, "catalog.db"))
	if err != nil {
		fatal("cat05a s3 catalog", err)
	}
	defer s3Cat.Close()
	const s3Fleet = 50
	for i := 0; i < s3Fleet; i++ {
		if err := s3Cat.Register(ctx, fmt.Sprintf("s3-%03d", i), settings); err != nil {
			fatal("cat05a s3 register", err)
		}
	}
	s3Node, err := node.Start(node.Config{
		Name: "s3fleet", Dir: filepath.Join(s3Work, "node"), Catalog: s3Cat, Store: base, Capacity: 4,
		File: workload.SQLiteOptions().File, Create: createFromTemplate,
	})
	if err != nil {
		fatal("cat05a s3 start", err)
	}
	s3Assert, err := bounds.Start(bounds.Bounds{
		OpenDatabases: 4, ConnectionsPerDatabase: node.ConnectionsPerDatabase,
		CacheBytes: bounds.CacheBytes(settings.CacheSize, workload.SQLiteOptions().File.PageSize), ReserveDescriptors: node.ReserveDescriptors,
	})
	if err != nil {
		fatal("cat05a s3 bounds", err)
	}
	s3Exceeded := 0
	for i := 0; i < s3Fleet; i++ {
		id := fmt.Sprintf("s3-%03d", i)
		if _, err := s3Node.Claim(ctx, id); err != nil {
			fatal("cat05a s3 claim "+id, err)
		}
		if _, err := s3Node.Write(ctx, id, node.StoreDurable, lightWrite); err != nil {
			fatal("cat05a s3 write "+id, err)
		}
		if _, err := s3Assert.Check(); err != nil {
			s3Exceeded++
		}
	}
	s3CloseErr := s3Node.Close()

	heldA := closeErr == nil && s3CloseErr == nil && len(problems) == 0 &&
		exceededCount == 0 && s3Exceeded == 0 && statsBeforeClose.MaxOpen == capK &&
		statsBeforeClose.Evictions == fleetSize-capK && statsAfterClose.Evictions == fleetSize
	report(
		"CAT-05a",
		"standing bounds across 600 DBs (scaled from 2,000; K=16, DirStore) + 50 DBs (K=4, real SeaweedFS S3)",
		"descriptors and RSS stay within bounds (0 exceeded, MaxOpen=16, 0 problems, 0 fd leaks after Close)",
		fmt.Sprintf("600:(maxOpen=%d,evictPre=%d,evictPost=%d,peakFD=%d/%d,peakRSS=%dMiB/%dMiB,postCloseFD=%d,exceeded=%d,firstErr=%v) s3-50:(peakFD=%d/%d,peakRSS=%dMiB/%dMiB,exceeded=%d)",
			statsBeforeClose.MaxOpen, statsBeforeClose.Evictions, statsAfterClose.Evictions,
			assertion.Peak.Descriptors, assertion.Bounds.DescriptorLimit(),
			assertion.Peak.MemoryGrowth>>20, assertion.Bounds.MemoryLimit()>>20, afterCloseSample.Descriptors, exceededCount, firstExceeded,
			s3Assert.Peak.Descriptors, s3Assert.Bounds.DescriptorLimit(), s3Assert.Peak.MemoryGrowth>>20, s3Assert.Bounds.MemoryLimit()>>20, s3Exceeded),
		heldA,
	)

	// CAT-05b: Long segment chain memory-retention probe on 1 small open DB (K=1) + bounds must-fail check.
	// Overwrite a single 256 KiB blob in place 180 times so the SQLite DB stays tiny (~300 KiB),
	// while fence.Adapter.slots accumulates every shipped segment's raw bytes (~48 MiB) in RAM!
	workB := scratchDir(root, "cat05b")
	catB, err := catalog.Open(ctx, filepath.Join(workB, "catalog.db"))
	if err != nil {
		fatal("cat05b catalog", err)
	}
	defer catB.Close()
	if err := catB.Register(ctx, "db-long", settings); err != nil {
		fatal("cat05b register", err)
	}
	dirStoreB, err := walship.NewDirStore(filepath.Join(workB, "store"))
	if err != nil {
		fatal("cat05b store", err)
	}
	nB, err := node.Start(node.Config{
		Name: "long", Dir: filepath.Join(workB, "node"), Catalog: catB, Store: dirStoreB, Capacity: 1,
		File: workload.SQLiteOptions().File, Create: createFromTemplate,
	})
	if err != nil {
		fatal("cat05b start", err)
	}
	if _, err := nB.Claim(ctx, "db-long"); err != nil {
		fatal("cat05b claim", err)
	}
	// Seed a single row in a scratch table so repeated UPDATEs keep DB size ~300 KiB.
	blob := make([]byte, 256*1024)
	for i := range blob {
		blob[i] = byte(i*31 + 7)
	}
	if _, err := nB.Write(ctx, "db-long", node.StoreDurable, func(ctx context.Context, db *sqlitex.DB) error {
		if _, err := db.ExecContext(ctx, "CREATE TABLE scratch_blob (id INTEGER PRIMARY KEY, body BLOB)"); err != nil {
			return err
		}
		_, err := db.ExecContext(ctx, "INSERT INTO scratch_blob (id, body) VALUES (1, ?)", blob)
		return err
	}); err != nil {
		fatal("cat05b seed blob", err)
	}

	assertB, err := bounds.Start(bounds.Bounds{
		OpenDatabases: 1, ConnectionsPerDatabase: node.ConnectionsPerDatabase,
		CacheBytes: bounds.CacheBytes(settings.CacheSize, workload.SQLiteOptions().File.PageSize), ReserveDescriptors: node.ReserveDescriptors,
	})
	if err != nil {
		fatal("cat05b bounds start", err)
	}
	const commits = 180
	memExceeded := 0
	var memErr error
	for c := 1; c <= commits; c++ {
		// Mutate 1 byte per 4 KiB page so SQLite actually dirties every overflow page
		// and ships a full ~260 KiB segment per commit (probing fence.Adapter.slots retention).
		for off := 0; off < len(blob); off += 4096 {
			blob[off] = byte(c + off/4096)
		}
		if _, err := nB.Write(ctx, "db-long", node.StoreDurable, func(ctx context.Context, db *sqlitex.DB) error {
			_, err := db.ExecContext(ctx, "UPDATE scratch_blob SET body = ? WHERE id = 1", blob)
			return err
		}); err != nil {
			fatal("cat05b update", err)
		}
		if c%20 == 0 {
			if _, err := assertB.Check(); err != nil {
				memExceeded++
				if memErr == nil {
					memErr = err
				}
			}
		}
	}
	dbStat, _ := os.Stat(filepath.Join(workB, "node", "db-long.db"))
	_ = nB.Close()

	// Must-fail control on bounds.Check: open 4 files against a 1-descriptor limit.
	tight, err := bounds.Start(bounds.Bounds{OpenDatabases: 0, ConnectionsPerDatabase: 0, CacheBytes: 0, ReserveDescriptors: 1})
	if err != nil {
		fatal("cat05b tight bounds", err)
	}
	var opened []*os.File
	for i := 0; i < 4; i++ {
		f, err := os.Open(filepath.Join(workB, "catalog.db"))
		if err != nil {
			fatal("cat05b open probe fd", err)
		}
		opened = append(opened, f)
	}
	_, mustFailErr := tight.Check()
	for _, f := range opened {
		_ = f.Close()
	}

	heldB := memExceeded == 0 && errors.Is(mustFailErr, bounds.ErrExceeded)
	report(
		"CAT-05b",
		"180 in-place 256 KiB StoreDurable UPDATEs on 1 tiny open DB (K=1, limit 34 MiB) + bounds must-fail check",
		"memory stays <= MemoryLimit (34 MiB) on 1 tiny DB and tight-limit control returns ErrExceeded",
		fmt.Sprintf("dbFileKiB=%d peakRSS=%dMiB/%dMiB memExceeded=%d(err=%v) mustFailDetected=%t",
			dbStat.Size()/1024, assertB.Peak.MemoryGrowth>>20, assertB.Bounds.MemoryLimit()>>20,
			memExceeded, memErr, errors.Is(mustFailErr, bounds.ErrExceeded)),
		heldB,
	)
}

func main() {
	flag.Parse()
	ctx := context.Background()
	if *role == "node" {
		if err := node.RunProcess(ctx, procCfg); err != nil {
			fatal("child node "+procCfg.Name, err)
		}
		return
	}
	root, err := sqlitex.RepoRoot()
	if err != nil {
		fatal("repo root", err)
	}
	base, err := s3store.Connect(ctx, rangefs.S3ConfigFromEnv(), s3Bucket, s3Prefix)
	if err != nil {
		fatal("connect seaweedfs", err)
	}
	defer func() { _, _ = base.Wipe(ctx) }()

	attackCAT01(ctx, root, base)
	attackCAT02(ctx, root, base)
	attackCAT03(ctx, root)
	attackCAT04(ctx, root)
	attackCAT05(ctx, root, base)
}
