package lab

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/bounds"
	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/fence"
	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/node"
	"github.com/nickstrad/sqlite-internals/internal/connmgr"
	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
	"github.com/nickstrad/sqlite-internals/internal/walship"
)

// MatchImage hashes raw SQLite database bytes image against hashes, decodes
// user_version from page-1 header bytes 60..63, and reports RecipeState true
// only when the matched step equals user_version and either the bytes match
// exactly or only the change counter differs with bytes 24..27 == 92..95.
func MatchImage(hashes []recipe.FileHashes, image []byte) (Recovered, error) {
	if len(image) < 100 {
		return Recovered{}, errors.New("lab: an image shorter than the database header")
	}
	match := recipe.MatchHashes(hashes, recipe.HashImage(image))
	// Bytes 60..63 of the SQLite header store PRAGMA user_version in big-endian.
	version := int(image[60])<<24 | int(image[61])<<16 | int(image[62])<<8 | int(image[63])
	ok := match.Found() && match.Step == version && (match.Exact || recipe.ChangeCountersAgree(image))
	return Recovered{UserVersion: version, Match: match, RecipeState: ok}, nil
}

// DatabasesIn lists every key under "dbs/" in store, validates that every key
// has the fenced slot layout "dbs/<id>/log/<pos>", and returns the sorted list
// of distinct database ids.
func DatabasesIn(ctx context.Context, store walship.Store) ([]string, error) {
	keys, err := store.List(ctx, "dbs/")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, key := range keys {
		parts := strings.Split(key, "/")
		if len(parts) != 4 || parts[0] != "dbs" || parts[2] != "log" {
			return nil, fmt.Errorf("lab: unexpected key %q in the store", key)
		}
		seen[parts[1]] = true
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

// FleetReport summarizes one full-store inspection pass across all databases.
type FleetReport struct {
	Databases     int
	RestorePoints int
	BytesRead     int64
	Failed        []fence.Inspection
}

// InspectFleet discovers every database in store and runs fence.Inspect in
// parallel across workers goroutines, rebuilding every published restore point
// in memory and verifying that each rebuilt image is a valid recipe state.
func InspectFleet(ctx context.Context, store walship.Store, hashes []recipe.FileHashes, workers int) (FleetReport, error) {
	ids, err := DatabasesIn(ctx, store)
	if err != nil {
		return FleetReport{}, err
	}
	report := FleetReport{Databases: len(ids)}
	inspections := make([]fence.Inspection, len(ids))
	errs := make([]error, len(ids))
	var next atomic.Int64
	var wg sync.WaitGroup
	for range max(workers, 1) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1) - 1)
				if i >= len(ids) {
					return
				}
				inspections[i], errs[i] = fence.Inspect(ctx, store, ids[i], func(position int64, image []byte) error {
					state, err := MatchImage(hashes, image)
					if err == nil && !state.RecipeState {
						err = fmt.Errorf("the rebuilt state is not a recipe state (user_version %d, %s)", state.UserVersion, state.Match)
					}
					return err
				})
			}
		}()
	}
	wg.Wait()
	for i, inspection := range inspections {
		if errs[i] != nil {
			return report, fmt.Errorf("inspect %s: %w", ids[i], errs[i])
		}
		report.RestorePoints += inspection.RestorePoints
		report.BytesRead += inspection.BytesRead
		if inspection.Problem != "" {
			report.Failed = append(report.Failed, inspection)
		}
	}
	return report, nil
}

// ServeChecker runs Experiment 10.4's standalone checker subprocess over the
// same line protocol as node.Serve ("pass" and "close"), enforcing standing
// bounds with OpenDatabases=0 (since the checker never opens SQLite files).
func ServeChecker(ctx context.Context, store walship.Store, workers int, in io.Reader, out io.Writer) error {
	hashes, err := recipe.Default().StepHashes(ctx)
	if err != nil {
		return err
	}
	assertion, err := bounds.Start(bounds.Bounds{ConnectionsPerDatabase: node.ConnectionsPerDatabase, CacheBytes: bounds.CacheBytes(node.PlatformSettings().CacheSize, recipe.Default().SQLiteOptions().File.PageSize), ReserveDescriptors: node.ReserveDescriptors})
	if err != nil {
		return err
	}
	lines := bufio.NewScanner(in)
	fmt.Fprintln(out, "ready")
	for lines.Scan() {
		command := strings.TrimSpace(lines.Text())
		reply := "refused " + command + ": unknown command"
		switch command {
		case "pass":
			report, err := InspectFleet(ctx, store, hashes, workers)
			if err != nil {
				reply = "refused pass: error: " + err.Error()
				break
			}
			for _, failed := range report.Failed {
				fmt.Fprintln(out, "failed "+failed.String())
			}
			reply = fmt.Sprintf("ok pass: databases %d, restore points %d, failed %d, bytes read %d", report.Databases, report.RestorePoints, len(report.Failed), report.BytesRead)
		case "close":
			reply = "ok close"
		}
		fmt.Fprintln(out, reply)
		sample, err := assertion.Check()
		status := "ok"
		if err != nil {
			status = "EXCEEDED: " + err.Error()
		}
		fmt.Fprintf(out, "bounds descriptors=%d memory=%d %s\n", sample.Descriptors, sample.MemoryGrowth, status)
		if command == "close" {
			fmt.Fprintln(out, assertion.Summary())
			return nil
		}
	}
	return lines.Err()
}

// CheckerPass holds the parsed counts and failure descriptions from one "pass"
// reply emitted by ServeChecker.
type CheckerPass struct {
	Databases     int
	RestorePoints int
	Failed        int
	BytesRead     int64
	FailedLines   []string
}

// ParsePass strictly parses a ServeChecker "pass" reply and verifies that the
// number of preceding "failed …" lines matches the reported failure count.
func ParsePass(reply node.Reply) (CheckerPass, error) {
	var pass CheckerPass
	detail, found := strings.CutPrefix(reply.Line, "ok pass: ")
	if !found {
		return pass, fmt.Errorf("checker reply %q", reply.Line)
	}
	if err := scan(detail, "databases %d, restore points %d, failed %d, bytes read %d", &pass.Databases, &pass.RestorePoints, &pass.Failed, &pass.BytesRead); err != nil {
		return pass, err
	}
	for _, line := range reply.Problems {
		if !strings.HasPrefix(line, "failed ") {
			return pass, fmt.Errorf("checker line %q", line)
		}
		pass.FailedLines = append(pass.FailedLines, strings.TrimPrefix(line, "failed "))
	}
	if len(pass.FailedLines) != pass.Failed {
		return pass, fmt.Errorf("checker reported %d failures and listed %d", pass.Failed, len(pass.FailedLines))
	}
	return pass, nil
}

func scan(detail, format string, values ...any) error {
	if _, err := fmt.Sscanf(detail, format, values...); err != nil {
		return fmt.Errorf("reply %q: %w", detail, err)
	}
	shown := make([]any, len(values))
	for i, v := range values {
		switch p := v.(type) {
		case *int:
			shown[i] = *p
		case *int64:
			shown[i] = *p
		default:
			return fmt.Errorf("scan: unsupported %T", v)
		}
	}
	if fmt.Sprintf(format, shown...) != detail {
		return fmt.Errorf("reply %q does not have the form %q", detail, format)
	}
	return nil
}

// CountingStore wraps a walship.Store and atomically counts Get, bytes read,
// PutIfAbsent, and List calls across concurrent rebuild workers.
type CountingStore struct {
	walship.Store
	gets  atomic.Int64
	bytes atomic.Int64
	puts  atomic.Int64
	lists atomic.Int64
}

// Get forwards to Store.Get and increments the object and byte counters on success.
func (c *CountingStore) Get(ctx context.Context, key string) ([]byte, error) {
	data, err := c.Store.Get(ctx, key)
	if err == nil {
		c.gets.Add(1)
		c.bytes.Add(int64(len(data)))
	}
	return data, err
}

// PutIfAbsent increments the put counter and forwards to Store.PutIfAbsent.
func (c *CountingStore) PutIfAbsent(ctx context.Context, key string, data []byte) error {
	c.puts.Add(1)
	return c.Store.PutIfAbsent(ctx, key, data)
}

// List increments the listing counter and forwards to Store.List.
func (c *CountingStore) List(ctx context.Context, prefix string) ([]string, error) {
	c.lists.Add(1)
	return c.Store.List(ctx, prefix)
}

// Counts returns the total successful gets, bytes read, puts, and listings so far.
func (c *CountingStore) Counts() (gets, bytes, puts, lists int64) {
	return c.gets.Load(), c.bytes.Load(), c.puts.Load(), c.lists.Load()
}

// Quiesced holds the at-rest SHA256 and PRAGMA wal_checkpoint(TRUNCATE) result
// recorded by TruncateAndHash.
type Quiesced struct {
	SHA256     string
	Checkpoint sqlitex.CheckpointResult
}

// TruncateAndHash opens path with platform settings, runs
// PRAGMA wal_checkpoint(TRUNCATE), closes the handle, and returns the at-rest
// SHA-256 digest. After a clean writer shutdown sqlite3WalClose
// (src/wal.c:2507-2571) has already checkpointed and removed -wal, so
// Checkpoint.LogFrames is expected to be 0.
func TruncateAndHash(ctx context.Context, path string) (Quiesced, error) {
	options := recipe.Default().SQLiteOptions()
	options.Conn = node.PlatformSettings()
	options.SingleConnection = true
	db, err := sqlitex.Open(ctx, path, options)
	if err != nil {
		return Quiesced{}, err
	}
	result, err := sqlitex.Checkpoint(ctx, db, "", sqlitex.CheckpointTruncate)
	if err = errors.Join(err, db.Close()); err != nil {
		return Quiesced{}, err
	}
	status, err := connmgr.CheckAtRest(path)
	if err != nil {
		return Quiesced{}, err
	}
	if !status.AtRest {
		return Quiesced{}, fmt.Errorf("lab: %s is not at rest after the checkpoint: %s", path, status)
	}
	return Quiesced{SHA256: status.SHA256, Checkpoint: result}, nil
}
