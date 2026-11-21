package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/bounds"
	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/catalog"
	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/lab"
	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/node"
	"github.com/nickstrad/sqlite-internals/internal/procstat"
	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
)

const stepsBeforeRollout = recipe.IndexStep - 1

var (
	failures  []string
	databases = flag.Int("databases", 500, "databases in the fleet (the outline asks for 2000; see the notes for that run)")
	writers   = flag.Int("writers", 8, "writer node processes; each owns an equal share of the fleet")
	workers   = flag.Int("checker-workers", 4, "databases the checker inspects at once")
	pause     = flag.Duration("checker-pause", time.Second, "pause between the checker's passes while the writers run")
	role      = flag.String("role", "", "internal: run as a service node or as the checker")
	process   node.ProcessConfig
)

func init() {
	flag.StringVar(&process.Name, "name", "", "internal: node name")
	flag.StringVar(&process.Work, "work", "", "internal: work directory")
	flag.StringVar(&process.Fault, "fault", "", "internal: fault rule fault:match:nth on the node's store")
	flag.StringVar(&process.Bucket, "bucket", "course-ch10b", "S3 bucket")
	flag.StringVar(&process.Prefix, "prefix", "exp-10-4", "key prefix of this experiment in the bucket")
	flag.IntVar(&process.Capacity, "capacity", 32, "databases a node keeps open")
	flag.BoolVar(&process.AckBeforePut, "ack-before-put", false, "internal: unused here")
}

func check(ok bool, claim string) {
	status := "ok"
	if !ok {
		status = "FAILED"
		failures = append(failures, claim)
	}
	fmt.Printf("check %s: %s\n", status, claim)
}

func fatal(what string, err error) {
	fmt.Printf("FATAL %s: %v\n", what, err)
	os.Exit(1)
}

type fleet struct {
	env           *lab.Env
	ids           []string
	shares        [][]string
	writers       []*node.Process
	mu            sync.Mutex
	acks          map[string]int
	rebuildCounts *lab.CountingStore
}

func (f *fleet) send(p *node.Process, command string) node.Reply {
	reply, err := p.Send(command)
	if err != nil {
		fatal("send "+command, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.env.Tracker.Note(p.Name, reply)
	if strings.HasPrefix(reply.Line, "ok write ") {
		id, ack, err := node.ParseAck(reply.Line)
		if err != nil {
			fatal("parse acknowledgement", err)
		}
		f.acks[id] = max(f.acks[id], ack.UserVersion)
	} else if !strings.HasPrefix(reply.Line, "ok ") {
		f.env.Tracker.Problems = append(f.env.Tracker.Problems, p.Name+": "+reply.Line)
	}
	return reply
}

func (f *fleet) eachWriter(work func(w int, p *node.Process, share []string)) {
	var wg sync.WaitGroup
	for w, p := range f.writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			work(w, p, f.shares[w])
		}()
	}
	wg.Wait()
}

func (f *fleet) writeStream() {
	f.eachWriter(func(_ int, p *node.Process, share []string) {
		for start := 0; start < len(share); start += process.Capacity {
			batch := share[start:min(start+process.Capacity, len(share))]
			for _, id := range batch {
				f.send(p, "claim "+id)
			}
			for range stepsBeforeRollout {
				for _, id := range batch {
					f.send(p, fmt.Sprintf("write %s %s", id, node.StoreDurable))
				}
			}
		}
	})
}

func (f *fleet) rollout() int {
	f.eachWriter(func(_ int, p *node.Process, share []string) {
		for _, id := range share[:len(share)/2] {
			f.send(p, fmt.Sprintf("write %s %s", id, node.StoreDurable))
		}
	})
	rolled := 0
	for _, share := range f.shares {
		rolled += len(share) / 2
	}
	return rolled
}

func (f *fleet) runChecker(checker *node.Process, stop <-chan struct{}) (passes, points int, failed []string) {
	for {
		select {
		case <-stop:
			return
		case <-time.After(*pause):
		}
		reply, err := checker.Send("pass")
		if err != nil {
			fatal("checker", err)
		}
		pass, err := lab.ParsePass(reply)
		if err != nil {
			fatal("checker pass", err)
		}
		f.mu.Lock()
		f.env.Tracker.Note("checker", node.Reply{Line: reply.Line, Bounds: reply.Bounds})
		f.mu.Unlock()
		passes++
		points += pass.RestorePoints
		failed = append(failed, pass.FailedLines...)
	}
}

type recorded struct {
	sha256        string
	schemaVersion int
	lastShipped   int64
}

func (f *fleet) quiesce(cat *catalog.Catalog) map[string]recorded {
	for _, p := range f.writers {
		reply, err := f.env.Stop(p)
		if err != nil {
			fatal("close "+p.Name, err)
		}
		fmt.Printf("INFO: writer %s: %s; %s\n", p.Name, reply.Line, reply.Summary)
	}
	records, err := cat.All(f.env.Ctx)
	if err != nil {
		fatal("catalog", err)
	}
	out := map[string]recorded{}
	frames := 0
	for _, record := range records {
		quiesced, err := lab.TruncateAndHash(f.env.Ctx, f.env.DatabasePath(record.Owner, record.ID))
		if err != nil {
			fatal("quiesce "+record.ID, err)
		}
		frames += quiesced.Checkpoint.LogFrames
		out[record.ID] = recorded{sha256: quiesced.SHA256, schemaVersion: record.SchemaVersion, lastShipped: record.LastShipped}
	}
	fmt.Printf("quiesce: every writer closed; %d databases recorded after a TRUNCATE checkpoint (WAL frames found: %d)\n", len(out), frames)
	check(len(out) == len(f.ids) && frames == 0, "every database was at rest when quiesced: the writers' close left no WAL frame for the TRUNCATE checkpoint")
	return out
}

func (f *fleet) rebuild(cat *catalog.Catalog, before map[string]recorded) {
	for w := range f.writers {
		if err := os.RemoveAll(f.env.NodeDir(fmt.Sprintf("w%d", w+1))); err != nil {
			fatal("delete local files", err)
		}
	}
	left, err := filepath.Glob(filepath.Join(f.env.Work, "w*", "*"))
	if err != nil {
		fatal("glob", err)
	}
	fmt.Printf("every local file deleted (%d files left under the writers' directories)\n", len(left))
	check(len(left) == 0, "the new node starts with empty disks")
	counting := &lab.CountingStore{Store: f.env.Store}
	f.rebuildCounts = counting
	workload := recipe.Default()
	assertion, err := bounds.Start(bounds.Bounds{OpenDatabases: *writers, ConnectionsPerDatabase: node.ConnectionsPerDatabase, CacheBytes: bounds.CacheBytes(node.PlatformSettings().CacheSize, workload.SQLiteOptions().File.PageSize), ReserveDescriptors: node.ReserveDescriptors})
	if err != nil {
		fatal("bounds", err)
	}
	rebuilders := make([]*node.Node, *writers)
	for w := range rebuilders {
		rebuilders[w], err = node.Start(node.Config{
			Name: fmt.Sprintf("fresh-%d", w+1), Dir: filepath.Join(f.env.Work, "fresh", fmt.Sprintf("%d", w+1)), Catalog: cat, Store: counting, Capacity: 1,
			File:   workload.SQLiteOptions().File,
			Create: func(ctx context.Context, path string) error { return fmt.Errorf("a rebuild never creates a database") },
		})
		if err != nil {
			fatal("start the new node", err)
		}
	}
	start := time.Now()
	paths := make(map[string]string, len(f.ids))
	var mu sync.Mutex
	var overBounds []string
	var wg sync.WaitGroup
	for w, share := range f.shares {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i, id := range share {
				if _, err := rebuilders[w].Claim(f.env.Ctx, id); err != nil {
					fatal("rebuild "+id, err)
				}
				mu.Lock()
				paths[id] = f.env.DatabasePath(filepath.Join("fresh", fmt.Sprintf("%d", w+1)), id)
				if i%50 == 0 {
					if _, err := assertion.Check(); err != nil {
						overBounds = append(overBounds, err.Error())
					}
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)
	for _, rebuilder := range rebuilders {
		if err := rebuilder.Close(); err != nil {
			fatal("close the new node", err)
		}
	}
	if _, err := assertion.Check(); err != nil {
		overBounds = append(overBounds, err.Error())
	}
	gets, bytes, puts, lists := counting.Counts()
	fmt.Printf("INFO: rebuild of %d databases by %d workers took %s; %s\n", len(f.ids), *writers, elapsed.Round(time.Millisecond), assertion.Summary())
	fmt.Printf("rebuild read %d objects in %d listings and wrote %d fence objects\n", gets, lists, puts)
	fmt.Printf("INFO: rebuild read %d bytes (%d per database)\n", bytes, bytes/int64(len(f.ids)))
	equal, steps, schemaAgrees := 0, map[int]int{}, 0
	var mismatches []string
	for _, id := range f.ids {
		image, err := os.ReadFile(paths[id])
		if err != nil {
			fatal("read the rebuilt "+id, err)
		}
		state, err := f.env.MatchImage(image)
		if err != nil {
			fatal("match", err)
		}
		sha, err := f.env.AtRestSHA256(filepath.Join("fresh", filepath.Base(filepath.Dir(paths[id]))), id)
		if err != nil {
			fatal("at rest", err)
		}
		if sha == before[id].sha256 {
			equal++
		} else if len(mismatches) < 5 {
			mismatches = append(mismatches, id)
		}
		if state.RecipeState && state.UserVersion == before[id].schemaVersion && state.UserVersion == f.acks[id] {
			schemaAgrees++
		}
		steps[state.UserVersion]++
	}
	var histogram []string
	var keys []int
	for step := range steps {
		keys = append(keys, step)
	}
	sort.Ints(keys)
	for _, step := range keys {
		histogram = append(histogram, fmt.Sprintf("%d at recipe step %d", steps[step], step))
	}
	fmt.Printf("rebuilt: %d of %d byte-identical (E1) to the sha256 recorded after the TRUNCATE checkpoint; %s\n", equal, len(f.ids), strings.Join(histogram, ", "))
	if len(mismatches) > 0 {
		fmt.Printf("  first mismatches: %s\n", strings.Join(mismatches, ", "))
	}
	check(equal == len(f.ids), fmt.Sprintf("%d of %d rebuilt databases are byte-identical to the quiesced originals", equal, len(f.ids)))
	check(schemaAgrees == len(f.ids), "every rebuilt database is a recipe state whose schema version (user_version) is the one the catalog recorded and the last one acknowledged")
	check(steps[recipe.IndexStep]+steps[stepsBeforeRollout] == len(f.ids) && steps[recipe.IndexStep] > 0 && steps[stepsBeforeRollout] > 0, fmt.Sprintf("the rollout stopped halfway: databases are on the old schema (step %d) or the new one (step %d with %s), nothing else", stepsBeforeRollout, recipe.IndexStep, strings.TrimPrefix(recipe.IndexSQL, "CREATE ")))
	for _, line := range overBounds {
		fmt.Println("over bounds: " + line)
	}
	check(len(overBounds) == 0, "the rebuilding process stayed within the standing bounds")
}

func main() {
	flag.Parse()
	ctx := context.Background()
	switch *role {
	case "node":
		if err := node.RunProcess(ctx, process); err != nil {
			fatal("node "+process.Name, err)
		}
		return
	case "checker":
		store, err := lab.Connect(ctx, process)
		if err == nil {
			err = lab.ServeChecker(ctx, store, *workers, os.Stdin, os.Stdout)
		}
		if err != nil {
			fatal("checker", err)
		}
		return
	}
	sqlitex.MustPinCheckMemory(ctx)
	if *databases < 2**writers || *writers < 1 || process.Capacity < 1 {
		fatal("flags", fmt.Errorf("need -writers >= 1, -capacity >= 1 and at least two databases per writer"))
	}
	soft, _, err := procstat.FileLimit()
	if err != nil {
		fatal("file limit", err)
	}
	perNode := bounds.DescriptorsPerConnection*node.ConnectionsPerDatabase*process.Capacity + node.ReserveDescriptors
	fmt.Printf("INFO: descriptor soft limit %d; each writer's bound %d\n", soft, perNode)
	if uint64(perNode) > soft {
		fatal("descriptor limit", fmt.Errorf("-capacity %d needs up to %d descriptors per writer, the soft limit is %d", process.Capacity, perNode, soft))
	}
	store, err := lab.Connect(ctx, process)
	if err != nil {
		fmt.Printf("STORE UNREACHABLE: %v\n", err)
		os.Exit(1)
	}
	env, err := lab.New(ctx, "10.4", store, process)
	if err != nil {
		fatal("work directory", err)
	}
	defer env.Close()
	start := time.Now()
	f := &fleet{env: env, ids: lab.Ids("db", *databases), acks: map[string]int{}}
	if err := env.Reset(f.ids); err != nil {
		fatal("reset", err)
	}
	f.shares = make([][]string, *writers)
	for i, id := range f.ids {
		f.shares[i%*writers] = append(f.shares[i%*writers], id)
	}
	fmt.Printf("INFO: store %s, work directory %s\n", store.Endpoint, env.ShownWork())
	fmt.Printf("fleet: %d databases, %d writer processes (each owns %d and keeps at most %d open), one checker process; store: bucket %s, prefix %s\n",
		*databases, *writers, len(f.shares[0]), process.Capacity, store.Bucket, store.Prefix)
	for w := range *writers {
		p, err := env.Start(env.Config(fmt.Sprintf("w%d", w+1), ""))
		if err != nil {
			fatal("start writer", err)
		}
		f.writers = append(f.writers, p)
	}
	checkerArgs := []string{"-role", "checker", "-bucket", process.Bucket, "-prefix", process.Prefix, "-checker-workers", fmt.Sprint(*workers)}
	checker, err := node.StartProcess(ctx, "checker", checkerArgs...)
	if err != nil {
		fatal("start checker", err)
	}
	stop := make(chan struct{})
	type checked struct {
		passes, points int
		failed         []string
	}
	done := make(chan checked)
	go func() {
		passes, points, failed := f.runChecker(checker, stop)
		done <- checked{passes, points, failed}
	}()
	phase := time.Now()
	f.writeStream()
	fmt.Printf("INFO: write stream took %s\n", time.Since(phase).Round(time.Millisecond))
	phase = time.Now()
	fmt.Printf("write stream: every database claimed and given recipe steps 1-%d, each acknowledged store-durable\n", stepsBeforeRollout)
	rolled := f.rollout()
	fmt.Printf("schema rollout: recipe step %d (%s) applied to %d of %d databases, then stopped\n", recipe.IndexStep, recipe.IndexSQL, rolled, len(f.ids))
	fmt.Printf("INFO: rollout took %s\n", time.Since(phase).Round(time.Millisecond))
	close(stop)
	phase = time.Now()
	concurrent := <-done
	fmt.Printf("INFO: waiting for the checker's last concurrent pass took %s\n", time.Since(phase).Round(time.Millisecond))
	fmt.Printf("INFO: the concurrent checker ran %d passes over the store and rebuilt %d restore points while the writers ran\n", concurrent.passes, concurrent.points)
	for i, line := range concurrent.failed {
		if i < 5 {
			fmt.Println("  failed: " + line)
		}
	}
	check(concurrent.passes > 0 && len(concurrent.failed) == 0, "every restore point the checker found while the writers ran rebuilt to a recipe state")
	phase = time.Now()
	before := f.quiesce(env.Catalog)
	fmt.Printf("INFO: quiesce took %s\n", time.Since(phase).Round(time.Millisecond))
	f.rebuild(env.Catalog, before)
	phase = time.Now()
	reply, err := checker.Send("pass")
	if err != nil {
		fatal("checker", err)
	}
	fmt.Printf("INFO: final checker pass took %s\n", time.Since(phase).Round(time.Millisecond))
	env.Tracker.Note("checker", node.Reply{Line: reply.Line, Bounds: reply.Bounds})
	final, err := lab.ParsePass(reply)
	if err != nil {
		fatal("checker pass", err)
	}
	fmt.Printf("final checker pass: %d databases, %d restore points, %d fail to rebuild\n", final.Databases, final.RestorePoints, final.Failed)
	fmt.Printf("INFO: the final pass read %d bytes, the store's whole content\n", final.BytesRead)
	for i, line := range final.FailedLines {
		if i < 5 {
			fmt.Println("  failed: " + line)
		}
	}
	check(final.Databases == len(f.ids) && final.Failed == 0, "after the rebuild every published restore point of every database rebuilds to a recipe state")
	gets, _, _, _ := f.rebuildCounts.Counts()
	check(gets == 2*int64(final.RestorePoints-len(f.ids)), fmt.Sprintf("the rebuild read each of the %d slots it found exactly twice (once to check the lineage, once to restore)", final.RestorePoints-len(f.ids)))
	closing, err := env.Stop(checker)
	if err != nil {
		fatal("close checker", err)
	}
	fmt.Printf("INFO: checker: %s\n", closing.Summary)
	for _, problem := range env.Tracker.Problems {
		fmt.Println("problem: " + problem)
	}
	for _, line := range env.Tracker.OverBounds {
		fmt.Println("over bounds: " + line)
	}
	check(len(env.Tracker.Problems) == 0, "no node reported a problem and every command was acknowledged")
	check(len(env.Tracker.OverBounds) == 0 && env.Tracker.Replies > 0, "after every command of every writer and checker command the descriptors and memory stayed within the standing bounds")
	fmt.Printf("INFO: %d replies carried a bounds check; total runtime %s\n", env.Tracker.Replies, time.Since(start).Round(time.Second))
	if len(failures) > 0 {
		fmt.Printf("\n%d check(s) FAILED\n", len(failures))
		os.Exit(1)
	}
	fmt.Println("\nall checks passed")
}
