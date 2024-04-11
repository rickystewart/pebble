// Copyright 2018 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package pebble

import (
	"context"
	"fmt"

	"github.com/cockroachdb/pebble/internal/base"
	"github.com/cockroachdb/pebble/internal/keyspan"
	"github.com/cockroachdb/pebble/internal/manifest"
	"github.com/cockroachdb/pebble/sstable"
)

// getIter is an internal iterator used to perform gets. It iterates through
// the values for a particular key, level by level. It is not a general purpose
// internalIterator, but specialized for Get operations so that it loads data
// lazily.
type getIter struct {
	logger       Logger
	comparer     *Comparer
	newIters     tableNewIters
	snapshot     uint64
	key          []byte
	iter         internalIterator
	rangeDelIter keyspan.FragmentIterator
	tombstone    *keyspan.Span
	levelIter    levelIter
	level        int
	batch        *Batch
	mem          flushableList
	l0           []manifest.LevelSlice
	version      *version
	iterKV       *base.InternalKV
	err          error
}

// TODO(sumeer): CockroachDB code doesn't use getIter, but, for completeness,
// make this implement InternalIteratorWithStats.

// getIter implements the base.InternalIterator interface.
var _ base.InternalIterator = (*getIter)(nil)

func (g *getIter) String() string {
	return fmt.Sprintf("len(l0)=%d, len(mem)=%d, level=%d", len(g.l0), len(g.mem), g.level)
}

func (g *getIter) SeekGE(key []byte, flags base.SeekGEFlags) *base.InternalKV {
	panic("pebble: SeekGE unimplemented")
}

func (g *getIter) SeekPrefixGE(prefix, key []byte, flags base.SeekGEFlags) *base.InternalKV {
	return g.SeekPrefixGEStrict(prefix, key, flags)
}

func (g *getIter) SeekPrefixGEStrict(prefix, key []byte, flags base.SeekGEFlags) *base.InternalKV {
	panic("pebble: SeekPrefixGE unimplemented")
}

func (g *getIter) SeekLT(key []byte, flags base.SeekLTFlags) *base.InternalKV {
	panic("pebble: SeekLT unimplemented")
}

func (g *getIter) First() *base.InternalKV {
	return g.Next()
}

func (g *getIter) Last() *base.InternalKV {
	panic("pebble: Last unimplemented")
}

func (g *getIter) Next() *base.InternalKV {
	if g.iter != nil {
		g.iterKV = g.iter.Next()
		if err := g.iter.Error(); err != nil {
			g.err = err
			return nil
		}
	}

	for {
		if g.iter != nil {
			// We have to check rangeDelIter on each iteration because a single
			// user-key can be spread across multiple tables in a level. A range
			// tombstone will appear in the table corresponding to its start
			// key. Every call to levelIter.Next() potentially switches to a new
			// table and thus reinitializes rangeDelIter.
			if g.rangeDelIter != nil {
				g.tombstone, g.err = keyspan.Get(g.comparer.Compare, g.rangeDelIter, g.key)
				g.err = firstError(g.err, g.rangeDelIter.Close())
				if g.err != nil {
					return nil
				}
				g.rangeDelIter = nil
			}

			if g.iterKV != nil {
				if g.tombstone != nil && g.tombstone.CoversAt(g.snapshot, g.iterKV.SeqNum()) {
					// We have a range tombstone covering this key. Rather than return a
					// point or range deletion here, we return false and close our
					// internal iterator which will make Valid() return false,
					// effectively stopping iteration.
					g.err = g.iter.Close()
					g.iter = nil
					return nil
				}
				if g.comparer.Equal(g.key, g.iterKV.K.UserKey) {
					if !g.iterKV.Visible(g.snapshot, base.InternalKeySeqNumMax) {
						g.iterKV = g.iter.Next()
						continue
					}
					return g.iterKV
				}
			}
			// We've advanced the iterator passed the desired key. Move on to the
			// next memtable / level.
			g.err = g.iter.Close()
			g.iter = nil
			if g.err != nil {
				return nil
			}
		}

		// Create an iterator from the batch.
		if g.batch != nil {
			if g.batch.index == nil {
				g.err = ErrNotIndexed
				g.iterKV = nil
				return nil
			}
			g.iter = g.batch.newInternalIter(nil)
			g.rangeDelIter = g.batch.newRangeDelIter(
				nil,
				// Get always reads the entirety of the batch's history, so no
				// batch keys should be filtered.
				base.InternalKeySeqNumMax,
			)
			g.iterKV = g.iter.SeekGE(g.key, base.SeekGEFlagsNone)
			if err := g.iter.Error(); err != nil {
				g.err = err
				return nil
			}
			g.batch = nil
			continue
		}

		// If we have a tombstone from a previous level it is guaranteed to delete
		// keys in lower levels.
		if g.tombstone != nil && g.tombstone.VisibleAt(g.snapshot) {
			return nil
		}

		// Create iterators from memtables from newest to oldest.
		if n := len(g.mem); n > 0 {
			m := g.mem[n-1]
			g.iter = m.newIter(nil)
			g.rangeDelIter = m.newRangeDelIter(nil)
			g.mem = g.mem[:n-1]
			g.iterKV = g.iter.SeekGE(g.key, base.SeekGEFlagsNone)
			if err := g.iter.Error(); err != nil {
				g.err = err
				return nil
			}
			continue
		}

		if g.level == 0 {
			// Create iterators from L0 from newest to oldest.
			if n := len(g.l0); n > 0 {
				files := g.l0[n-1].Iter()
				g.l0 = g.l0[:n-1]
				iterOpts := IterOptions{
					// TODO(sumeer): replace with a parameter provided by the caller.
					CategoryAndQoS: sstable.CategoryAndQoS{
						Category: "pebble-get",
						QoSLevel: sstable.LatencySensitiveQoSLevel,
					},
					logger:                        g.logger,
					snapshotForHideObsoletePoints: g.snapshot}
				g.levelIter.init(context.Background(), iterOpts, g.comparer, g.newIters,
					files, manifest.L0Sublevel(n), internalIterOpts{})
				g.levelIter.initRangeDel(&g.rangeDelIter)
				bc := levelIterBoundaryContext{}
				g.levelIter.initBoundaryContext(&bc)
				g.iter = &g.levelIter

				prefix := g.key[:g.comparer.Split(g.key)]
				g.iterKV = g.iter.SeekPrefixGE(prefix, g.key, base.SeekGEFlagsNone)
				if err := g.iter.Error(); err != nil {
					g.err = err
					return nil
				}

				if bc.isSyntheticIterBoundsKey || bc.isIgnorableBoundaryKey {
					g.iterKV = nil
				}
				continue
			}
			g.level++
		}

		if g.level >= numLevels {
			return nil
		}
		if g.version.Levels[g.level].Empty() {
			g.level++
			continue
		}

		iterOpts := IterOptions{
			// TODO(sumeer): replace with a parameter provided by the caller.
			CategoryAndQoS: sstable.CategoryAndQoS{
				Category: "pebble-get",
				QoSLevel: sstable.LatencySensitiveQoSLevel,
			}, logger: g.logger, snapshotForHideObsoletePoints: g.snapshot}
		g.levelIter.init(context.Background(), iterOpts, g.comparer, g.newIters,
			g.version.Levels[g.level].Iter(), manifest.Level(g.level), internalIterOpts{})
		g.levelIter.initRangeDel(&g.rangeDelIter)
		bc := levelIterBoundaryContext{}
		g.levelIter.initBoundaryContext(&bc)
		g.level++
		g.iter = &g.levelIter

		// Compute the key prefix for bloom filtering if split function is
		// specified, or use the user key as default.
		prefix := g.key[:g.comparer.Split(g.key)]
		g.iterKV = g.iter.SeekPrefixGE(prefix, g.key, base.SeekGEFlagsNone)
		if err := g.iter.Error(); err != nil {
			g.err = err
			return nil
		}
		if bc.isSyntheticIterBoundsKey || bc.isIgnorableBoundaryKey {
			g.iterKV = nil
		}
	}
}

func (g *getIter) Prev() *base.InternalKV {
	panic("pebble: Prev unimplemented")
}

func (g *getIter) NextPrefix([]byte) *base.InternalKV {
	panic("pebble: NextPrefix unimplemented")
}

func (g *getIter) Valid() bool {
	return g.iterKV != nil && g.err == nil
}

func (g *getIter) Error() error {
	return g.err
}

func (g *getIter) Close() error {
	if g.iter != nil {
		if err := g.iter.Close(); err != nil && g.err == nil {
			g.err = err
		}
		g.iter = nil
	}
	return g.err
}

func (g *getIter) SetBounds(lower, upper []byte) {
	panic("pebble: SetBounds unimplemented")
}

func (g *getIter) SetContext(_ context.Context) {}
