// Package node owns a single service node's runtime: database routing, bounded
// LRU connection management (internal/connmgr), continuous WAL capture per open
// database (internal/walship over fence.Adapter), local ownership and at-rest
// state files, takeover claims, and the two explicit acknowledgement modes
// (LocalDurable and StoreDurable). It also provides the line-protocol server
// (Serve) and child-process driver (Process) used by multi-process experiments.
//
// Ownership state on disk lives in two per-database sidecar files in Config.Dir:
//   - <id>.owner holds "epoch N\n" so a restarted node retains its pre-crash
//     ownership belief until the store proves a newer epoch fenced it.
//   - <id>.atrest holds the 64-hex sha256 recorded after a clean eviction or
//     close so offline tampering between runs is rejected before shipping.
//
// Each open database holds 3 SQLite connections (the writer plus walship's
// control and pin connections) and opens 7 file descriptors (3 .db, 3 -wal,
// and 1 process-shared -shm via unixInodeInfo, src/os_unix.c:1323-1342).
package node

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/catalog"
	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/fence"
	"github.com/nickstrad/sqlite-internals/internal/connmgr"
	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
	"github.com/nickstrad/sqlite-internals/internal/walship"
)

// AckMode selects when Node.Write acknowledges a committed transaction.
type AckMode string

// Acknowledgement modes and claim constants.
const (
	// LocalDurable acknowledges immediately after the local SQLite commit at
	// synchronous=FULL; unshipped commits can be lost if another node takes over.
	LocalDurable AckMode = "local-durable"
	// StoreDurable acknowledges only after the shipper puts the segment holding
	// the commit into the object store.
	StoreDurable AckMode = "store-durable"
	// ReasonFence is the walship snapshot reason recorded on a takeover fence.
	ReasonFence   = "fence"
	claimAttempts = 3
)

// Sentinel errors returned by Node ownership, durability, and reopen checks.
var (
	// ErrNotOwner reports that this node has no ownership belief for the database.
	ErrNotOwner = errors.New("this node does not own the database")
	// ErrNotLocallyDurable reports a LocalDurable write against a database whose
	// catalog settings do not specify synchronous=FULL.
	ErrNotLocallyDurable = errors.New("local-durable acknowledgement needs synchronous=FULL")
	// ErrMissingLocalFile reports that an owned database's local file is absent.
	ErrMissingLocalFile = errors.New("the local database file is missing; claim the database again to restore it")
	// ErrChangedWhileShut reports that a database closed cleanly no longer
	// matches its recorded at-rest sha256 (or still has WAL sidecars).
	ErrChangedWhileShut = errors.New("the local database file changed while it was closed")
	idPattern           = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	sha256Pattern       = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Config configures a Node: its identity, local working directory, shared
// catalog, object store, open-database LRU capacity, persistent SQLite file
// settings, and initializer for a brand-new database (empty store chain).
type Config struct {
	Name         string
	Dir          string
	Catalog      *catalog.Catalog
	Store        walship.Store
	Capacity     int
	File         sqlitex.FileSettings
	Create       func(ctx context.Context, path string) error
	AckBeforePut bool
}

// Ack describes an acknowledged write: the durability mode, the owning epoch,
// the last shipped stream position (0 for LocalDurable), and the database's
// user_version after the write.
type Ack struct {
	Mode        AckMode
	Epoch       uint64
	Position    int64
	UserVersion int
}

// ClaimResult summarizes a completed Node.Claim: the new catalog epoch, the
// stream position restored from the store (0 when newly created), the position
// of the accepted fence snapshot, and how many listing/put attempts were needed.
type ClaimResult struct {
	Epoch        uint64
	RestoredFrom int64
	Fence        int64
	Attempts     int
}

type capture struct {
	shipper *walship.Shipper
	epoch   uint64
	broken  bool
}

// Node serves databases it owns on one host, bounding open handles through
// internal/connmgr and serializing public calls with a single mutex.
type Node struct {
	config   Config
	mu       sync.Mutex
	manager  *connmgr.Manager
	owned    map[string]uint64
	fenced   map[string]error
	open     map[string]*capture
	atRest   map[string]string
	problems []string
}

// PlatformSettings returns the per-connection SQLite settings registered in
// the catalog for platform databases: WAL mode's default recipe settings with
// wal_autocheckpoint=0 (so only walship checkpoints) and a 256 KiB cache.
func PlatformSettings() sqlitex.ConnSettings {
	settings := recipe.Default().SQLiteOptions().Conn
	settings.WALAutocheckpoint = 0
	settings.CacheSize = -256
	return settings
}

// Start initializes a Node in config.Dir, strictly loading any persisted
// *.owner belief files (rejecting epoch 0) and *.atrest digests from prior runs.
func Start(config Config) (*Node, error) {
	if config.Name == "" || config.Catalog == nil || config.Store == nil || config.Create == nil {
		return nil, errors.New("node: Name, Catalog, Store and Create are required")
	}
	if err := os.MkdirAll(config.Dir, 0o755); err != nil {
		return nil, err
	}
	n := &Node{config: config, owned: map[string]uint64{}, fenced: map[string]error{}, open: map[string]*capture{}, atRest: map[string]string{}}
	beliefs, err := filepath.Glob(filepath.Join(config.Dir, "*.owner"))
	if err != nil {
		return nil, err
	}
	for _, belief := range beliefs {
		var epoch uint64
		data, err := os.ReadFile(belief)
		if err != nil {
			return nil, err
		}
		if _, err := fmt.Sscanf(string(data), "epoch %d\n", &epoch); err != nil || epoch == 0 || fmt.Sprintf("epoch %d\n", epoch) != string(data) {
			return nil, fmt.Errorf("node: belief file %s: %q", belief, data)
		}
		n.owned[strings.TrimSuffix(filepath.Base(belief), ".owner")] = epoch
	}
	records, err := filepath.Glob(filepath.Join(config.Dir, "*.atrest"))
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		data, err := os.ReadFile(record)
		if err != nil {
			return nil, err
		}
		digest := strings.TrimSuffix(string(data), "\n")
		if !sha256Pattern.MatchString(digest) || digest+"\n" != string(data) {
			return nil, fmt.Errorf("node: at-rest record %s: %q", record, data)
		}
		n.atRest[strings.TrimSuffix(filepath.Base(record), ".atrest")] = digest
	}
	n.manager, err = connmgr.New(connmgr.Options{Capacity: config.Capacity, PathFor: n.path, Open: n.openDatabase, OnEvict: n.evicted})
	return n, err
}

func (n *Node) path(id string) (string, error) {
	if !idPattern.MatchString(id) {
		return "", fmt.Errorf("node: invalid database id %q", id)
	}
	return filepath.Join(n.config.Dir, id+".db"), nil
}

func (n *Node) options(settings sqlitex.ConnSettings) sqlitex.Options {
	return sqlitex.Options{File: n.config.File, Conn: settings, SingleConnection: true, ImmediateTx: true}
}

func (n *Node) openDatabase(ctx context.Context, path string) (*sqlitex.DB, error) {
	id := strings.TrimSuffix(filepath.Base(path), ".db")
	epoch, ok := n.owned[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotOwner, id)
	}
	// Refuse a missing local file explicitly: otherwise SQLite would create an
	// empty database and walship would publish it as a discontinuity snapshot.
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMissingLocalFile, err)
	}
	if recorded, ok := n.atRest[id]; ok {
		status, err := connmgr.CheckAtRest(path)
		if err != nil {
			return nil, err
		}
		if !status.AtRest || status.SHA256 != recorded {
			return nil, fmt.Errorf("%w: %s: %s", ErrChangedWhileShut, id, status)
		}
		// Remove the on-disk at-rest digest once the reopen check passes, before
		// opening the shipper: if the node crashes while the database is open,
		// the next restart sees no .atrest file and treats it as post-crash
		// (shipping any unshipped local commits instead of rejecting the file).
		if err := n.removeFile(id, ".atrest"); err != nil {
			return nil, err
		}
	}
	record, err := n.config.Catalog.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	adapter, err := fence.New(n.config.Store, id, epoch)
	if err != nil {
		return nil, err
	}
	options := n.options(record.Settings)
	// Opening walship lists the fenced slot chain through adapter, which fences
	// a restarted stale owner before any local write or upload can happen.
	shipper, err := walship.Open(ctx, walship.Config{Database: path, Store: adapter, Options: options})
	if err != nil {
		return nil, err
	}
	if _, err := shipper.AdoptAtRest(ctx); err != nil {
		return nil, errors.Join(err, shipper.Close())
	}
	// Ship any commits left unshipped on disk (for example, local-durable writes
	// committed before a process crash) before admitting new requests.
	report, err := shipper.Cycle(ctx)
	if err != nil {
		return nil, errors.Join(err, shipper.Close())
	}
	db, err := sqlitex.Open(ctx, path, options)
	if err != nil {
		return nil, errors.Join(err, shipper.Close())
	}
	if shipped(report) {
		var version int
		err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version)
		if err == nil {
			err = n.recordShipped(ctx, id, epoch, shipper.Position(), version)
		}
		if err != nil {
			return nil, errors.Join(err, db.Close(), shipper.Close())
		}
	}
	n.open[id] = &capture{shipper: shipper, epoch: epoch}
	return db, nil
}

func shipped(report walship.CycleReport) bool {
	return report.Segment != nil || report.Snapshot != nil
}

func (n *Node) recordShipped(ctx context.Context, id string, epoch uint64, position int64, userVersion int) error {
	err := n.config.Catalog.RecordShipped(ctx, id, epoch, position, userVersion, n.config.Name+":"+id+".db")
	// Ignore ErrStaleEpoch: the store already accepted this put into the one
	// lineage, and another node bumping the catalog epoch before fencing does
	// not invalidate an already-accepted slot.
	if errors.Is(err, catalog.ErrStaleEpoch) {
		return nil
	}
	return err
}

func (n *Node) removeFile(id, suffix string) error {
	err := os.Remove(filepath.Join(n.config.Dir, id+suffix))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (n *Node) writeFile(id, suffix, content string) error {
	final := filepath.Join(n.config.Dir, id+suffix)
	temp := final + ".tmp"
	if err := os.WriteFile(temp, []byte(content), 0o644); err != nil {
		return err
	}
	return os.Rename(temp, final)
}

func (n *Node) evicted(eviction connmgr.Eviction) {
	c := n.open[eviction.ID]
	delete(n.open, eviction.ID)
	delete(n.atRest, eviction.ID)
	if c == nil {
		return
	}
	_, fenced := n.fenced[eviction.ID]
	var shipErr error
	var report walship.CycleReport
	if !fenced && !c.broken {
		report, shipErr = c.shipper.Cycle(context.Background())
	}
	// Closing the shipper closes the last SQLite connections to the database,
	// so sqlite3WalClose (src/wal.c:2507-2571) checkpoints and deletes -wal/-shm.
	closeErr := c.shipper.Close()
	if errors.Is(shipErr, fence.ErrFenced) {
		n.markFenced(eviction.ID, shipErr)
		fenced = true
	}
	if err := errors.Join(eviction.CloseErr, shipErr, closeErr); err != nil && !fenced {
		n.problems = append(n.problems, fmt.Sprintf("evict %s: %v", eviction.ID, err))
		return
	}
	if fenced || c.broken {
		return
	}
	// Verify at-rest state here rather than relying on connmgr's own check:
	// connmgr ran while the shipper still held -wal/-shm (returning early
	// without opening the file, avoiding src/os_unix.c:1189-1342 lock loss).
	// Now that all connections are closed, prove local bytes == last shipped state.
	status, err := connmgr.CheckAtRest(eviction.Path)
	switch {
	case err != nil:
		n.problems = append(n.problems, fmt.Sprintf("evict %s: %v", eviction.ID, err))
	case !status.AtRest || status.SHA256 != c.shipper.StateSHA256():
		n.problems = append(n.problems, fmt.Sprintf("evict %s: the closed file is not the last shipped state: %s", eviction.ID, status))
	default:
		n.atRest[eviction.ID] = status.SHA256
		if err := n.writeFile(eviction.ID, ".atrest", status.SHA256+"\n"); err != nil {
			n.problems = append(n.problems, fmt.Sprintf("evict %s: %v", eviction.ID, err))
		}
	}
	if shipped(report) {
		version, err := userVersionOf(eviction.Path)
		if err == nil {
			err = n.recordShipped(context.Background(), eviction.ID, c.epoch, c.shipper.Position(), version)
		}
		if err != nil {
			n.problems = append(n.problems, fmt.Sprintf("evict %s: record shipped: %v", eviction.ID, err))
		}
	}
}

func userVersionOf(path string) (int, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	header := make([]byte, 100)
	if _, err := io.ReadFull(file, header); err != nil {
		return 0, err
	}
	// Bytes 60..63 of the SQLite database header store the big-endian user_version.
	return int(binary.BigEndian.Uint32(header[60:64])), nil
}

func (n *Node) markFenced(id string, err error) {
	n.fenced[id] = err
	delete(n.owned, id)
	if removeErr := n.removeFile(id, ".owner"); removeErr != nil {
		n.problems = append(n.problems, fmt.Sprintf("forget ownership of %s: %v", id, removeErr))
	}
}

func (n *Node) evictIfOpen(id string) {
	if _, err := n.manager.Evict(id); err != nil && !errors.Is(err, connmgr.ErrNotOpen) {
		n.problems = append(n.problems, fmt.Sprintf("evict %s: %v", id, err))
	}
}

func (n *Node) writeBelief(id string, epoch uint64) error {
	return n.writeFile(id, ".owner", fmt.Sprintf("epoch %d\n", epoch))
}

func pageSizeOf(image []byte) (int, error) {
	if len(image) < 100 {
		return 0, fmt.Errorf("node: image of %d bytes", len(image))
	}
	// Header bytes 16..17 store page size in big-endian; value 1 encodes 65536.
	size := int(binary.BigEndian.Uint16(image[16:18]))
	if size == 1 {
		size = 65536
	}
	if size < 512 || size&(size-1) != 0 || len(image)%size != 0 {
		return 0, fmt.Errorf("node: image of %d bytes has page size %d", len(image), size)
	}
	return size, nil
}

// Claim takes ownership of id for this node: it clears any local belief and
// at-rest digest, bumps the catalog epoch, restores the store's latest state
// (or creates an empty database when the chain is empty), writes a fence
// snapshot at end+1 with put-if-absent (retrying on ErrExists from a stale
// listing), writes the local database and <id>.owner belief file, and updates
// the catalog.
func (n *Node) Claim(ctx context.Context, id string) (ClaimResult, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	path, err := n.path(id)
	if err != nil {
		return ClaimResult{}, err
	}
	n.evictIfOpen(id)
	delete(n.fenced, id)
	delete(n.owned, id)
	delete(n.atRest, id)
	// Remove both sidecars on disk before claiming so a failed re-claim cannot
	// resurrect the old epoch on restart, and an older at-rest digest cannot
	// reject a reclaimed database that another node advanced.
	if err := errors.Join(n.removeFile(id, ".owner"), n.removeFile(id, ".atrest")); err != nil {
		return ClaimResult{}, err
	}
	if err := errors.Join(n.removeFile(id, ".sealed"), recipe.RemoveDatabase(n.sidecar(id, ".staged"))); err != nil {
		return ClaimResult{}, err
	}
	epoch, err := n.config.Catalog.Claim(ctx, id, n.config.Name)
	if err != nil {
		return ClaimResult{}, err
	}
	adapter, err := fence.New(n.config.Store, id, epoch)
	if err != nil {
		return ClaimResult{}, err
	}
	for attempt := 1; attempt <= claimAttempts; attempt++ {
		end, err := adapter.Refresh(ctx)
		if err != nil {
			return ClaimResult{Epoch: epoch, Attempts: attempt}, err
		}
		// imageAt reads through adapter's already-cached listing so the restored
		// image and the fence position end+1 come from the same listing.
		image, err := n.imageAt(ctx, adapter, path, end)
		if err != nil {
			return ClaimResult{Epoch: epoch, Attempts: attempt}, err
		}
		pageSize, err := pageSizeOf(image)
		if err != nil {
			return ClaimResult{Epoch: epoch, Attempts: attempt}, fmt.Errorf("claim %s: %w", id, err)
		}
		meta := walship.SnapshotMeta{
			Position: end + 1, Reason: ReasonFence, PageSize: pageSize, Pages: uint32(len(image) / pageSize),
			UserVersion: binary.BigEndian.Uint32(image[60:64]), StateSHA256: walship.SHA256Hex(image),
		}
		object, err := walship.EncodeSnapshot(meta, image)
		if err != nil {
			return ClaimResult{Epoch: epoch, Attempts: attempt}, err
		}
		err = adapter.PutIfAbsent(ctx, walship.SnapshotKey(end+1), object)
		if errors.Is(err, walship.ErrExists) {
			// A lower or equal epoch occupies end+1 (stale listing or a last
			// pre-fence ship by the old owner); re-list and try again.
			continue
		}
		if err != nil {
			return ClaimResult{Epoch: epoch, Attempts: attempt}, err
		}
		if err := recipe.RemoveDatabase(path); err != nil {
			return ClaimResult{Epoch: epoch, Attempts: attempt}, err
		}
		if err := walship.WriteDatabase(path, image); err != nil {
			return ClaimResult{Epoch: epoch, Attempts: attempt}, err
		}
		if err := n.writeBelief(id, epoch); err != nil {
			return ClaimResult{Epoch: epoch, Attempts: attempt}, err
		}
		n.owned[id] = epoch
		result := ClaimResult{Epoch: epoch, RestoredFrom: end, Fence: end + 1, Attempts: attempt}
		return result, n.recordShipped(ctx, id, epoch, end+1, int(meta.UserVersion))
	}
	return ClaimResult{Epoch: epoch, Attempts: claimAttempts}, fmt.Errorf("node: claim %s: the next position stayed taken for %d attempts", id, claimAttempts)
}

func (n *Node) imageAt(ctx context.Context, adapter *fence.Adapter, path string, end int64) ([]byte, error) {
	if end > 0 {
		chain, err := walship.LoadCatalog(ctx, adapter)
		if err != nil {
			return nil, err
		}
		plan, err := chain.Plan(end)
		if err != nil {
			return nil, err
		}
		image, _, err := plan.Execute()
		return image, err
	}
	scratch := path + "-create"
	if err := recipe.RemoveDatabase(scratch); err != nil {
		return nil, err
	}
	if err := n.config.Create(ctx, scratch); err != nil {
		return nil, err
	}
	if err := recipe.CheckAtRest(scratch); err != nil {
		return nil, err
	}
	image, err := os.ReadFile(scratch)
	return image, errors.Join(err, recipe.RemoveDatabase(scratch))
}

func (n *Node) acquire(ctx context.Context, id string) (*connmgr.Handle, uint64, error) {
	if err := n.fenced[id]; err != nil {
		return nil, 0, err
	}
	epoch, ok := n.owned[id]
	if !ok {
		return nil, 0, fmt.Errorf("%w: %s", ErrNotOwner, id)
	}
	h, err := n.manager.Acquire(ctx, id)
	if errors.Is(err, fence.ErrFenced) {
		n.markFenced(id, err)
	}
	return h, epoch, err
}

// Write runs apply against database id under mode. LocalDurable requires
// synchronous=FULL in the catalog settings and acknowledges after the local
// commit. StoreDurable acknowledges only after shipping the new WAL frames to
// the store; if shipping fails (including FencedError), the write is committed
// locally on this node's orphaned file but not acknowledged, and the database is
// evicted so the next use reopens from the store.
func (n *Node) Write(ctx context.Context, id string, mode AckMode, apply func(context.Context, *sqlitex.DB) error) (Ack, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if mode != LocalDurable && mode != StoreDurable {
		return Ack{}, fmt.Errorf("node: unknown acknowledgement mode %q", mode)
	}
	if err := n.refuseSealed(id); err != nil {
		return Ack{}, err
	}
	h, epoch, err := n.acquire(ctx, id)
	if err != nil {
		return Ack{}, err
	}
	if mode == LocalDurable && !strings.EqualFold(h.DB.Options.Conn.Synchronous, sqlitex.SyncFull) {
		h.Release()
		return Ack{}, fmt.Errorf("%w: %s runs synchronous=%s", ErrNotLocallyDurable, id, h.DB.Options.Conn.Synchronous)
	}
	ack := Ack{Mode: mode, Epoch: epoch}
	err = apply(ctx, h.DB)
	if err == nil {
		err = h.DB.QueryRowContext(ctx, "PRAGMA user_version").Scan(&ack.UserVersion)
	}
	if err == nil && mode == StoreDurable && !n.config.AckBeforePut {
		ack.Position, err = n.shipLocked(ctx, id, ack.UserVersion)
	}
	h.Release()
	n.afterShip(id, err)
	if err != nil {
		return Ack{}, err
	}
	return ack, nil
}

func (n *Node) shipLocked(ctx context.Context, id string, userVersion int) (int64, error) {
	c := n.open[id]
	if c == nil {
		return 0, fmt.Errorf("node: %s has no open shipper", id)
	}
	if _, err := c.shipper.Cycle(ctx); err != nil {
		c.broken = true
		return 0, err
	}
	position := c.shipper.Position()
	return position, n.recordShipped(ctx, id, c.epoch, position, userVersion)
}

func (n *Node) afterShip(id string, err error) {
	if errors.Is(err, fence.ErrFenced) {
		n.markFenced(id, err)
	}
	if c := n.open[id]; c != nil && c.broken {
		n.evictIfOpen(id)
	}
}

// Ship runs one walship Cycle for database id, updates the catalog, and returns
// the latest shipped stream position.
func (n *Node) Ship(ctx context.Context, id string) (int64, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	h, _, err := n.acquire(ctx, id)
	if err != nil {
		return 0, err
	}
	var version int
	err = h.DB.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version)
	var position int64
	if err == nil {
		position, err = n.shipLocked(ctx, id, version)
	}
	h.Release()
	n.afterShip(id, err)
	return position, err
}

// Read acquires database id (opening and fencing-checking it if not open) and
// runs read against its SQLite connection.
func (n *Node) Read(ctx context.Context, id string, read func(context.Context, *sqlitex.DB) error) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	h, _, err := n.acquire(ctx, id)
	if err != nil {
		return err
	}
	defer h.Release()
	return read(ctx, h.DB)
}

// Owned reports whether this node currently believes it owns id, and if so at
// what epoch.
func (n *Node) Owned(id string) (uint64, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	epoch, ok := n.owned[id]
	return epoch, ok
}

// Stats returns the underlying connmgr statistics (open count, evictions, etc.).
func (n *Node) Stats() connmgr.Stats { return n.manager.Stats() }

// TakeProblems returns and clears any background problems recorded during
// evictions or ownership cleanup (such as a failed final ship or at-rest check).
func (n *Node) TakeProblems() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	problems := n.problems
	n.problems = nil
	return problems
}

// Close closes every open database (shipping and verifying at-rest state via
// evicted) and returns any close errors joined with recorded problems.
func (n *Node) Close() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	err := n.manager.Close()
	if len(n.problems) > 0 {
		err = errors.Join(err, errors.New(strings.Join(n.problems, "; ")))
	}
	return err
}
