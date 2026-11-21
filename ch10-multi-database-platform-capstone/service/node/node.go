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

type AckMode string

const (
	LocalDurable  AckMode = "local-durable"
	StoreDurable  AckMode = "store-durable"
	ReasonFence           = "fence"
	claimAttempts         = 3
)

var (
	ErrNotOwner          = errors.New("this node does not own the database")
	ErrNotLocallyDurable = errors.New("local-durable acknowledgement needs synchronous=FULL")
	ErrMissingLocalFile  = errors.New("the local database file is missing; claim the database again to restore it")
	ErrChangedWhileShut  = errors.New("the local database file changed while it was closed")
	idPattern            = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	sha256Pattern        = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type Config struct {
	Name     string
	Dir      string
	Catalog  *catalog.Catalog
	Store    walship.Store
	Capacity int
	File     sqlitex.FileSettings
	Create   func(ctx context.Context, path string) error
}

type Ack struct {
	Mode        AckMode
	Epoch       uint64
	Position    int64
	UserVersion int
}

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

func PlatformSettings() sqlitex.ConnSettings {
	settings := recipe.Default().SQLiteOptions().Conn
	settings.WALAutocheckpoint = 0
	settings.CacheSize = -256
	return settings
}

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
	shipper, err := walship.Open(ctx, walship.Config{Database: path, Store: adapter, Options: options})
	if err != nil {
		return nil, err
	}
	if _, err := shipper.AdoptAtRest(ctx); err != nil {
		return nil, errors.Join(err, shipper.Close())
	}
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
	size := int(binary.BigEndian.Uint16(image[16:18]))
	if size == 1 {
		size = 65536
	}
	if size < 512 || size&(size-1) != 0 || len(image)%size != 0 {
		return 0, fmt.Errorf("node: image of %d bytes has page size %d", len(image), size)
	}
	return size, nil
}

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
	if err := errors.Join(n.removeFile(id, ".owner"), n.removeFile(id, ".atrest")); err != nil {
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

func (n *Node) Write(ctx context.Context, id string, mode AckMode, apply func(context.Context, *sqlitex.DB) error) (Ack, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if mode != LocalDurable && mode != StoreDurable {
		return Ack{}, fmt.Errorf("node: unknown acknowledgement mode %q", mode)
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
	if err == nil && mode == StoreDurable {
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

func (n *Node) Owned(id string) (uint64, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	epoch, ok := n.owned[id]
	return epoch, ok
}

func (n *Node) Stats() connmgr.Stats { return n.manager.Stats() }

func (n *Node) TakeProblems() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	problems := n.problems
	n.problems = nil
	return problems
}

func (n *Node) Close() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	err := n.manager.Close()
	if len(n.problems) > 0 {
		err = errors.Join(err, errors.New(strings.Join(n.problems, "; ")))
	}
	return err
}
