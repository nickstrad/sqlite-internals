package fence

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/nickstrad/sqlite-internals/internal/walship"
)

type Inspection struct {
	Database      string
	Slots         int
	BytesRead     int64
	RestorePoints int
	LastGood      int64
	Problem       string
}

func (i Inspection) String() string {
	if i.Problem == "" {
		return fmt.Sprintf("%s: %d slots, %d restore points, all rebuild", i.Database, i.Slots, i.RestorePoints)
	}
	return fmt.Sprintf("%s: unrecoverable past position %d (last good restore point %d): %s", i.Database, i.LastGood, i.LastGood, i.Problem)
}

type prefixStore struct {
	objects map[string][]byte
}

func (p *prefixStore) PutIfAbsent(ctx context.Context, key string, data []byte) error {
	return fmt.Errorf("fence: inspection is read-only")
}

func (p *prefixStore) Get(ctx context.Context, key string) ([]byte, error) {
	data, ok := p.objects[key]
	if !ok {
		return nil, fmt.Errorf("%w: %s", walship.ErrNotFound, key)
	}
	return data, nil
}

func (p *prefixStore) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	for key := range p.objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys, nil
}

func (p *prefixStore) Delete(ctx context.Context, key string) error {
	return fmt.Errorf("fence: inspection is read-only")
}

func Inspect(ctx context.Context, base walship.Store, database string, visit func(position int64, image []byte) error) (Inspection, error) {
	inspection := Inspection{Database: database}
	keys, err := base.List(ctx, SlotPrefix(database))
	if err != nil {
		return inspection, err
	}
	sort.Strings(keys)
	inspection.Slots = len(keys)
	prefix := &prefixStore{objects: map[string][]byte{}}
	var good []Slot
	for _, key := range keys {
		data, err := base.Get(ctx, key)
		if err != nil {
			return inspection, err
		}
		inspection.BytesRead += int64(len(data))
		slot, err := Decode(key, data)
		if err == nil {
			err = CheckLineage(append(good[:len(good):len(good)], slot))
		}
		if err != nil {
			inspection.Problem = err.Error()
			break
		}
		good = append(good, slot)
		walshipKey := walship.SnapshotKey(slot.First)
		if slot.Kind == KindSegment {
			walshipKey = walship.SegmentKey(slot.First)
		}
		prefix.objects[walshipKey] = slot.Object
	}
	if inspection.Problem == "" && len(keys) == 0 {
		inspection.Problem = "the store holds no slot"
	}
	restorable, err := walship.LoadCatalog(ctx, prefix)
	if err != nil {
		return inspection, err
	}
	var last int64
	if len(good) > 0 {
		last = good[len(good)-1].Last
	}
	for position := int64(1); position <= last; position++ {
		image, err := RestoreAt(restorable, position)
		if err == nil && visit != nil {
			err = visit(position, image)
		}
		if err != nil {
			if inspection.Problem == "" {
				inspection.Problem = fmt.Sprintf("position %d does not rebuild: %v", position, err)
			} else {
				inspection.Problem = fmt.Sprintf("position %d does not rebuild: %v; then %s", position, err, inspection.Problem)
			}
			break
		}
		inspection.RestorePoints++
		inspection.LastGood = position
	}
	return inspection, nil
}
