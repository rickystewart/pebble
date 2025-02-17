// Copyright 2024 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package colblk

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"fmt"
	"io"
	"slices"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/cockroachdb/crlib/crbytes"
	"github.com/cockroachdb/crlib/crstrings"
	"github.com/cockroachdb/pebble/internal/base"
	"github.com/cockroachdb/pebble/internal/crdbtest"
	"github.com/cockroachdb/pebble/internal/invariants"
	"github.com/cockroachdb/pebble/sstable/block"
	"github.com/stretchr/testify/require"
	"golang.org/x/exp/rand"
)

const (
	cockroachColRoachKey int = iota
	cockroachColMVCCWallTime
	cockroachColMVCCLogical
	cockroachColUntypedVersion
	cockroachColCount
)

var cockroachKeySchema = KeySchema{
	ColumnTypes: []DataType{
		cockroachColRoachKey:       DataTypePrefixBytes,
		cockroachColMVCCWallTime:   DataTypeUint,
		cockroachColMVCCLogical:    DataTypeUint,
		cockroachColUntypedVersion: DataTypeBytes,
	},
	NewKeyWriter: func() KeyWriter {
		kw := &cockroachKeyWriter{}
		kw.roachKeys.Init(16)
		kw.wallTimes.Init()
		kw.logicalTimes.InitWithDefault()
		kw.untypedVersions.Init()
		return kw
	},
	NewKeySeeker: func() KeySeeker {
		return &cockroachKeySeeker{}
	},
}

type cockroachKeyWriter struct {
	roachKeys       PrefixBytesBuilder
	wallTimes       UintBuilder
	logicalTimes    UintBuilder
	untypedVersions RawBytesBuilder
	prevSuffix      []byte
}

func (kw *cockroachKeyWriter) ComparePrev(key []byte) KeyComparison {
	var cmpv KeyComparison
	cmpv.PrefixLen = int32(crdbtest.Split(key)) // TODO(jackson): Inline
	if kw.roachKeys.Rows() == 0 {
		cmpv.UserKeyComparison = 1
		return cmpv
	}
	lp := kw.roachKeys.UnsafeGet(kw.roachKeys.Rows() - 1)
	cmpv.CommonPrefixLen = int32(crbytes.CommonPrefix(lp, key[:cmpv.PrefixLen-1]))
	if cmpv.CommonPrefixLen == cmpv.PrefixLen-1 {
		// Adjust CommonPrefixLen to include the sentinel byte,
		cmpv.CommonPrefixLen = cmpv.PrefixLen
		cmpv.UserKeyComparison = int32(crdbtest.CompareSuffixes(key[cmpv.PrefixLen:], kw.prevSuffix))
		return cmpv
	}
	// The keys have different MVCC prefixes. We haven't determined which is
	// greater, but we know the index at which they diverge. The base.Comparer
	// contract dictates that prefixes must be lexicographically ordered.
	if len(lp) == int(cmpv.CommonPrefixLen) {
		// cmpv.PrefixLen > cmpv.PrefixLenShared; key is greater.
		cmpv.UserKeyComparison = +1
	} else {
		// Both keys have at least 1 additional byte at which they diverge.
		// Compare the diverging byte.
		cmpv.UserKeyComparison = int32(cmp.Compare(key[cmpv.CommonPrefixLen], lp[cmpv.CommonPrefixLen]))
	}
	return cmpv
}

func (kw *cockroachKeyWriter) WriteKey(
	row int, key []byte, keyPrefixLen, keyPrefixLenSharedWithPrev int32,
) {
	// TODO(jackson): Avoid copying the previous suffix.
	// TODO(jackson): Use keyPrefixLen to speed up decoding.
	roachKey, untypedVersion, wallTime, logicalTime := crdbtest.DecodeEngineKey(key)
	kw.prevSuffix = append(kw.prevSuffix[:0], key[keyPrefixLen:]...)
	// When the roach key is the same, keyPrefixLenSharedWithPrev includes the
	// separator byte.
	kw.roachKeys.Put(roachKey, min(int(keyPrefixLenSharedWithPrev), len(roachKey)))
	kw.wallTimes.Set(row, wallTime)
	// The w.logicalTimes builder was initialized with InitWithDefault, so if we
	// don't set a value, the column value is implicitly zero. We only need to
	// Set anything for non-zero values.
	if logicalTime > 0 {
		kw.logicalTimes.Set(row, uint64(logicalTime))
	}
	kw.untypedVersions.Put(untypedVersion)
}

func (kw *cockroachKeyWriter) MaterializeKey(dst []byte, i int) []byte {
	dst = append(dst, kw.roachKeys.UnsafeGet(i)...)
	// Append separator byte.
	dst = append(dst, 0)
	if untypedVersion := kw.untypedVersions.UnsafeGet(i); len(untypedVersion) > 0 {
		dst = append(dst, untypedVersion...)
		dst = append(dst, byte(len(untypedVersion)))
		return dst
	}
	return crdbtest.AppendTimestamp(dst, kw.wallTimes.Get(i), uint32(kw.logicalTimes.Get(i)))
}

func (kw *cockroachKeyWriter) Reset() {
	kw.roachKeys.Reset()
	kw.wallTimes.Reset()
	kw.logicalTimes.Reset()
	kw.untypedVersions.Reset()
}

func (kw *cockroachKeyWriter) WriteDebug(dst io.Writer, rows int) {
	fmt.Fprint(dst, "prefixes: ")
	kw.roachKeys.WriteDebug(dst, rows)
	fmt.Fprintln(dst)
	fmt.Fprint(dst, "wall times: ")
	kw.wallTimes.WriteDebug(dst, rows)
	fmt.Fprintln(dst)
	fmt.Fprint(dst, "logical times: ")
	kw.logicalTimes.WriteDebug(dst, rows)
	fmt.Fprintln(dst)
	fmt.Fprint(dst, "untyped suffixes: ")
	kw.untypedVersions.WriteDebug(dst, rows)
	fmt.Fprintln(dst)
}

func (kw *cockroachKeyWriter) NumColumns() int {
	return cockroachColCount
}

func (kw *cockroachKeyWriter) DataType(col int) DataType {
	return cockroachKeySchema.ColumnTypes[col]
}

func (kw *cockroachKeyWriter) Size(rows int, offset uint32) uint32 {
	offset = kw.roachKeys.Size(rows, offset)
	offset = kw.wallTimes.Size(rows, offset)
	offset = kw.logicalTimes.Size(rows, offset)
	offset = kw.untypedVersions.Size(rows, offset)
	return offset
}

func (kw *cockroachKeyWriter) Finish(
	col int, rows int, offset uint32, buf []byte,
) (endOffset uint32) {
	switch col {
	case cockroachColRoachKey:
		return kw.roachKeys.Finish(0, rows, offset, buf)
	case cockroachColMVCCWallTime:
		return kw.wallTimes.Finish(0, rows, offset, buf)
	case cockroachColMVCCLogical:
		return kw.logicalTimes.Finish(0, rows, offset, buf)
	case cockroachColUntypedVersion:
		return kw.untypedVersions.Finish(0, rows, offset, buf)
	default:
		panic(fmt.Sprintf("unknown default key column: %d", col))
	}
}

var cockroachKeySeekerPool = sync.Pool{
	New: func() interface{} { return &cockroachKeySeeker{} },
}

type cockroachKeySeeker struct {
	reader          *DataBlockReader
	roachKeys       PrefixBytes
	mvccWallTimes   UnsafeUints
	mvccLogical     UnsafeUints
	untypedVersions RawBytes
	sharedPrefix    []byte
}

var _ KeySeeker = (*cockroachKeySeeker)(nil)

// Init is part of the KeySeeker interface.
func (ks *cockroachKeySeeker) Init(r *DataBlockReader) error {
	ks.reader = r
	ks.roachKeys = r.r.PrefixBytes(cockroachColRoachKey)
	ks.mvccWallTimes = r.r.Uints(cockroachColMVCCWallTime)
	ks.mvccLogical = r.r.Uints(cockroachColMVCCLogical)
	ks.untypedVersions = r.r.RawBytes(cockroachColUntypedVersion)
	ks.sharedPrefix = ks.roachKeys.SharedPrefix()
	return nil
}

// CompareFirstUserKey compares the provided key to the first user key
// contained within the data block. It's equivalent to performing
//
//	Compare(firstUserKey, k)
func (ks *cockroachKeySeeker) IsLowerBound(k []byte, syntheticSuffix []byte) bool {
	roachKey, untypedVersion, wallTime, logicalTime := crdbtest.DecodeEngineKey(k)
	if v := crdbtest.Compare(ks.roachKeys.UnsafeFirstSlice(), roachKey); v != 0 {
		return v > 0
	}
	if len(syntheticSuffix) > 0 {
		return crdbtest.Compare(syntheticSuffix, k[len(roachKey)+1:]) >= 0
	}
	if len(untypedVersion) > 0 {
		if invariants.Enabled && ks.mvccWallTimes.At(0) != 0 {
			panic("comparing timestamp with untyped suffix")
		}
		return crdbtest.Compare(ks.untypedVersions.At(0), untypedVersion) >= 0
	}
	if v := cmp.Compare(ks.mvccWallTimes.At(0), wallTime); v != 0 {
		return v > 0
	}
	return cmp.Compare(uint32(ks.mvccLogical.At(0)), logicalTime) >= 0
}

// SeekGE is part of the KeySeeker interface.
func (ks *cockroachKeySeeker) SeekGE(
	key []byte, boundRow int, searchDir int8,
) (row int, equalPrefix bool) {
	// TODO(jackson): Inline crdbtest.Split.
	si := crdbtest.Split(key)
	row, eq := ks.roachKeys.Search(key[:si-1])
	if eq {
		return ks.seekGEOnSuffix(row, key[si:]), true
	}
	return row, false
}

// seekGEOnSuffix is a helper function for SeekGE when a seek key's prefix
// exactly matches a row. seekGEOnSuffix finds the first row at index or later
// with the same prefix as index and a suffix greater than or equal to [suffix],
// or if no such row exists, the next row with a different prefix.
func (ks *cockroachKeySeeker) seekGEOnSuffix(index int, seekSuffix []byte) (row int) {
	// The search key's prefix exactly matches the prefix of the row at index.
	const withWall = 9
	const withLogical = withWall + 4
	const withSynthetic = withLogical + 1
	var seekWallTime uint64
	var seekLogicalTime uint32
	switch len(seekSuffix) {
	case 0:
		// The search key has no suffix, so it's the smallest possible key with
		// its prefix. Return the row. This is a common case where the user is
		// seeking to the most-recent row and just wants the smallest key with
		// the prefix.
		return index
	case withLogical, withSynthetic:
		seekWallTime = binary.BigEndian.Uint64(seekSuffix)
		seekLogicalTime = binary.BigEndian.Uint32(seekSuffix[8:])
	case withWall:
		seekWallTime = binary.BigEndian.Uint64(seekSuffix)
	default:
		// The suffix is untyped. Compare the untyped suffixes.
		// Binary search between [index, prefixChanged.SeekSetBitGE(index+1)].
		//
		// Define f(i) = true iff key at i is >= seek key.
		// Invariant: f(l-1) == false, f(u) == true.
		l := index
		u := ks.reader.prefixChanged.SeekSetBitGE(index + 1)
		for l < u {
			h := int(uint(l+u) >> 1) // avoid overflow when computing h
			// l ≤ h < u
			if bytes.Compare(ks.untypedVersions.At(h), seekSuffix) >= 0 {
				u = h // preserves f(u) == true
			} else {
				l = h + 1 // preserves f(l-1) == false
			}
		}
		return l
	}
	// Seeking among MVCC versions using a MVCC timestamp.

	// TODO(jackson): What if the row has an untyped suffix?

	// Binary search between [index, prefixChanged.SeekSetBitGE(index+1)].
	//
	// Define f(i) = true iff key at i is >= seek key.
	// Invariant: f(l-1) == false, f(u) == true.
	l := index
	u := ks.reader.prefixChanged.SeekSetBitGE(index + 1)
	for l < u {
		h := int(uint(l+u) >> 1) // avoid overflow when computing h
		// l ≤ h < u
		hWallTime := ks.mvccWallTimes.At(h)
		if hWallTime < seekWallTime ||
			(hWallTime == seekWallTime && uint32(ks.mvccLogical.At(h)) <= seekLogicalTime) {
			u = h // preserves f(u) = true
		} else {
			l = h + 1 // preserves f(l-1) = false
		}
	}
	return l
}

// MaterializeUserKey is part of the KeySeeker interface.
func (ks *cockroachKeySeeker) MaterializeUserKey(ki *PrefixBytesIter, prevRow, row int) []byte {
	if prevRow+1 == row && prevRow >= 0 {
		ks.roachKeys.SetNext(ki)
	} else {
		ks.roachKeys.SetAt(ki, row)
	}

	roachKeyLen := len(ki.buf)
	ptr := unsafe.Pointer(uintptr(unsafe.Pointer(unsafe.SliceData(ki.buf))) + uintptr(roachKeyLen))
	mvccWall := ks.mvccWallTimes.At(row)
	mvccLogical := uint32(ks.mvccLogical.At(row))
	if mvccWall == 0 && mvccLogical == 0 {
		// This is not an MVCC key. Use the untyped suffix.
		untypedVersion := ks.untypedVersions.At(row)
		if len(untypedVersion) == 0 {
			res := ki.buf[:roachKeyLen+1]
			res[roachKeyLen] = 0
			return res
		}
		// Slice first, to check that the capacity is sufficient.
		res := ki.buf[:roachKeyLen+1+len(untypedVersion)]
		*(*byte)(ptr) = 0
		memmove(
			unsafe.Pointer(uintptr(ptr)+1),
			unsafe.Pointer(unsafe.SliceData(untypedVersion)),
			uintptr(len(untypedVersion)),
		)
		return res
	}

	// Inline binary.BigEndian.PutUint64. Note that this code is converted into
	// word-size instructions by the compiler.
	*(*byte)(ptr) = 0
	*(*byte)(unsafe.Pointer(uintptr(ptr) + 1)) = byte(mvccWall >> 56)
	*(*byte)(unsafe.Pointer(uintptr(ptr) + 2)) = byte(mvccWall >> 48)
	*(*byte)(unsafe.Pointer(uintptr(ptr) + 3)) = byte(mvccWall >> 40)
	*(*byte)(unsafe.Pointer(uintptr(ptr) + 4)) = byte(mvccWall >> 32)
	*(*byte)(unsafe.Pointer(uintptr(ptr) + 5)) = byte(mvccWall >> 24)
	*(*byte)(unsafe.Pointer(uintptr(ptr) + 6)) = byte(mvccWall >> 16)
	*(*byte)(unsafe.Pointer(uintptr(ptr) + 7)) = byte(mvccWall >> 8)
	*(*byte)(unsafe.Pointer(uintptr(ptr) + 8)) = byte(mvccWall)

	ptr = unsafe.Pointer(uintptr(ptr) + 9)
	// This is an MVCC key.
	if mvccLogical == 0 {
		*(*byte)(ptr) = 9
		return ki.buf[:len(ki.buf)+10]
	}

	// Inline binary.BigEndian.PutUint32.
	*(*byte)(ptr) = byte(mvccWall >> 24)
	*(*byte)(unsafe.Pointer(uintptr(ptr) + 1)) = byte(mvccWall >> 16)
	*(*byte)(unsafe.Pointer(uintptr(ptr) + 2)) = byte(mvccWall >> 8)
	*(*byte)(unsafe.Pointer(uintptr(ptr) + 3)) = byte(mvccWall)
	*(*byte)(unsafe.Pointer(uintptr(ptr) + 4)) = 13
	return ki.buf[:len(ki.buf)+14]
}

// MaterializeUserKeyWithSyntheticSuffix is part of the KeySeeker interface.
func (ks *cockroachKeySeeker) MaterializeUserKeyWithSyntheticSuffix(
	ki *PrefixBytesIter, suffix []byte, prevRow, row int,
) []byte {
	if prevRow+1 == row && prevRow >= 0 {
		ks.roachKeys.SetNext(ki)
	} else {
		ks.roachKeys.SetAt(ki, row)
	}

	// Slice first, to check that the capacity is sufficient.
	res := ki.buf[:len(ki.buf)+1+len(suffix)]
	ptr := unsafe.Pointer(uintptr(unsafe.Pointer(unsafe.SliceData(ki.buf))) + uintptr(len(ki.buf)))
	*(*byte)(ptr) = 0
	memmove(unsafe.Pointer(uintptr(ptr)+1), unsafe.Pointer(unsafe.SliceData(suffix)), uintptr(len(suffix)))
	return res
}

// Release is part of the KeySeeker interface.
func (ks *cockroachKeySeeker) Release() {
	*ks = cockroachKeySeeker{}
	cockroachKeySeekerPool.Put(ks)
}

func TestCockroachDataBlock(t *testing.T) {
	const targetBlockSize = 32 << 10
	const valueLen = 100
	seed := uint64(time.Now().UnixNano())
	t.Logf("seed: %d", seed)
	rng := rand.New(rand.NewSource(seed))
	serializedBlock, keys, values := generateDataBlock(rng, targetBlockSize, crdbtest.KeyConfig{
		PrefixAlphabetLen: 26,
		PrefixLen:         12,
		AvgKeysPerPrefix:  2,
		BaseWallTime:      seed,
	}, valueLen)

	var reader DataBlockReader
	var it DataBlockIter
	it.InitOnce(cockroachKeySchema, crdbtest.Compare, crdbtest.Split, getLazyValuer(func([]byte) base.LazyValue {
		return base.LazyValue{ValueOrHandle: []byte("mock external value")}
	}))
	reader.Init(cockroachKeySchema, serializedBlock)
	if err := it.Init(&reader, block.IterTransforms{}); err != nil {
		t.Fatal(err)
	}

	t.Run("Next", func(t *testing.T) {
		// Scan the block using Next and ensure that all the keys values match.
		i := 0
		for kv := it.First(); kv != nil; i, kv = i+1, it.Next() {
			if !bytes.Equal(kv.K.UserKey, keys[i]) {
				t.Fatalf("expected %q, but found %q", keys[i], kv.K.UserKey)
			}
			if !bytes.Equal(kv.V.InPlaceValue(), values[i]) {
				t.Fatalf("expected %x, but found %x", values[i], kv.V.InPlaceValue())
			}
		}
		require.Equal(t, len(keys), i)
	})
	t.Run("SeekGE", func(t *testing.T) {
		rng := rand.New(rand.NewSource(seed))
		for _, i := range rng.Perm(len(keys)) {
			kv := it.SeekGE(keys[i], base.SeekGEFlagsNone)
			if kv == nil {
				t.Fatalf("%q not found", keys[i])
			}
			if !bytes.Equal(kv.V.InPlaceValue(), values[i]) {
				t.Fatalf(
					"expected:\n    %x\nfound:\n    %x\nquery key:\n    %x\nreturned key:\n    %x",
					values[i], kv.V.InPlaceValue(), keys[i], kv.K.UserKey)
			}
		}
	})
}

// generateDataBlock writes out a random cockroach data block using the given
// parameters. Returns the serialized block data and the keys and values
// written.
func generateDataBlock(
	rng *rand.Rand, targetBlockSize int, cfg crdbtest.KeyConfig, valueLen int,
) (data []byte, keys [][]byte, values [][]byte) {
	keys, values = crdbtest.RandomKVs(rng, targetBlockSize/valueLen, cfg, valueLen)

	var w DataBlockWriter
	w.Init(cockroachKeySchema)
	count := 0
	for w.Size() < targetBlockSize {
		ik := base.MakeInternalKey(keys[count], base.SeqNum(rng.Uint64n(uint64(base.SeqNumMax))), base.InternalKeyKindSet)
		kcmp := w.KeyWriter.ComparePrev(ik.UserKey)
		w.Add(ik, values[count], block.InPlaceValuePrefix(kcmp.PrefixEqual()), kcmp, false /* isObsolete */)
		count++
	}
	data, _ = w.Finish(w.Rows(), w.Size())
	return data, keys[:count], values[:count]
}

func BenchmarkCockroachDataBlockWriter(b *testing.B) {
	for _, alphaLen := range []int{4, 8, 26} {
		for _, lenSharedPct := range []float64{0.25, 0.5} {
			for _, prefixLen := range []int{8, 32, 128} {
				lenShared := int(float64(prefixLen) * lenSharedPct)
				for _, valueLen := range []int{8, 128, 1024} {
					keyConfig := crdbtest.KeyConfig{
						PrefixAlphabetLen: alphaLen,
						PrefixLen:         prefixLen,
						PrefixLenShared:   lenShared,
						PercentLogical:    0,
						AvgKeysPerPrefix:  2,
						BaseWallTime:      uint64(time.Now().UnixNano()),
					}
					b.Run(fmt.Sprintf("%s,valueLen=%d", keyConfig, valueLen), func(b *testing.B) {
						benchmarkCockroachDataBlockWriter(b, keyConfig, valueLen)
					})
				}
			}
		}
	}
}

func benchmarkCockroachDataBlockWriter(b *testing.B, keyConfig crdbtest.KeyConfig, valueLen int) {
	const targetBlockSize = 32 << 10
	seed := uint64(time.Now().UnixNano())
	rng := rand.New(rand.NewSource(seed))
	_, keys, values := generateDataBlock(rng, targetBlockSize, keyConfig, valueLen)

	var w DataBlockWriter
	w.Init(cockroachKeySchema)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w.Reset()
		var count int
		for w.Size() < targetBlockSize {
			ik := base.MakeInternalKey(keys[count], base.SeqNum(rng.Uint64n(uint64(base.SeqNumMax))), base.InternalKeyKindSet)
			kcmp := w.KeyWriter.ComparePrev(ik.UserKey)
			w.Add(ik, values[count], block.InPlaceValuePrefix(kcmp.PrefixEqual()), kcmp, false /* isObsolete */)
			count++
		}
		_, _ = w.Finish(w.Rows(), w.Size())
	}
}

func BenchmarkCockroachDataBlockIterFull(b *testing.B) {
	for _, alphaLen := range []int{4, 8, 26} {
		for _, lenSharedPct := range []float64{0.25, 0.5} {
			for _, prefixLen := range []int{8, 32, 128} {
				lenShared := int(float64(prefixLen) * lenSharedPct)
				for _, avgKeysPerPrefix := range []int{1, 10, 100} {
					for _, percentLogical := range []int{0, 50} {
						for _, valueLen := range []int{8, 128, 1024} {
							cfg := benchConfig{
								KeyConfig: crdbtest.KeyConfig{
									PrefixAlphabetLen: alphaLen,
									PrefixLen:         prefixLen,
									PrefixLenShared:   lenShared,
									AvgKeysPerPrefix:  avgKeysPerPrefix,
									PercentLogical:    percentLogical,
								},
								ValueLen: valueLen,
							}
							b.Run(cfg.String(), func(b *testing.B) {
								benchmarkCockroachDataBlockIter(b, cfg, block.IterTransforms{})
							})
						}
					}
				}
			}
		}
	}
}

var shortBenchConfigs = []benchConfig{
	{
		KeyConfig: crdbtest.KeyConfig{
			PrefixAlphabetLen: 8,
			PrefixLen:         8,
			PrefixLenShared:   4,
			AvgKeysPerPrefix:  4,
			PercentLogical:    10,
		},
		ValueLen: 8,
	},
	{
		KeyConfig: crdbtest.KeyConfig{
			PrefixAlphabetLen: 8,
			PrefixLen:         128,
			PrefixLenShared:   64,
			AvgKeysPerPrefix:  4,
			PercentLogical:    10,
		},
		ValueLen: 128,
	},
}

func BenchmarkCockroachDataBlockIterShort(b *testing.B) {
	for _, cfg := range shortBenchConfigs {
		b.Run(cfg.String(), func(b *testing.B) {
			benchmarkCockroachDataBlockIter(b, cfg, block.IterTransforms{})
		})
	}
}

func BenchmarkCockroachDataBlockIterTransforms(b *testing.B) {
	transforms := []struct {
		description string
		transforms  block.IterTransforms
	}{
		{},
		{
			description: "SynthSeqNum",
			transforms: block.IterTransforms{
				SyntheticSeqNum: 1234,
			},
		},
		{
			description: "HideObsolete",
			transforms: block.IterTransforms{
				HideObsoletePoints: true,
			},
		},
		{
			description: "SyntheticPrefix",
			transforms: block.IterTransforms{
				SyntheticPrefix: []byte("prefix_"),
			},
		},
		{
			description: "SyntheticSuffix",
			transforms: block.IterTransforms{
				SyntheticSuffix: crdbtest.EncodeTimestamp(make([]byte, 0, 20), 1_000_000_000_000, 0)[1:],
			},
		},
	}
	for _, cfg := range shortBenchConfigs {
		for _, t := range transforms {
			name := cfg.String() + crstrings.If(t.description != "", ","+t.description)
			b.Run(name, func(b *testing.B) {
				benchmarkCockroachDataBlockIter(b, cfg, t.transforms)
			})
		}
	}
}

type benchConfig struct {
	crdbtest.KeyConfig
	ValueLen int
}

func (cfg benchConfig) String() string {
	return fmt.Sprintf("%s,ValueLen=%d", cfg.KeyConfig, cfg.ValueLen)
}

func benchmarkCockroachDataBlockIter(
	b *testing.B, cfg benchConfig, transforms block.IterTransforms,
) {
	const targetBlockSize = 32 << 10
	seed := uint64(time.Now().UnixNano())
	rng := rand.New(rand.NewSource(seed))
	cfg.BaseWallTime = seed

	serializedBlock, keys, _ := generateDataBlock(rng, targetBlockSize, cfg.KeyConfig, cfg.ValueLen)

	var reader DataBlockReader
	var it DataBlockIter
	it.InitOnce(cockroachKeySchema, crdbtest.Compare, crdbtest.Split, getLazyValuer(func([]byte) base.LazyValue {
		return base.LazyValue{ValueOrHandle: []byte("mock external value")}
	}))
	reader.Init(cockroachKeySchema, serializedBlock)
	if err := it.Init(&reader, transforms); err != nil {
		b.Fatal(err)
	}
	avgRowSize := float64(len(serializedBlock)) / float64(len(keys))

	if transforms.SyntheticPrefix.IsSet() {
		for i := range keys {
			keys[i] = slices.Concat(transforms.SyntheticPrefix, keys[i])
		}
	}

	b.Run("Next", func(b *testing.B) {
		kv := it.First()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if kv == nil {
				kv = it.First()
			} else {
				kv = it.Next()
			}
		}
		b.StopTimer()
		b.ReportMetric(avgRowSize, "bytes/row")
	})
	for _, queryLatest := range []bool{false, true} {
		b.Run("SeekGE"+crstrings.If(queryLatest, "Latest"), func(b *testing.B) {
			rng := rand.New(rand.NewSource(seed))
			const numQueryKeys = 65536
			baseWallTime := cfg.BaseWallTime
			if queryLatest {
				baseWallTime += 24 * uint64(time.Hour)
			}
			queryKeys := crdbtest.RandomQueryKeys(rng, numQueryKeys, keys, baseWallTime)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				k := queryKeys[i%numQueryKeys]
				if kv := it.SeekGE(k, base.SeekGEFlagsNone); kv == nil {
					// SeekGE should always end up finding a key if we are querying for the
					// latest version of each prefix and we are not hiding any points.
					if queryLatest && !transforms.HideObsoletePoints {
						b.Fatalf("%q not found", k)
					}
				}
			}
			b.StopTimer()
			b.ReportMetric(avgRowSize, "bytes/row")
		})
	}
}
