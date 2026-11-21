// Command fencing-and-crashes attacks Chapter 10a's fencing, crash/restart,
// real S3 concurrency, and fault-injection claims against real SeaweedFS S3.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/catalog"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/faultstore"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/fence"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/node"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/s3store"
	"github.com/nickstrad/walfleet/internal/rangefs"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/walship"
)

const (
	s3Bucket = "course-ch10a"
	s3Prefix = "adv-ch10a-fence"
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
	dir := filepath.Join(root, "_work", "adv-ch10a", "fencing-and-crashes", name)
	if err := os.RemoveAll(dir); err != nil {
		fatal("clean scratch "+name, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("mkdir scratch "+name, err)
	}
	return dir
}

func startChild(ctx context.Context, work, name, fault, prefix string) *node.Process {
	cfg := node.ProcessConfig{
		Name:     name,
		Work:     work,
		Fault:    fault,
		Bucket:   s3Bucket,
		Prefix:   prefix,
		Capacity: 4,
	}
	p, err := node.StartProcess(ctx, name, cfg.Args()...)
	if err != nil {
		fatal("start child "+name, err)
	}
	return p
}

func attackFENCE01(ctx context.Context, root string, base *s3store.Store) {
	// FENCE-01a..d: real two-process SIGKILL & restart stale-owner matrix on SeaweedFS S3.
	if _, err := base.Wipe(ctx); err != nil {
		fatal("wipe s3 fence01", err)
	}
	work := scratchDir(root, "fence01")
	cat, err := catalog.Open(ctx, filepath.Join(work, "catalog.db"))
	if err != nil {
		fatal("fence01 catalog", err)
	}
	defer cat.Close()
	for _, id := range []string{"db-a", "db-b", "db-c1", "db-c2", "db-d"} {
		if err := cat.Register(ctx, id, node.PlatformSettings()); err != nil {
			fatal("fence01 register "+id, err)
		}
	}

	// --- FENCE-01a: SIGKILL after shipped write -> takeover -> restart -> refused & belief deleted ---
	a := startChild(ctx, work, "a", "", s3Prefix)
	b := startChild(ctx, work, "b", "", s3Prefix)
	if r, err := a.Send("claim db-a"); err != nil || !strings.HasPrefix(r.Line, "ok ") {
		fatal("01a a claim", fmt.Errorf("%+v %v", r, err))
	}
	if r, err := a.Send("write db-a store-durable"); err != nil || !strings.HasPrefix(r.Line, "ok ") {
		fatal("01a a write", fmt.Errorf("%+v %v", r, err))
	}
	if r, err := b.Send("claim db-a"); err != nil || !strings.HasPrefix(r.Line, "ok ") {
		fatal("01a b claim", fmt.Errorf("%+v %v", r, err))
	}
	// Kill a while it still believes epoch 1 (belief file a/db-a.owner exists).
	if err := a.Kill(); err != nil {
		fatal("01a kill a", err)
	}
	a = startChild(ctx, work, "a", "", s3Prefix)
	rStore, err := a.Send("write db-a store-durable")
	if err != nil {
		fatal("01a a restarted store write", err)
	}
	rLocal, err := a.Send("write db-a local-durable")
	if err != nil {
		fatal("01a a restarted local write", err)
	}
	_, ownerStatErr := os.Stat(filepath.Join(work, "a", "db-a.owner"))
	chainA, _, auditErrA := fence.Audit(ctx, base, "db-a")
	heldA := strings.Contains(rStore.Line, "refused write db-a store-durable: fenced:") &&
		strings.Contains(rLocal.Line, "refused write db-a local-durable: fenced:") &&
		errors.Is(ownerStatErr, os.ErrNotExist) &&
		auditErrA == nil && len(chainA) == 3
	report(
		"FENCE-01a",
		"SIGKILL stale owner after takeover, restart with epoch-1 belief file, attempt store-durable & local-durable writes",
		"both writes refused with fenced:, belief file removed, store unchanged (3 slots)",
		fmt.Sprintf("store=%q local=%q beliefRemoved=%t slots=%d auditErr=%v",
			rStore.Line, rLocal.Line, errors.Is(ownerStatErr, os.ErrNotExist), len(chainA), auditErrA),
		heldA,
	)

	// --- FENCE-01b: SIGKILL with UNSHIPPED local-durable commit -> takeover -> restart ---
	if r, err := a.Send("claim db-b"); err != nil || !strings.HasPrefix(r.Line, "ok ") {
		fatal("01b a claim", fmt.Errorf("%+v %v", r, err))
	}
	if r, err := a.Send("write db-b store-durable"); err != nil || !strings.HasPrefix(r.Line, "ok ") {
		fatal("01b a write store", fmt.Errorf("%+v %v", r, err))
	}
	if r, err := a.Send("write db-b local-durable"); err != nil || !strings.HasPrefix(r.Line, "ok ") {
		fatal("01b a write local", fmt.Errorf("%+v %v", r, err))
	}
	if err := a.Kill(); err != nil {
		fatal("01b kill a", err)
	}
	if r, err := b.Send("claim db-b"); err != nil || !strings.HasPrefix(r.Line, "ok ") {
		fatal("01b b claim", fmt.Errorf("%+v %v", r, err))
	}
	a = startChild(ctx, work, "a", "", s3Prefix)
	rShipB, err := a.Send("ship db-b")
	if err != nil {
		fatal("01b a restarted ship", err)
	}
	chainB, _, auditErrB := fence.Audit(ctx, base, "db-b")
	heldB := strings.Contains(rShipB.Line, "refused ship db-b: fenced:") && auditErrB == nil && len(chainB) == 3
	report(
		"FENCE-01b",
		"SIGKILL owner with unshipped local-durable commit, takeover by B, restart A and ship",
		"A is fenced on open before shipping unshipped local commit; S3 stays at 3 slots",
		fmt.Sprintf("reply=%q slots=%d auditErr=%v", rShipB.Line, len(chainB), auditErrB),
		heldB,
	)

	// --- FENCE-01c: SIGKILL -> takeover -> restart A with stale-list:/log/:1 hiding B's fence ---
	if r, err := a.Send("claim db-c1"); err != nil || !strings.HasPrefix(r.Line, "ok ") {
		fatal("01c a claim c1", fmt.Errorf("%+v %v", r, err))
	}
	if r, err := a.Send("write db-c1 store-durable"); err != nil || !strings.HasPrefix(r.Line, "ok ") {
		fatal("01c a write c1", fmt.Errorf("%+v %v", r, err))
	}
	if r, err := a.Send("claim db-c2"); err != nil || !strings.HasPrefix(r.Line, "ok ") {
		fatal("01c a claim c2", fmt.Errorf("%+v %v", r, err))
	}
	if r, err := a.Send("write db-c2 store-durable"); err != nil || !strings.HasPrefix(r.Line, "ok ") {
		fatal("01c a write c2", fmt.Errorf("%+v %v", r, err))
	}
	if err := a.Kill(); err != nil {
		fatal("01c kill a", err)
	}
	if r, err := b.Send("claim db-c1"); err != nil || !strings.HasPrefix(r.Line, "ok ") {
		fatal("01c b claim c1", fmt.Errorf("%+v %v", r, err))
	}
	if r, err := b.Send("claim db-c2"); err != nil || !strings.HasPrefix(r.Line, "ok ") {
		fatal("01c b claim c2", fmt.Errorf("%+v %v", r, err))
	}
	// Restart a with stale-list hiding B's fence on db-c1's first listing.
	a = startChild(ctx, work, "a", "stale-list:db-c1/log/:1", s3Prefix)
	rC1Store, err := a.Send("write db-c1 store-durable")
	if err != nil {
		fatal("01c a write c1", err)
	}
	// Now kill & restart a with stale-list on db-c2, and try local-durable followed by ship.
	if err := a.Kill(); err != nil {
		fatal("01c kill a2", err)
	}
	a = startChild(ctx, work, "a", "stale-list:db-c2/log/:1", s3Prefix)
	rC2Local, err := a.Send("write db-c2 local-durable")
	if err != nil {
		fatal("01c a write c2 local", err)
	}
	rC2Ship, err := a.Send("ship db-c2")
	if err != nil {
		fatal("01c a ship c2", err)
	}
	chainC1, _, errC1 := fence.Audit(ctx, base, "db-c1")
	chainC2, _, errC2 := fence.Audit(ctx, base, "db-c2")
	heldC := strings.Contains(rC1Store.Line, "refused write db-c1 store-durable: fenced:") &&
		strings.Contains(rC2Ship.Line, "refused ship db-c2: fenced:") &&
		errC1 == nil && len(chainC1) == 3 && errC2 == nil && len(chainC2) == 3
	report(
		"FENCE-01c",
		"restarted stale owner gets stale-list hiding B's fence; tries store-durable (db-c1) and local-durable+ship (db-c2)",
		"store-durable fenced at slot Put; local-durable may ack locally until ship is fenced; 0 stale objects enter S3",
		fmt.Sprintf("c1Store=%q c2Local=%q c2Ship=%q c1Slots=%d c2Slots=%d",
			rC1Store.Line, rC2Local.Line, rC2Ship.Line, len(chainC1), len(chainC2)),
		heldC,
	)

	// --- FENCE-01d: crash-after-put during Claim (dies between fence put and belief file) ---
	if _, err := a.Close(); err != nil {
		fatal("01d close a", err)
	}
	a = startChild(ctx, work, "a", "crash-after-put:db-d/log/:1", s3Prefix)
	_, crashSendErr := a.Send("claim db-d")
	// Child exited with status 3 during PutIfAbsent after slot 1 was stored.
	a = startChild(ctx, work, "a", "", s3Prefix)
	rAWrite, err := a.Send("write db-d store-durable")
	if err != nil {
		fatal("01d a write after crash", err)
	}
	rBClaim, err := b.Send("claim db-d")
	if err != nil {
		fatal("01d b claim", err)
	}
	rBWrite, err := b.Send("write db-d store-durable")
	if err != nil {
		fatal("01d b write", err)
	}
	chainD, _, errD := fence.Audit(ctx, base, "db-d")
	heldD := crashSendErr != nil &&
		strings.Contains(rAWrite.Line, "refused write db-d store-durable: not the owner") &&
		strings.Contains(rBClaim.Line, "ok claim db-d: epoch 2, restored position 1, fence at position 2") &&
		strings.Contains(rBWrite.Line, "ok write db-d store-durable:") &&
		errD == nil && len(chainD) == 3
	report(
		"FENCE-01d",
		"crash-after-put kills node A between fence slot Put and writing .owner belief file",
		"restarted A has no belief; B's Claim restores slot 1, fences at slot 2, and writes slot 3 cleanly",
		fmt.Sprintf("crashDetected=%t aReply=%q bClaim=%q bWrite=%q slots=%d auditErr=%v",
			crashSendErr != nil, rAWrite.Line, rBClaim.Line, rBWrite.Line, len(chainD), errD),
		heldD,
	)

	_, _ = a.Close()
	_, _ = b.Close()
}

func attackFENCE02(ctx context.Context, root string, base *s3store.Store) {
	// Real SeaweedFS S3 PutIfAbsent race (8 goroutines x 20 rounds) +
	// real SQLite catalog.Claim race (8 goroutines x 25 rounds).
	if _, err := base.Wipe(ctx); err != nil {
		fatal("wipe s3 fence02", err)
	}
	const racers = 8
	const s3Rounds = 20
	s3Violations := 0
	for round := 0; round < s3Rounds; round++ {
		key := fmt.Sprintf("race/slot-%04d", round)
		var wg sync.WaitGroup
		var winners atomic.Int32
		var existsCount atomic.Int32
		var otherErr atomic.Int32
		var winPayload atomic.Value
		start := make(chan struct{})
		for g := 0; g < racers; g++ {
			wg.Add(1)
			payload := bytes.Repeat([]byte(fmt.Sprintf("r%02d-g%02d|", round, g)), 128)
			go func(body []byte) {
				defer wg.Done()
				<-start
				err := base.PutIfAbsent(ctx, key, body)
				switch {
				case err == nil:
					winners.Add(1)
					winPayload.Store(body)
				case errors.Is(err, walship.ErrExists):
					existsCount.Add(1)
				default:
					otherErr.Add(1)
				}
			}(payload)
		}
		close(start)
		wg.Wait()
		stored, err := base.Get(ctx, key)
		want, _ := winPayload.Load().([]byte)
		if err != nil || winners.Load() != 1 || existsCount.Load() != racers-1 || otherErr.Load() != 0 || !bytes.Equal(stored, want) {
			s3Violations++
		}
	}

	// Concurrent catalog.Claim race across 8 separate catalog handles on one SQLite file.
	work := scratchDir(root, "fence02")
	catPath := filepath.Join(work, "catalog.db")
	initCat, err := catalog.Open(ctx, catPath)
	if err != nil {
		fatal("fence02 open catalog", err)
	}
	if err := initCat.Register(ctx, "db-race", node.PlatformSettings()); err != nil {
		fatal("fence02 register", err)
	}
	_ = initCat.Close()

	const catRounds = 25
	var mu sync.Mutex
	seenEpochs := map[uint64]int{}
	var catErrs []error
	var wg sync.WaitGroup
	startCat := make(chan struct{})
	for g := 0; g < racers; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			c, err := catalog.Open(ctx, catPath)
			if err != nil {
				mu.Lock()
				catErrs = append(catErrs, err)
				mu.Unlock()
				return
			}
			defer c.Close()
			<-startCat
			for r := 0; r < catRounds; r++ {
				ep, err := c.Claim(ctx, "db-race", fmt.Sprintf("n%d", id))
				mu.Lock()
				if err != nil {
					catErrs = append(catErrs, err)
				} else {
					seenEpochs[ep]++
				}
				mu.Unlock()
			}
		}(g)
	}
	close(startCat)
	wg.Wait()

	wantTotal := racers * catRounds
	uniqueEpochsOK := len(catErrs) == 0 && len(seenEpochs) == wantTotal
	for e := uint64(1); e <= uint64(wantTotal); e++ {
		if seenEpochs[e] != 1 {
			uniqueEpochsOK = false
			break
		}
	}
	held := s3Violations == 0 && uniqueEpochsOK
	report(
		"FENCE-02",
		"concurrent s3store.PutIfAbsent (8x20 rounds on SeaweedFS) and catalog.Claim (8x25 rounds on shared SQLite)",
		"strictly 1 slot winner & untorn bytes per S3 round; 200 distinct epochs 1..200 with 0 catalog errors",
		fmt.Sprintf("s3Violations=%d/%d catUnique=%d/%d catErrs=%d",
			s3Violations, s3Rounds, len(seenEpochs), wantTotal, len(catErrs)),
		held,
	)
}

// hookStore invokes afterPut once right after Base.PutIfAbsent succeeds.
type hookStore struct {
	walship.Store
	afterPut func(key string)
	fired    atomic.Bool
}

func (h *hookStore) PutIfAbsent(ctx context.Context, key string, data []byte) error {
	if err := h.Store.PutIfAbsent(ctx, key, data); err != nil {
		return err
	}
	if h.afterPut != nil && h.fired.CompareAndSwap(false, true) {
		h.afterPut(key)
	}
	return nil
}

func attackFENCE03(ctx context.Context, root string, base *s3store.Store) {
	// FENCE-03a: 16 rounds of real concurrent node.Claim vs node.Claim on SeaweedFS S3.
	if _, err := base.Wipe(ctx); err != nil {
		fatal("wipe s3 fence03", err)
	}
	work := scratchDir(root, "fence03")
	catPath := filepath.Join(work, "catalog.db")
	catA, err := catalog.Open(ctx, catPath)
	if err != nil {
		fatal("fence03 catA", err)
	}
	defer catA.Close()
	catB, err := catalog.Open(ctx, catPath)
	if err != nil {
		fatal("fence03 catB", err)
	}
	defer catB.Close()

	workload := recipe.Default()
	// Pre-build one step-0 template so concurrent claims race tightly on S3 rather than SQLite init.
	tmplPath := filepath.Join(work, "template.db")
	if err := workload.Build(ctx, tmplPath, 0); err != nil {
		fatal("fence03 template", err)
	}
	tmplBytes, err := os.ReadFile(tmplPath)
	if err != nil {
		fatal("fence03 read template", err)
	}
	createFromTemplate := func(ctx context.Context, path string) error {
		return walship.WriteDatabase(path, tmplBytes)
	}

	nodeA, err := node.Start(node.Config{
		Name: "a", Dir: filepath.Join(work, "a"), Catalog: catA, Store: base, Capacity: 4,
		File: workload.SQLiteOptions().File, Create: createFromTemplate,
	})
	if err != nil {
		fatal("fence03 start a", err)
	}
	defer nodeA.Close()
	nodeB, err := node.Start(node.Config{
		Name: "b", Dir: filepath.Join(work, "b"), Catalog: catB, Store: base, Capacity: 4,
		File: workload.SQLiteOptions().File, Create: createFromTemplate,
	})
	if err != nil {
		fatal("fence03 start b", err)
	}
	defer nodeB.Close()

	const rounds = 16
	auditFailures := 0
	staleEpochErrors := 0
	inconsistentOwned := 0
	for r := 0; r < rounds; r++ {
		id := fmt.Sprintf("race-%02d", r)
		if err := catA.Register(ctx, id, node.PlatformSettings()); err != nil {
			fatal("fence03 register", err)
		}
		var wg sync.WaitGroup
		var errA, errB error
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, errA = nodeA.Claim(ctx, id)
		}()
		go func() {
			defer wg.Done()
			<-start
			_, errB = nodeB.Claim(ctx, id)
		}()
		close(start)
		wg.Wait()
		for _, pair := range []struct {
			n   *node.Node
			err error
		}{{nodeA, errA}, {nodeB, errB}} {
			if errors.Is(pair.err, catalog.ErrStaleEpoch) {
				staleEpochErrors++
				if _, owns := pair.n.Owned(id); owns {
					inconsistentOwned++
				}
			}
		}
		if _, _, err := fence.Audit(ctx, base, id); err != nil {
			auditFailures++
		}
	}
	report(
		"FENCE-03a",
		"16 rounds of concurrent a.Claim vs b.Claim on real SeaweedFS S3",
		"all 16 lineages pass fence.Audit and no failed Claim leaves node.Owned(id)==true",
		fmt.Sprintf("auditFailures=%d/%d staleEpochErrs=%d failedClaimStillOwns=%d",
			auditFailures, rounds, staleEpochErrors, inconsistentOwned),
		auditFailures == 0 && inconsistentOwned == 0,
	)

	// FENCE-03b: deterministic reproduction of Claim returning catalog.ErrStaleEpoch
	// AFTER PutIfAbsent succeeded and AFTER n.owned[id] + .owner file were already written!
	idDet := "race-det"
	if err := catA.Register(ctx, idDet, node.PlatformSettings()); err != nil {
		fatal("fence03b register", err)
	}
	hooked := &hookStore{
		Store: base,
		afterPut: func(key string) {
			// Simulate B calling Catalog.Claim right after A's fence Put succeeded and
			// right before A's Catalog.RecordShipped at node.go:292.
			_, _ = catB.Claim(ctx, idDet, "b")
		},
	}
	nodeAHook, err := node.Start(node.Config{
		Name: "ahook", Dir: filepath.Join(work, "ahook"), Catalog: catA, Store: hooked, Capacity: 2,
		File: workload.SQLiteOptions().File, Create: createFromTemplate,
	})
	if err != nil {
		fatal("fence03b start ahook", err)
	}
	defer nodeAHook.Close()

	res, claimErr := nodeAHook.Claim(ctx, idDet)
	epOwned, owns := nodeAHook.Owned(idDet)
	_, beliefErr := os.Stat(filepath.Join(work, "ahook", idDet+".owner"))
	// Consistent behavior: either Claim ignores ErrStaleEpoch (like shipLocked does at node.go:380,
	// because the fence was accepted by the store — build-notes.md line 265), OR if Claim returns an
	// error it must NOT leave n.owned[id] and .owner set.
	consistent := claimErr == nil || (!owns && errors.Is(beliefErr, os.ErrNotExist))
	report(
		"FENCE-03b",
		"deterministic Claim race: B bumps catalog epoch after A's fence Put succeeds, before A's RecordShipped",
		"A's Claim either succeeds (ignoring ErrStaleEpoch like shipLocked) or cleans up owned/.owner on error",
		fmt.Sprintf("claimErr=%v fencePos=%d owned=(%d,%t) beliefExists=%t",
			claimErr, res.Fence, epOwned, owns, beliefErr == nil),
		consistent,
	)
}

// beforeListHook runs a callback on every List call on the target prefix.
type beforeListHook struct {
	walship.Store
	match string
	hook  func(call int)
	calls atomic.Int32
}

func (b *beforeListHook) List(ctx context.Context, prefix string) ([]string, error) {
	keys, err := b.Store.List(ctx, prefix)
	if err == nil && strings.Contains(prefix, b.match) && b.hook != nil {
		n := int(b.calls.Add(1))
		b.hook(n)
	}
	return keys, err
}

func attackFENCE04(ctx context.Context, root string, base *s3store.Store) {
	// Contention test: Node A commits a StoreDurable write right after each of Node B's
	// 3 Claim Refresh listings. Check (1) safety: store lineage stays valid with one writer,
	// and (2) liveness/catalog state: whether claimAttempts=3 exhausts and leaves the catalog
	// naming B (epoch 2) while A keeps writing epoch 1 into S3.
	if _, err := base.Wipe(ctx); err != nil {
		fatal("wipe s3 fence04", err)
	}
	work := scratchDir(root, "fence04")
	catPath := filepath.Join(work, "catalog.db")
	catA, err := catalog.Open(ctx, catPath)
	if err != nil {
		fatal("fence04 catA", err)
	}
	defer catA.Close()
	catB, err := catalog.Open(ctx, catPath)
	if err != nil {
		fatal("fence04 catB", err)
	}
	defer catB.Close()
	if err := catA.Register(ctx, "db-hot", node.PlatformSettings()); err != nil {
		fatal("fence04 register", err)
	}

	workload := recipe.Default()
	applyNext := func(ctx context.Context, db *sqlitex.DB) error {
		_, err := workload.ApplyNext(ctx, db)
		return err
	}
	nodeA, err := node.Start(node.Config{
		Name: "a", Dir: filepath.Join(work, "a"), Catalog: catA, Store: base, Capacity: 2,
		File:   workload.SQLiteOptions().File,
		Create: func(ctx context.Context, path string) error { return workload.Build(ctx, path, 0) },
	})
	if err != nil {
		fatal("fence04 start a", err)
	}
	defer nodeA.Close()
	if _, err := nodeA.Claim(ctx, "db-hot"); err != nil {
		fatal("fence04 a claim", err)
	}

	// Each time B lists db-hot/log/, A races ahead and ships one StoreDurable commit into the
	// next slot before B puts its fence.
	var aRaced int
	bStore := &beforeListHook{
		Store: base,
		match: "db-hot/log/",
		hook: func(call int) {
			if _, err := nodeA.Write(ctx, "db-hot", node.StoreDurable, applyNext); err == nil {
				aRaced++
			}
		},
	}
	nodeB, err := node.Start(node.Config{
		Name: "b", Dir: filepath.Join(work, "b"), Catalog: catB, Store: bStore, Capacity: 2,
		File:   workload.SQLiteOptions().File,
		Create: func(ctx context.Context, path string) error { return workload.Build(ctx, path, 0) },
	})
	if err != nil {
		fatal("fence04 start b", err)
	}
	defer nodeB.Close()

	resB, errB := nodeB.Claim(ctx, "db-hot")
	// Even after B's Claim failed (exhausting 3 attempts), A can still write at epoch 1!
	ackAfter, errAfter := nodeA.Write(ctx, "db-hot", node.StoreDurable, applyNext)
	rec, errRec := catA.Get(ctx, "db-hot")
	chain, _, auditErr := fence.Audit(ctx, base, "db-hot")

	// Safety (one-writer invariant) MUST hold: zero lineage violations, all slots epoch 1.
	allEpoch1 := auditErr == nil && len(chain) == 5
	for _, s := range chain {
		if s.Epoch != 1 {
			allEpoch1 = false
		}
	}
	// Note the contention observation: catalog says owner=b epoch=2 while store has only epoch 1
	// through position 5 (consistent with build-notes correction 3: ownership transfers at fence, not catalog).
	held := allEpoch1 && errB != nil && resB.Attempts == 3 && errAfter == nil && errRec == nil && rec.Owner == "b" && rec.Epoch == 2
	report(
		"FENCE-04",
		"active writer A races ahead of B's 3 Claim attempts on SeaweedFS S3 (exhausting claimAttempts=3)",
		"one-writer safety holds (clean epoch-1 lineage, B fails after 3 attempts, A continues safely since B never fenced)",
		fmt.Sprintf("bClaimErr=%v(attempts=%d) aRaced=%d aPostAckPos=%d cat=(owner=%s,e=%d,shipped=%d) slots=%d auditErr=%v",
			errB, resB.Attempts, aRaced, ackAfter.Position, rec.Owner, rec.Epoch, rec.LastShipped, len(chain), auditErr),
		held,
	)
}

// multiStaleListStore hides the newest hideN keys on the first List matching prefix.
type multiStaleListStore struct {
	walship.Store
	match string
	hideN int
	fired atomic.Bool
}

func (m *multiStaleListStore) List(ctx context.Context, prefix string) ([]string, error) {
	keys, err := m.Store.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	if strings.Contains(prefix, m.match) && m.fired.CompareAndSwap(false, true) && len(keys) > 0 {
		sort.Strings(keys)
		if m.hideN >= len(keys) {
			return nil, nil
		}
		return keys[:len(keys)-m.hideN], nil
	}
	return keys, nil
}

func attackFENCE05(ctx context.Context, root string, base *s3store.Store) {
	// faultstore injection matrix on real SeaweedFS S3:
	// fail-before-put, fail-after-put, partial-put, and multi-key stale-list (hide 2 slots).
	if _, err := base.Wipe(ctx); err != nil {
		fatal("wipe s3 fence05", err)
	}
	work := scratchDir(root, "fence05")
	cat, err := catalog.Open(ctx, filepath.Join(work, "catalog.db"))
	if err != nil {
		fatal("fence05 catalog", err)
	}
	defer cat.Close()
	for _, id := range []string{"db-fbp", "db-fap", "db-pp", "db-msl"} {
		if err := cat.Register(ctx, id, node.PlatformSettings()); err != nil {
			fatal("fence05 register "+id, err)
		}
	}
	workload := recipe.Default()
	applyNext := func(ctx context.Context, db *sqlitex.DB) error {
		_, err := workload.ApplyNext(ctx, db)
		return err
	}
	mkNode := func(name string, st walship.Store) *node.Node {
		n, err := node.Start(node.Config{
			Name: name, Dir: filepath.Join(work, name), Catalog: cat, Store: st, Capacity: 4,
			File:   workload.SQLiteOptions().File,
			Create: func(ctx context.Context, path string) error { return workload.Build(ctx, path, 0) },
		})
		if err != nil {
			fatal("fence05 start "+name, err)
		}
		return n
	}

	// (a) fail-before-put on slot 2 -> refused, next write recovers cleanly.
	fsBefore := faultstore.New(base, faultstore.Rule{Fault: faultstore.FailBeforePut, Match: "db-fbp/log/000000000002", Nth: 1})
	nBefore := mkNode("n-fbp", fsBefore)
	if _, err := nBefore.Claim(ctx, "db-fbp"); err != nil {
		fatal("05a claim", err)
	}
	_, errFailBefore := nBefore.Write(ctx, "db-fbp", node.StoreDurable, applyNext)
	ackRecoverBefore, errRecoverBefore := nBefore.Write(ctx, "db-fbp", node.StoreDurable, applyNext)
	_ = nBefore.Close()
	chainBefore, restBefore, auditBefore := fence.Audit(ctx, base, "db-fbp")
	var imgBeforeOK bool
	if auditBefore == nil && len(chainBefore) > 0 {
		if img, err := fence.RestoreAt(restBefore, chainBefore[len(chainBefore)-1].Last); err == nil && len(img) >= 64 {
			imgBeforeOK = true
		}
	}

	// (b) fail-after-put on slot 2 -> refused (not acked), object IS in S3; next write adopts cleanly.
	fsAfter := faultstore.New(base, faultstore.Rule{Fault: faultstore.FailAfterPut, Match: "db-fap/log/000000000002", Nth: 1})
	nAfter := mkNode("n-fap", fsAfter)
	if _, err := nAfter.Claim(ctx, "db-fap"); err != nil {
		fatal("05b claim", err)
	}
	_, errFailAfter := nAfter.Write(ctx, "db-fap", node.StoreDurable, applyNext)
	ackRecoverAfter, errRecoverAfter := nAfter.Write(ctx, "db-fap", node.StoreDurable, applyNext)
	_ = nAfter.Close()
	chainAfter, restAfter, auditAfter := fence.Audit(ctx, base, "db-fap")
	var imgAfterOK bool
	if auditAfter == nil && len(chainAfter) == 3 {
		if _, err := fence.RestoreAt(restAfter, 3); err == nil {
			imgAfterOK = true
		}
	}

	// (c) partial-put on slot 2 -> truncated bytes in S3; Audit & Claim MUST refuse with ErrDamaged.
	fsPartial := faultstore.New(base, faultstore.Rule{Fault: faultstore.PartialPut, Match: "db-pp/log/000000000002", Nth: 1})
	nPartial := mkNode("n-pp", fsPartial)
	if _, err := nPartial.Claim(ctx, "db-pp"); err != nil {
		fatal("05c claim", err)
	}
	_, errPartialPut := nPartial.Write(ctx, "db-pp", node.StoreDurable, applyNext)
	_ = nPartial.Close()
	_, _, auditPartial := fence.Audit(ctx, base, "db-pp")
	nClaimer := mkNode("n-pp-claimer", base)
	_, claimPartialErr := nClaimer.Claim(ctx, "db-pp")
	_ = nClaimer.Close()

	// (d) multi-slot stale listing (hides 2 newest keys: slot 2 epoch 1 + slot 3 epoch 2).
	nA := mkNode("n-msl-a", base)
	if _, err := nA.Claim(ctx, "db-msl"); err != nil {
		fatal("05d a claim", err)
	}
	if _, err := nA.Write(ctx, "db-msl", node.StoreDurable, applyNext); err != nil {
		fatal("05d a write", err)
	}
	nB := mkNode("n-msl-b", base)
	if _, err := nB.Claim(ctx, "db-msl"); err != nil {
		fatal("05d b claim", err)
	}
	// Node C claims epoch 3 with a stale listing hiding BOTH slot 2 and slot 3 (sees only slot 1).
	mslStore := &multiStaleListStore{Store: base, match: "db-msl/log/", hideN: 2}
	nC := mkNode("n-msl-c", mslStore)
	resC, errC := nC.Claim(ctx, "db-msl")
	_ = nA.Close()
	_ = nB.Close()
	_ = nC.Close()
	chainMSL, _, auditMSL := fence.Audit(ctx, base, "db-msl")

	held := errors.Is(errFailBefore, faultstore.ErrInjected) && errRecoverBefore == nil && imgBeforeOK &&
		errors.Is(errFailAfter, faultstore.ErrInjected) && errRecoverAfter == nil && ackRecoverAfter.Position == 3 && imgAfterOK &&
		errors.Is(errPartialPut, faultstore.ErrInjected) && errors.Is(auditPartial, fence.ErrDamaged) && errors.Is(claimPartialErr, fence.ErrDamaged) &&
		errC == nil && resC.Attempts == 2 && resC.Fence == 4 && auditMSL == nil && len(chainMSL) == 4
	report(
		"FENCE-05",
		"faultstore on SeaweedFS S3: fail-before-put, fail-after-put, partial-put, and 2-slot stale-list",
		"fail-before/after recover cleanly; partial-put rejected as ErrDamaged; 2-slot stale-list retries and fences at slot 4",
		fmt.Sprintf("fbp=(err=%v,recPos=%d,ok=%t) fap=(err=%v,recPos=%d,ok=%t) partial=(audit=%v,claim=%v) msl2=(att=%d,fence=%d,slots=%d,err=%v)",
			errFailBefore != nil, ackRecoverBefore.Position, imgBeforeOK,
			errFailAfter != nil, ackRecoverAfter.Position, imgAfterOK,
			errors.Is(auditPartial, fence.ErrDamaged), errors.Is(claimPartialErr, fence.ErrDamaged),
			resC.Attempts, resC.Fence, len(chainMSL), errC),
		held,
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

	attackFENCE01(ctx, root, base)
	attackFENCE02(ctx, root, base)
	attackFENCE03(ctx, root, base)
	attackFENCE04(ctx, root, base)
	attackFENCE05(ctx, root, base)
}
