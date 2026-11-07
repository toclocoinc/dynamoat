package memstore

import (
	"context"
	"sort"
	"time"

	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
)

// histVersion is one recorded version of an item: the item itself, or nil for a
// tombstone (a delete).
type histVersion struct {
	at   time.Time
	item attr.Item
}

// pitrTrail is how far the latest restorable time trails now.
const pitrTrail = 5 * time.Second

// pitrWindow returns the times t can currently be restored to.
func pitrWindow(t *storage.Table, now time.Time) (earliest, latest time.Time) {
	days := t.PITRDays
	if days <= 0 {
		days = 35
	}
	earliest = t.PITRSince
	if start := now.AddDate(0, 0, -days); start.After(earliest) {
		earliest = start
	}
	return earliest, now.Add(-pitrTrail)
}

// appendHistory records one write's new version (or tombstone) for a table with
// PITR enabled, alongside the write itself.
func (s *Store) appendHistory(st *table, key itemKey, new attr.Item) {
	if !st.def.PITREnabled {
		return
	}
	if st.history == nil {
		st.history = make(map[itemKey][]histVersion)
	}
	st.history[key] = append(st.history[key], histVersion{at: time.Now(), item: cloneItem(new)})
}

// pitrEnableLocked copies the table's current items into history as of its
// PITRSince, the initial snapshot taken when recovery is enabled. The caller
// holds s.mu.
func (s *Store) pitrEnableLocked(st *table) {
	if st.history == nil {
		st.history = make(map[itemKey][]histVersion)
	}
	since := st.def.PITRSince
	for key, it := range st.items {
		st.history[key] = append(st.history[key], histVersion{at: since, item: cloneItem(it)})
	}
	st.pitrDone = true
}

// pitrReadyLocked reports nil when the table's recovery window is ready. The
// caller holds s.mu.
func (s *Store) pitrReadyLocked(st *table) error {
	if !st.def.PITREnabled || !st.pitrDone {
		return storage.ErrOutsideRecoveryWindow
	}
	return nil
}

// RecoveryWindow returns the earliest and latest times t can be restored to now.
func (s *Store) RecoveryWindow(ctx context.Context, t *storage.Table) (time.Time, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.tables[t.Name]
	if !ok {
		return time.Time{}, time.Time{}, storage.ErrTableNotFound
	}
	if err := s.pitrReadyLocked(st); err != nil {
		return time.Time{}, time.Time{}, err
	}
	earliest, latest := pitrWindow(&st.def, time.Now())
	return earliest, latest, nil
}

// RestoreToTime creates target and fills it with t's items as they were at time
// at, then marks it ACTIVE. The caller must hold nothing; it takes s.mu itself.
func (s *Store) RestoreToTime(ctx context.Context, t *storage.Table, at time.Time, target *storage.Table) (*storage.Table, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.tables[t.Name]
	if !ok {
		return nil, storage.ErrTableNotFound
	}
	if err := s.pitrReadyLocked(st); err != nil {
		return nil, err
	}
	earliest, latest := pitrWindow(&st.def, time.Now())
	if at.Before(earliest) || at.After(latest) {
		return nil, storage.ErrOutsideRecoveryWindow
	}
	if _, ok := s.tables[target.Name]; ok {
		return nil, storage.ErrTableExists
	}

	c := cloneTable(target)
	c.ID = s.nextID
	s.nextID++
	assignGSIIDs(c)
	c.Status = storage.StatusCreating
	for i := range c.GSIs {
		c.GSIs[i].Status = storage.StatusCreating
	}
	nt := &table{def: *cloneTable(c), items: make(map[itemKey]attr.Item)}
	for key, versions := range st.history {
		if v := newestAtOrBefore(versions, at); v != nil && v.item != nil {
			nt.items[key] = cloneItem(v.item)
		}
	}
	c.Status = storage.StatusActive
	for i := range c.GSIs {
		c.GSIs[i].Status = storage.StatusActive
	}
	nt.def = *cloneTable(c)
	s.tables[target.Name] = nt
	s.names = append(s.names, target.Name)
	sort.Strings(s.names)
	return cloneTable(&nt.def), nil
}

// newestAtOrBefore returns the newest version at or before at, or nil.
func newestAtOrBefore(versions []histVersion, at time.Time) *histVersion {
	var best *histVersion
	for i := range versions {
		if versions[i].at.After(at) {
			continue
		}
		if best == nil || !versions[i].at.Before(best.at) {
			best = &versions[i]
		}
	}
	return best
}

// TrimHistory deletes item versions no table's recovery window needs any more,
// at most limit per call (0 means no limit), and returns how many it deleted.
func (s *Store) TrimHistory(ctx context.Context, now time.Time, limit int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	deleted := 0
	for _, st := range s.tables {
		if limit > 0 && deleted >= limit {
			break
		}
		enabled := st.def.PITREnabled
		start, _ := pitrWindow(&st.def, now)
		for key, versions := range st.history {
			if limit > 0 && deleted >= limit {
				break
			}
			keepIdx := -1
			if enabled {
				for i := range versions {
					if versions[i].at.After(start) {
						continue
					}
					if keepIdx == -1 || !versions[i].at.Before(versions[keepIdx].at) {
						keepIdx = i
					}
				}
			}
			kept := make([]histVersion, 0, len(versions))
			for i := range versions {
				drop := false
				if !enabled {
					drop = true
				} else if versions[i].at.Before(start) && i != keepIdx {
					drop = true
				}
				if drop {
					if limit > 0 && deleted >= limit {
						kept = append(kept, versions[i])
						continue
					}
					deleted++
					continue
				}
				kept = append(kept, versions[i])
			}
			st.history[key] = kept
		}
	}
	return deleted, nil
}
