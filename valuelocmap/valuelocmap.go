package valuelocmap

import (
	"fmt"
	"math"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/gholt/brimtext"
)

type config struct {
	cores           int
	pageSize        int
	splitMultiplier float64
}

func resolveConfig(opts ...func(*config)) *config {
	cfg := &config{}
	if env := os.Getenv("BRIMSTORE_VALUELOCMAP_CORES"); env != "" {
		if val, err := strconv.Atoi(env); err == nil {
			cfg.cores = val
		}
	} else if env = os.Getenv("BRIMSTORE_CORES"); env != "" {
		if val, err := strconv.Atoi(env); err == nil {
			cfg.cores = val
		}
	}
	if cfg.cores <= 0 {
		cfg.cores = runtime.GOMAXPROCS(0)
	}
	if env := os.Getenv("BRIMSTORE_VALUELOCMAP_PAGESIZE"); env != "" {
		if val, err := strconv.Atoi(env); err == nil {
			cfg.pageSize = val
		}
	}
	if cfg.pageSize <= 0 {
		cfg.pageSize = 524288
	}
	if env := os.Getenv("BRIMSTORE_VALUESLOCMAP_SPLITMULTIPLIER"); env != "" {
		if val, err := strconv.ParseFloat(env, 64); err == nil {
			cfg.splitMultiplier = val
		}
	}
	if cfg.splitMultiplier <= 0 {
		cfg.splitMultiplier = 3.0
	}
	for _, opt := range opts {
		opt(cfg)
	}
	if cfg.cores < 1 {
		cfg.cores = 1
	}
	if cfg.pageSize < 1 {
		cfg.pageSize = 1
	}
	if cfg.splitMultiplier <= 0 {
		cfg.splitMultiplier = 0.01
	}
	return cfg
}

// OptCores indicates how many cores may be in use (for calculating the number
// of locks to create, for example) and how many cores may be used for
// background jobs. Defaults to env BRIMSTORE_VALUELOCMAP_CORES,
// BRIMSTORE_CORES, or GOMAXPROCS.
func OptCores(n int) func(*config) {
	return func(cfg *config) {
		cfg.cores = n
	}
}

// OptPageSize controls the size of each chunk of memory allocated. Defaults to
// env BRIMSTORE_VALUELOCMAP_PAGESIZE or 524,288.
func OptPageSize(b int) func(*config) {
	return func(cfg *config) {
		cfg.pageSize = b
	}
}

// OptSplitMultiplier indicates how full a memory page can get before being
// split into two pages. Defaults to env BRIMSTORE_VALUELOCMAP_SPLITMULTIPLIER
// or 3.0.
func OptSplitMultiplier(m float64) func(*config) {
	return func(cfg *config) {
		cfg.splitMultiplier = m
	}
}

type ValueLocMap struct {
	root                    *valueLocNode
	cores                   int
	splitCount              int
	outOfPlaceKeyDetections int32
}

// OVERALL NOTES:
//
//  a is used to store at first, growth may then cause a split.
//  While splitting, b will be set, c and d will still be nil.
//  Once the split is complete, c and d will be set.
//  Shrinking may cause an unsplit.
//  During unsplit, a and e will be set, c and d will become nil.
//  e is considered read-only/fallback during unsplit.
//  Once unsplit is done, e will become nil.
//
// FOR SPEED'S SAKE THERE IS AN ASSUMPTION THAT ALL READS AND WRITES ACTIVE AT
// THE START OR DURING ONE RESIZE WILL BE COMPLETED BEFORE ANOTHER RESIZE OF
// THE SAME KEY SPACE STARTS.
//
// As one example, if a write starts, gathers a and b (split in progress), and
// somehow is stalled for an extended period of time and the split completes
// and another subsplit happens, then the write awakens and continues, it will
// have references to quite possibly incorrect memory areas.
//
// This code is not meant to be used with over-subscribed core counts that
// would create artificial goroutine slowdowns / starvations or with extremely
// small memory page sizes. In the rare case a single core on a CPU is going
// bad and is running slow, this code should still be safe as it is meant to be
// run with the data stored on multiple server replicas where if one server is
// behaving badly the other servers will supersede it. In addition, background
// routines are in place to detect and repair any out of place data and
// so these rare anomalies shoud be resolved fairly quickly. Any repairs done
// will be reported via the gatherStats' outOfPlaceKeyDetections value in case
// their rarity isn't as uncommon as they should be.
//
// If you would rather have perfect correctness at the cost of speed, you will
// have to use an additional lock around all uses of a-e.
type valueLocNode struct {
	leftMask                uint64
	rangeStart              uint64
	rangeStop               uint64
	a                       *valuesLocStore
	b                       *valuesLocStore
	c                       *valueLocNode
	d                       *valueLocNode
	e                       *valuesLocStore
	resizing                bool
	resizingLock            sync.RWMutex
	cores                   int
	splitCount              int
	outOfPlaceKeyDetections int32
}

type valuesLocStore struct {
	buckets []valueLoc
	locks   []sync.RWMutex
	used    int32
}

type valueLoc struct {
	next      *valueLoc
	keyA      uint64
	keyB      uint64
	timestamp uint64
	blockID   uint16
	offset    uint32
	length    uint32
}

type valuesLocMapStats struct {
	extended                bool
	wg                      sync.WaitGroup
	depth                   uint64
	depthCounts             []uint64
	sections                uint64
	storages                uint64
	buckets                 uint64
	bucketCounts            []uint64
	splitCount              uint64
	outOfPlaceKeyDetections int32
	locs                    uint64
	pointerLocs             uint64
	unused                  uint64
	used                    uint64
	active                  uint64
	length                  uint64
	tombstones              uint64
}

type valuesLocMapBackground struct {
	wg                  sync.WaitGroup
	tombstoneCutoff     uint64
	tombstonesDiscarded uint64
	tombstonesRetained  uint64
}

func NewValueLocMap(opts ...func(*config)) *ValueLocMap {
	cfg := resolveConfig(opts...)
	bucketCount := cfg.pageSize / int(unsafe.Sizeof(valueLoc{}))
	if bucketCount < 1 {
		bucketCount = 1
	}
	lockCount := cfg.cores
	if lockCount > bucketCount {
		lockCount = bucketCount
	}
	splitMultiplier := cfg.splitMultiplier
	if splitMultiplier <= 0 {
		splitMultiplier = 0.1
	}
	return &ValueLocMap{
		root: &valueLocNode{
			leftMask:   uint64(1) << 63,
			rangeStart: 0,
			rangeStop:  math.MaxUint64,
			a: &valuesLocStore{
				buckets: make([]valueLoc, bucketCount),
				locks:   make([]sync.RWMutex, lockCount),
			},
		},
		cores:      cfg.cores,
		splitCount: int(float64(bucketCount) * cfg.splitMultiplier),
	}
}

func (vlm *ValueLocMap) Get(keyA uint64, keyB uint64) (uint64, uint16, uint32, uint32) {
	var timestamp uint64
	var blockID uint16
	var offset uint32
	var length uint32
	vln := vlm.root
VLN_SELECTION:
	// Traverse the tree until we hit a leaf node (no c [and therefore no d]).
	for {
		c := (*valueLocNode)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&vln.c))))
		if c == nil {
			break
		}
		if keyA&vln.leftMask == 0 {
			vln = c
		} else {
			vln = (*valueLocNode)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&vln.d))))
		}
	}
	a := (*valuesLocStore)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&vln.a))))
	bix := keyB % uint64(len(a.buckets))
	lix := bix % uint64(len(a.locks))
	f := func(s *valuesLocStore, fb *valuesLocStore) {
		blockID = 0
		s.locks[lix].RLock()
		if fb != nil {
			fb.locks[lix].RLock()
		}
		for item := &s.buckets[bix]; item != nil; item = item.next {
			if item.blockID != 0 && item.keyA == keyA && item.keyB == keyB {
				timestamp, blockID, offset, length = item.timestamp, item.blockID, item.offset, item.length
				break
			}
		}
		if fb != nil && blockID == 0 {
			for item := &fb.buckets[bix]; item != nil; item = item.next {
				if item.blockID != 0 && item.keyA == keyA && item.keyB == keyB {
					timestamp, blockID, offset, length = item.timestamp, item.blockID, item.offset, item.length
					break
				}
			}
		}
		if fb != nil {
			fb.locks[lix].RUnlock()
		}
		s.locks[lix].RUnlock()
	}
	if keyA&vln.leftMask == 0 {
		// If we're on the left side, even if a split is in progress or happens
		// while we're reading it won't matter because we'd still read from the
		// same memory block, assuming more than one split doesn't occur while
		// we're reading.
		f(a, nil)
		// If an unsplit happened while we were reading, store a will end up
		// nil and we need to retry the read.
		a = (*valuesLocStore)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&vln.a))))
		if a == nil {
			vln = vlm.root
			goto VLN_SELECTION
		}
	} else {
		// If we're on the right side, then things might be a bit trickier...
		b := (*valuesLocStore)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&vln.b))))
		if b != nil {
			// If a split is in progress, then we can read from b and fallback
			// to a and we're safe, assuming another split doesn't occur during
			// our read.
			f(b, a)
		} else {
			// If no split is in progress, we'll read from a and fallback to e
			// if it exists...
			f(a, (*valuesLocStore)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&vln.e)))))
			// If an unsplit happened while we were reading, store a will end
			// up nil and we need to retry the read.
			a = (*valuesLocStore)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&vln.a))))
			if a == nil {
				vln = vlm.root
				goto VLN_SELECTION
			}
			// If we pass that test, we'll double check b...
			b := (*valuesLocStore)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&vln.b))))
			if b != nil {
				// If b is set, a split started while we were reading, so we'll
				// re-read from b and fallback to a and we're safe, assuming
				// another split doesn't occur during this re-read.
				f(b, a)
			} else {
				// If b isn't set, either no split happened while we were
				// reading, or the split happened and finished while we were
				// reading, so we'll double check d to find out...
				d := (*valueLocNode)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&vln.d))))
				if d != nil {
					// If a complete split occurred while we were reading,
					// we'll traverse the tree node and jump back to any
					// further tree node traversal and retry the read.
					vln = d
					goto VLN_SELECTION
				}
			}
		}
	}
	return timestamp, blockID, offset, length
}

func (vlm *ValueLocMap) Set(keyA uint64, keyB uint64, timestamp uint64, blockID uint16, offset uint32, length uint32, evenIfSameTimestamp bool) uint64 {
	var oldTimestamp uint64
	var originalOldTimestampCheck bool
	var originalOldTimestamp uint64
	var vlmPrev *valueLocNode
	vln := vlm.root
VLN_SELECTION:
	// Traverse the tree until we hit a leaf node (no c [and therefore no d]).
	for {
		c := (*valueLocNode)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&vln.c))))
		if c == nil {
			break
		}
		vlmPrev = vln
		if keyA&vln.leftMask == 0 {
			vln = c
		} else {
			vln = (*valueLocNode)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&vln.d))))
		}
	}
	a := (*valuesLocStore)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&vln.a))))
	bix := keyB % uint64(len(a.buckets))
	lix := bix % uint64(len(a.locks))
	f := func(s *valuesLocStore, fb *valuesLocStore) {
		oldTimestamp = 0
		var sMatch *valueLoc
		var fbMatch *valueLoc
		var unusedItem *valueLoc
		s.locks[lix].Lock()
		if fb != nil {
			fb.locks[lix].Lock()
		}
		for item := &s.buckets[bix]; item != nil; item = item.next {
			if item.blockID == 0 {
				if unusedItem == nil {
					unusedItem = item
				}
				continue
			}
			if item.keyA == keyA && item.keyB == keyB {
				sMatch = item
				break
			}
		}
		if fb != nil {
			for item := &fb.buckets[bix]; item != nil; item = item.next {
				if item.blockID == 0 {
					continue
				}
				if item.keyA == keyA && item.keyB == keyB {
					fbMatch = item
					break
				}
			}
		}
		if sMatch != nil {
			if fbMatch != nil {
				if sMatch.timestamp >= fbMatch.timestamp {
					oldTimestamp = sMatch.timestamp
					if timestamp > sMatch.timestamp || (evenIfSameTimestamp && timestamp == sMatch.timestamp) {
						sMatch.timestamp = timestamp
						sMatch.blockID = blockID
						sMatch.offset = offset
						sMatch.length = length
					}
				} else {
					oldTimestamp = fbMatch.timestamp
					if timestamp > fbMatch.timestamp || (evenIfSameTimestamp && timestamp == fbMatch.timestamp) {
						sMatch.timestamp = timestamp
						sMatch.blockID = blockID
						sMatch.offset = offset
						sMatch.length = length
					} else {
						sMatch.timestamp = fbMatch.timestamp
						sMatch.blockID = fbMatch.blockID
						sMatch.offset = fbMatch.offset
						sMatch.length = fbMatch.length
					}
				}
				atomic.AddInt32(&fb.used, -1)
				fbMatch.blockID = 0
			} else {
				oldTimestamp = sMatch.timestamp
				if timestamp > sMatch.timestamp || (evenIfSameTimestamp && timestamp == sMatch.timestamp) {
					sMatch.timestamp = timestamp
					sMatch.blockID = blockID
					sMatch.offset = offset
					sMatch.length = length
				}
			}
		} else {
			atomic.AddInt32(&s.used, 1)
			if unusedItem == nil {
				unusedItem = &valueLoc{next: s.buckets[bix].next}
				s.buckets[bix].next = unusedItem
			}
			unusedItem.keyA = keyA
			unusedItem.keyB = keyB
			if fbMatch != nil {
				oldTimestamp = fbMatch.timestamp
				if timestamp > fbMatch.timestamp || (evenIfSameTimestamp && timestamp == fbMatch.timestamp) {
					unusedItem.timestamp = timestamp
					unusedItem.blockID = blockID
					unusedItem.offset = offset
					unusedItem.length = length
				} else {
					unusedItem.timestamp = fbMatch.timestamp
					unusedItem.blockID = fbMatch.blockID
					unusedItem.offset = fbMatch.offset
					unusedItem.length = fbMatch.length
				}
				atomic.AddInt32(&fb.used, -1)
				fbMatch.blockID = 0
			} else {
				unusedItem.timestamp = timestamp
				unusedItem.blockID = blockID
				unusedItem.offset = offset
				unusedItem.length = length
			}
		}
		if fb != nil {
			fb.locks[lix].Unlock()
		}
		s.locks[lix].Unlock()
	}
	if keyA&vln.leftMask == 0 {
		// If we're on the left side, even if a split is in progress or happens
		// while we're writing it won't matter because we'd still write to the
		// same memory block, assuming more than one split doesn't occur while
		// we're writing.
		f(a, nil)
		// If our write was not superseded...
		if oldTimestamp < timestamp || (evenIfSameTimestamp && oldTimestamp == timestamp) {
			// If an unsplit happened while we were writing, store a will end
			// up nil and we need to clear what we wrote and retry the write.
			aAgain := (*valuesLocStore)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&vln.a))))
			if aAgain == nil {
				a.locks[lix].Lock()
				for item := &a.buckets[bix]; item != nil; item = item.next {
					if item.blockID == 0 {
						continue
					}
					if item.keyA == keyA && item.keyB == keyB {
						if item.timestamp == timestamp && item.blockID == blockID && item.offset == offset && item.length == length {
							item.blockID = 0
						}
						break
					}
				}
				a.locks[lix].Unlock()
				if !originalOldTimestampCheck {
					originalOldTimestampCheck = true
					originalOldTimestamp = oldTimestamp
				}
				vln = vlm.root
				goto VLN_SELECTION
			}
			// Otherwise, we read b and e and if both are nil (no split/unsplit
			// in progress) we check a's used counter to see if we should
			// request a split/unsplit.
			b := (*valuesLocStore)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&vln.b))))
			if b == nil {
				e := (*valuesLocStore)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&vln.e))))
				if e == nil {
					used := atomic.LoadInt32(&a.used)
					if int(used) > vlm.splitCount {
						go vln.split(vlm.cores)
					} else if used == 0 && vlmPrev != nil {
						go vlmPrev.unsplit(vlm.cores)
					}
				}
			}
		}
	} else {
		// If we're on the right side, then things might be a bit trickier...
		b := (*valuesLocStore)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&vln.b))))
		if b != nil {
			// If a split is in progress, then we can write to b checking a for
			// any competing value and we're safe, assuming another split
			// doesn't occur during our write.
			f(b, a)
		} else {
			// If no split is in progress, we'll write to a checking e (if it
			// exists) for any competing value...
			f(a, (*valuesLocStore)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&vln.e)))))
			// If our write was not superseded...
			if oldTimestamp < timestamp || (evenIfSameTimestamp && oldTimestamp == timestamp) {
				// If an unsplit happened while we were writing, store a will
				// end up nil and we need to clear what we wrote and retry the
				// write.
				aAgain := (*valuesLocStore)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&vln.a))))
				if aAgain == nil {
					a.locks[lix].Lock()
					for item := &a.buckets[bix]; item != nil; item = item.next {
						if item.blockID == 0 {
							continue
						}
						if item.keyA == keyA && item.keyB == keyB {
							if item.timestamp == timestamp && item.blockID == blockID && item.offset == offset && item.length == length {
								item.blockID = 0
							}
							break
						}
					}
					a.locks[lix].Unlock()
					if !originalOldTimestampCheck {
						originalOldTimestampCheck = true
						originalOldTimestamp = oldTimestamp
					}
					vln = vlm.root
					goto VLN_SELECTION
				}
				// If we pass that test, we'll double check b...
				b := (*valuesLocStore)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&vln.b))))
				if b != nil {
					// If b is set, a split started while we were writing, so
					// we'll re-write to b checking a for a competing value
					// (should at least be the one we just wrote) and we're
					// safe, assuming another split doesn't occur during our
					// write.
					if !originalOldTimestampCheck {
						originalOldTimestampCheck = true
						originalOldTimestamp = oldTimestamp
					}
					f(b, a)
				} else {
					// If b isn't set, either no split happened while we were
					// writing, or the split happened and finished while we
					// were writing, so we'll double check d to find out...
					d := (*valueLocNode)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&vln.d))))
					if d != nil {
						// If a complete split occurred while we were writing,
						// we'll clear our write and then we'll traverse the
						// tree node and jump back to any further tree node
						// traversal and retry the write.
						a.locks[lix].Lock()
						for item := &a.buckets[bix]; item != nil; item = item.next {
							if item.blockID == 0 {
								continue
							}
							if item.keyA == keyA && item.keyB == keyB {
								if item.timestamp == timestamp && item.blockID == blockID && item.offset == offset && item.length == length {
									item.blockID = 0
								}
								break
							}
						}
						a.locks[lix].Unlock()
						if !originalOldTimestampCheck {
							originalOldTimestampCheck = true
							originalOldTimestamp = oldTimestamp
						}
						vln = d
						goto VLN_SELECTION
					} else {
						// If no split is progress or had ocurred while we were
						// writing, we check e to see if an unsplit is in
						// progress and, if not, we check a's used counter to
						// see if we should request a split/unsplit.
						e := (*valuesLocStore)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&vln.e))))
						if e == nil {
							used := atomic.LoadInt32(&a.used)
							if int(used) > vlm.splitCount {
								go vln.split(vlm.cores)
							} else if used == 0 && vlmPrev != nil {
								go vlmPrev.unsplit(vlm.cores)
							}
						}
					}
				}
			}
		}
	}
	if originalOldTimestampCheck && originalOldTimestamp < oldTimestamp {
		oldTimestamp = originalOldTimestamp
	}
	return oldTimestamp
}

func (vlm *ValueLocMap) isResizing() bool {
	return vlm.root.isResizing()
}

func (vln *valueLocNode) isResizing() bool {
	vln.resizingLock.RLock()
	if vln.resizing {
		vln.resizingLock.RUnlock()
		return true
	}
	c := (*valueLocNode)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&vln.c))))
	if c != nil && c.isResizing() {
		vln.resizingLock.RUnlock()
		return true
	}
	d := (*valueLocNode)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&vln.d))))
	if d != nil && d.isResizing() {
		vln.resizingLock.RUnlock()
		return true
	}
	vln.resizingLock.RUnlock()
	return false
}

func (vlm *ValueLocMap) gatherStats(extended bool) *valuesLocMapStats {
	stats := &valuesLocMapStats{}
	if extended {
		stats.extended = true
		stats.depthCounts = []uint64{0}
		stats.splitCount = uint64(vlm.splitCount)
		stats.outOfPlaceKeyDetections = vlm.outOfPlaceKeyDetections
	}
	vlm.root.gatherStatsHelper(stats)
	stats.wg.Wait()
	if extended {
		stats.depthCounts = stats.depthCounts[1:]
	}
	return stats
}

func (vln *valueLocNode) gatherStatsHelper(stats *valuesLocMapStats) {
	if stats.extended {
		stats.sections++
		stats.depth++
		if stats.depth < uint64(len(stats.depthCounts)) {
			stats.depthCounts[stats.depth]++
		} else {
			stats.depthCounts = append(stats.depthCounts, 1)
		}
	}
	c := (*valueLocNode)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&vln.c))))
	if c != nil {
		d := (*valueLocNode)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&vln.d))))
		if stats.extended {
			depthOrig := stats.depth
			c.gatherStatsHelper(stats)
			depthC := stats.depth
			stats.depth = depthOrig
			d.gatherStatsHelper(stats)
			if depthC > stats.depth {
				stats.depth = depthC
			}
		} else {
			c.gatherStatsHelper(stats)
			d.gatherStatsHelper(stats)
		}
		return
	}
	f := func(s *valuesLocStore) {
		if stats.buckets == 0 {
			stats.buckets = uint64(len(s.buckets))
		}
		stats.wg.Add(1)
		go func() {
			var bucketCounts []uint64
			var pointerLocs uint64
			var locs uint64
			var unused uint64
			var used uint64
			var active uint64
			var length uint64
			var tombstones uint64
			if stats.extended {
				bucketCounts = make([]uint64, len(s.buckets))
			}
			for bix := len(s.buckets) - 1; bix >= 0; bix-- {
				lix := bix % len(s.locks)
				s.locks[lix].RLock()
				if stats.extended {
					for item := &s.buckets[bix]; item != nil; item = item.next {
						bucketCounts[bix]++
						if item.next != nil {
							pointerLocs++
						}
						locs++
						if item.blockID == 0 {
							unused++
						} else {
							used++
							if item.timestamp&1 == 0 {
								active++
								length += uint64(item.length)
							} else {
								tombstones++
							}
						}
					}
				} else {
					for item := &s.buckets[bix]; item != nil; item = item.next {
						if item.blockID > 0 {
							if item.timestamp&1 == 0 {
								active++
								length += uint64(item.length)
							}
						}
					}
				}
				s.locks[lix].RUnlock()
			}
			if stats.extended {
				atomic.AddUint64(&stats.storages, 1)
				atomic.AddUint64(&stats.pointerLocs, pointerLocs)
				atomic.AddUint64(&stats.locs, locs)
				atomic.AddUint64(&stats.used, used)
				atomic.AddUint64(&stats.unused, unused)
				atomic.AddUint64(&stats.tombstones, tombstones)
			}
			atomic.AddUint64(&stats.active, active)
			atomic.AddUint64(&stats.length, length)
			stats.wg.Done()
		}()
	}
	a := (*valuesLocStore)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&vln.a))))
	if a != nil {
		f(a)
	}
	b := (*valuesLocStore)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&vln.b))))
	if b != nil {
		f(b)
	}
	e := (*valuesLocStore)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&vln.e))))
	if e != nil {
		f(e)
	}
}

func (stats *valuesLocMapStats) String() string {
	if stats.extended {
		depthCounts := fmt.Sprintf("%d", stats.depthCounts[0])
		for i := 1; i < len(stats.depthCounts); i++ {
			depthCounts += fmt.Sprintf(" %d", stats.depthCounts[i])
		}
		return brimtext.Align([][]string{
			[]string{"depth", fmt.Sprintf("%d", stats.depth)},
			[]string{"depthCounts", depthCounts},
			[]string{"sections", fmt.Sprintf("%d", stats.sections)},
			[]string{"storages", fmt.Sprintf("%d", stats.storages)},
			[]string{"valuesLocMapPageSize", fmt.Sprintf("%d", stats.buckets*uint64(unsafe.Sizeof(valueLoc{})))},
			[]string{"bucketsPerPage", fmt.Sprintf("%d", stats.buckets)},
			[]string{"splitCount", fmt.Sprintf("%d", stats.splitCount)},
			[]string{"outOfPlaceKeyDetections", fmt.Sprintf("%d", stats.outOfPlaceKeyDetections)},
			[]string{"locs", fmt.Sprintf("%d", stats.locs)},
			[]string{"pointerLocs", fmt.Sprintf("%d %.1f%%", stats.pointerLocs, float64(stats.pointerLocs)/float64(stats.locs)*100)},
			[]string{"unused", fmt.Sprintf("%d %.1f%%", stats.unused, float64(stats.unused)/float64(stats.locs)*100)},
			[]string{"used", fmt.Sprintf("%d", stats.used)},
			[]string{"active", fmt.Sprintf("%d", stats.active)},
			[]string{"length", fmt.Sprintf("%d", stats.length)},
			[]string{"tombstones", fmt.Sprintf("%d", stats.tombstones)},
		}, nil)
	} else {
		return brimtext.Align([][]string{
			[]string{"active", fmt.Sprintf("%d", stats.active)},
			[]string{"length", fmt.Sprintf("%d", stats.length)},
		}, nil)
	}
}

func (vln *valueLocNode) split(cores int) {
	vln.resizingLock.Lock()
	a := (*valuesLocStore)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&vln.a))))
	c := (*valueLocNode)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&vln.c))))
	if vln.resizing || c != nil || int(atomic.LoadInt32(&a.used)) < vln.splitCount {
		vln.resizingLock.Unlock()
		return
	}
	vln.resizing = true
	vln.resizingLock.Unlock()
	b := &valuesLocStore{
		buckets: make([]valueLoc, len(a.buckets)),
		locks:   make([]sync.RWMutex, len(a.locks)),
	}
	atomic.StorePointer((*unsafe.Pointer)(unsafe.Pointer(&vln.b)), unsafe.Pointer(b))
	wg := &sync.WaitGroup{}
	var copies uint32
	var clears uint32
	f := func(coreOffset int, clear bool) {
		for bix := len(a.buckets) - 1 - coreOffset; bix >= 0; bix -= cores {
			lix := bix % len(a.locks)
			b.locks[lix].Lock()
			a.locks[lix].Lock()
		NEXT_ITEM_A:
			for itemA := &a.buckets[bix]; itemA != nil; itemA = itemA.next {
				if itemA.blockID == 0 || itemA.keyA&vln.leftMask == 0 {
					continue
				}
				var unusedItemB *valueLoc
				for itemB := &b.buckets[bix]; itemB != nil; itemB = itemB.next {
					if itemB.blockID == 0 {
						if unusedItemB == nil {
							unusedItemB = itemB
						}
						continue
					}
					if itemA.keyA == itemB.keyA && itemA.keyB == itemB.keyB {
						if itemA.timestamp > itemB.timestamp {
							itemB.keyA = itemA.keyA
							itemB.keyB = itemA.keyB
							itemB.timestamp = itemA.timestamp
							itemB.blockID = itemA.blockID
							itemB.offset = itemA.offset
							itemB.length = itemA.length
							atomic.AddUint32(&copies, 1)
						}
						if clear {
							atomic.AddInt32(&a.used, -1)
							itemA.blockID = 0
							atomic.AddUint32(&clears, 1)
						}
						continue NEXT_ITEM_A
					}
				}
				atomic.AddInt32(&b.used, 1)
				if unusedItemB != nil {
					unusedItemB.keyA = itemA.keyA
					unusedItemB.keyB = itemA.keyB
					unusedItemB.timestamp = itemA.timestamp
					unusedItemB.blockID = itemA.blockID
					unusedItemB.offset = itemA.offset
					unusedItemB.length = itemA.length
				} else {
					b.buckets[bix].next = &valueLoc{
						next:      b.buckets[bix].next,
						keyA:      itemA.keyA,
						keyB:      itemA.keyB,
						timestamp: itemA.timestamp,
						blockID:   itemA.blockID,
						offset:    itemA.offset,
						length:    itemA.length,
					}
				}
				atomic.AddUint32(&copies, 1)
				if clear {
					atomic.AddInt32(&a.used, -1)
					itemA.blockID = 0
					atomic.AddUint32(&clears, 1)
				}
			}
			a.locks[lix].Unlock()
			b.locks[lix].Unlock()
		}
		wg.Done()
	}
	for passes := 0; passes < 2 || copies > 0; passes++ {
		copies = 0
		wg.Add(cores)
		for core := 0; core < cores; core++ {
			go f(core, false)
		}
		wg.Wait()
	}
	for passes := 0; passes < 2 || copies > 0 || clears > 0; passes++ {
		copies = 0
		clears = 0
		wg.Add(cores)
		for core := 0; core < cores; core++ {
			go f(core, true)
		}
		wg.Wait()
	}
	newVLN := &valueLocNode{
		leftMask:   vln.leftMask >> 1,
		rangeStart: vln.rangeStart + vln.leftMask,
		rangeStop:  vln.rangeStop,
		a:          b,
	}
	atomic.StorePointer((*unsafe.Pointer)(unsafe.Pointer(&vln.d)), unsafe.Pointer(newVLN))
	newVLN = &valueLocNode{
		leftMask:   vln.leftMask >> 1,
		rangeStart: vln.rangeStart,
		rangeStop:  vln.rangeStop - vln.leftMask,
		a:          a,
	}
	atomic.StorePointer((*unsafe.Pointer)(unsafe.Pointer(&vln.c)), unsafe.Pointer(newVLN))
	vln.resizingLock.Lock()
	vln.resizing = false
	vln.resizingLock.Unlock()
}

func (vln *valueLocNode) unsplit(cores int) {
	vln.resizingLock.Lock()
	c := (*valueLocNode)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&vln.c))))
	if vln.resizing || c == nil {
		vln.resizingLock.Unlock()
		return
	}
	c.resizingLock.Lock()
	cc := (*valueLocNode)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&c.c))))
	if c.resizing || cc != nil {
		c.resizingLock.Unlock()
		vln.resizingLock.Unlock()
		return
	}
	d := (*valueLocNode)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&vln.d))))
	d.resizingLock.Lock()
	dc := (*valueLocNode)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&d.c))))
	if d.resizing || dc != nil {
		d.resizingLock.Unlock()
		c.resizingLock.Unlock()
		vln.resizingLock.Unlock()
		return
	}
	d.resizing = true
	c.resizing = true
	vln.resizing = true
	d.resizingLock.Unlock()
	c.resizingLock.Unlock()
	vln.resizingLock.Unlock()
	a := (*valuesLocStore)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&c.a))))
	e := (*valuesLocStore)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&d.a))))
	// Even if a has less items than e, we copy items from e to a because
	// get/set and other routines assume a is left and e is right.
	atomic.StorePointer((*unsafe.Pointer)(unsafe.Pointer(&vln.a)), nil)
	atomic.StorePointer((*unsafe.Pointer)(unsafe.Pointer(&vln.b)), nil)
	atomic.StorePointer((*unsafe.Pointer)(unsafe.Pointer(&vln.e)), unsafe.Pointer(e))
	atomic.StorePointer((*unsafe.Pointer)(unsafe.Pointer(&vln.a)), unsafe.Pointer(a))
	atomic.StorePointer((*unsafe.Pointer)(unsafe.Pointer(&c.a)), nil)
	atomic.StorePointer((*unsafe.Pointer)(unsafe.Pointer(&d.a)), nil)
	atomic.StorePointer((*unsafe.Pointer)(unsafe.Pointer(&vln.c)), nil)
	atomic.StorePointer((*unsafe.Pointer)(unsafe.Pointer(&vln.d)), nil)
	wg := &sync.WaitGroup{}
	var copies uint32
	var clears uint32
	f := func(coreOffset int, clear bool) {
		for bix := len(e.buckets) - 1 - coreOffset; bix >= 0; bix -= cores {
			lix := bix % len(e.locks)
			a.locks[lix].Lock()
			e.locks[lix].Lock()
		NEXT_ITEM_E:
			for itemE := &e.buckets[bix]; itemE != nil; itemE = itemE.next {
				if itemE.blockID == 0 {
					continue
				}
				var unusedItemA *valueLoc
				for itemA := &a.buckets[bix]; itemA != nil; itemA = itemA.next {
					if itemA.blockID == 0 {
						if unusedItemA == nil {
							unusedItemA = itemA
						}
						continue
					}
					if itemE.keyA == itemA.keyA && itemE.keyB == itemA.keyB {
						if itemE.timestamp > itemA.timestamp {
							itemA.keyA = itemE.keyA
							itemA.keyB = itemE.keyB
							itemA.timestamp = itemE.timestamp
							itemA.blockID = itemE.blockID
							itemA.offset = itemE.offset
							itemA.length = itemE.length
							atomic.AddUint32(&copies, 1)
						}
						if clear {
							atomic.AddInt32(&e.used, -1)
							itemE.blockID = 0
							atomic.AddUint32(&clears, 1)
						}
						continue NEXT_ITEM_E
					}
				}
				atomic.AddInt32(&a.used, 1)
				if unusedItemA != nil {
					unusedItemA.keyA = itemE.keyA
					unusedItemA.keyB = itemE.keyB
					unusedItemA.timestamp = itemE.timestamp
					unusedItemA.blockID = itemE.blockID
					unusedItemA.offset = itemE.offset
					unusedItemA.length = itemE.length
				} else {
					a.buckets[bix].next = &valueLoc{
						next:      a.buckets[bix].next,
						keyA:      itemE.keyA,
						keyB:      itemE.keyB,
						timestamp: itemE.timestamp,
						blockID:   itemE.blockID,
						offset:    itemE.offset,
						length:    itemE.length,
					}
				}
				atomic.AddUint32(&copies, 1)
				if clear {
					atomic.AddInt32(&e.used, -1)
					itemE.blockID = 0
					atomic.AddUint32(&clears, 1)
				}
			}
			e.locks[lix].Unlock()
			a.locks[lix].Unlock()
		}
		wg.Done()
	}
	for passes := 0; passes < 2 || copies > 0; passes++ {
		copies = 0
		wg.Add(cores)
		for core := 0; core < cores; core++ {
			go f(core, false)
		}
		wg.Wait()
	}
	for passes := 0; passes < 2 || copies > 0 || clears > 0; passes++ {
		copies = 0
		clears = 0
		wg.Add(cores)
		for core := 0; core < cores; core++ {
			go f(core, true)
		}
		wg.Wait()
	}
	atomic.StorePointer((*unsafe.Pointer)(unsafe.Pointer(&vln.e)), nil)
	vln.resizingLock.Lock()
	vln.resizing = false
	vln.resizingLock.Unlock()
}

func (vlm *ValueLocMap) Background(iteration uint16) {
	// This is what I had as the background job before. Need to reimplement
	// this tombstone expiration as part of scanCount most likely.
	// bg := &valuesLocMapBackground{tombstoneCutoff: uint64(time.Now().UnixNano()) - vs.tombstoneAge}
	// vlm.backgroundHelper(bg, nil)
	// bg.wg.Wait()

	// GLH: Just for now...
	if iteration >= 0 {
		return
	}
}

/*
	p := 0
	ppower := 10
	pincrement := uint64(1) << uint64(64-ppower)
	pstart := uint64(0)
	pstop := pstart + (pincrement - 1)
	wg := &sync.WaitGroup{}
	// Here I'm doing bloom filter scans for every partition when eventually it
	// should just do filters for partitions we're in the ring for. Partitions
	// we're not in the ring for (handoffs, old data from ring changes, etc.)
	// we should just send out what data we have and the remove it locally.
	for {
		for i := 0; i < vlm.cores; i++ {
			wg.Add(1)
			go vlm.scan(iteration, p, pstart, pstop, wg)
			if pstop == math.MaxUint64 {
				break
			}
			p++
			pstart += pincrement
			pstop += pincrement
		}
		wg.Wait()
		if pstop == math.MaxUint64 {
			break
		}
	}
	wg.Wait()
}

const _GLH_BLOOM_FILTER_N = 1000000
const _GLH_BLOOM_FILTER_P = 0.001

func (vlm *ValueLocMap) scan(iteration uint16, p int, pstart uint64, pstop uint64, wg *sync.WaitGroup) {
	count := vlm.root.scanCount(vlm, pstart, pstop, 0)
	for count > _GLH_BLOOM_FILTER_N {
		pstartNew := pstart + (pstop-pstart+1)/2
		wg.Add(1)
		go vlm.root.scan(iteration, p, pstart, pstartNew-1, wg)
		pstart = pstartNew
		count = vlm.root.scanCount(vlm, pstart, pstop, 0)
	}
	if count > 0 {
		ktbf := newKTBloomFilter(_GLH_BLOOM_FILTER_N, _GLH_BLOOM_FILTER_P, iteration)
		vlm.root.scanIntoBloomFilter(pstart, pstop, ktbf)
		if ktbf.hasData {
			// Here we'll send the bloom filter to the other replicas and ask
			// them to send us back all their data that isn't in the filter.
			// fmt.Printf("%016x %016x-%016x %s\n", p, pstart, pstop, ktbf)
		}
	}
	wg.Done()
}

func (vln *valueLocNode) scanCount(root *ValueLocMap, pstart uint64, pstop uint64, count uint64) uint64 {
	if vln.rangeStart > pstop {
		return count
	}
	if vln.rangeStop < pstart {
		return count
	}
	c := (*valueLocNode)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&vln.c))))
	if c != nil {
		d := (*valueLocNode)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&vln.d))))
		count = c.scanCount(root, pstart, pstop, count)
		if count > _GLH_BLOOM_FILTER_N {
			return count
		}
		return d.scanCount(root, pstart, pstop, count)
	}
	a := (*valuesLocStore)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&vln.a))))
	b := (*valuesLocStore)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&vln.b))))
	if b == nil {
		if atomic.LoadInt32(&a.used) <= 0 {
			return count
		}
		for bix := len(a.buckets) - 1; bix >= 0; bix-- {
			lix := bix % len(a.locks)
			a.locks[lix].RLock()
			for itemA := &a.buckets[bix]; itemA != nil; itemA = itemA.next {
				if itemA.blockID == 0 {
					continue
				}
				if itemA.keyA < vln.rangeStart || itemA.keyA > vln.rangeStop {
					// Out of place key, extract and reinsert.
					atomic.AddInt32(&vlm.outOfPlaceKeyDetections, 1)
					go root.set(itemA.keyA, itemA.keyB, itemA.timestamp, itemA.blockID, itemA.offset, itemA.length, false)
					itemA.blockID = 0
					continue
				}
				if itemA.keyA < pstart || itemA.keyA > pstop {
					continue
				}
				count++
				if count > _GLH_BLOOM_FILTER_N {
					a.locks[lix].RUnlock()
					return count
				}
			}
			a.locks[lix].RUnlock()
		}
	} else {
		if atomic.LoadInt32(&a.used) <= 0 && atomic.LoadInt32(&b.used) <= 0 {
			return count
		}
		for bix := len(b.buckets) - 1; bix >= 0; bix-- {
			lix := bix % len(b.locks)
			b.locks[lix].RLock()
			for itemB := &b.buckets[bix]; itemB != nil; itemB = itemB.next {
				if itemB.blockID == 0 || itemB.keyA < pstart || itemB.keyA > pstop {
					continue
				}
				count++
				if count > _GLH_BLOOM_FILTER_N {
					b.locks[lix].RUnlock()
					return count
				}
			}
			b.locks[lix].RUnlock()
		}
		for bix := len(a.buckets) - 1; bix >= 0; bix-- {
			lix := bix % len(a.locks)
			b.locks[lix].RLock()
			a.locks[lix].RLock()
		NEXT_ITEM_A:
			for itemA := &a.buckets[bix]; itemA != nil; itemA = itemA.next {
				if itemA.blockID == 0 || itemA.keyA < pstart || itemA.keyA > pstop {
					continue
				}
				for itemB := &a.buckets[bix]; itemB != nil; itemB = itemB.next {
					if itemB.blockID == 0 {
						continue
					}
					if itemB.keyA == itemA.keyA && itemB.keyB == itemA.keyB {
						if itemB.timestamp >= itemA.timestamp {
							count++
							if count > _GLH_BLOOM_FILTER_N {
								a.locks[lix].RUnlock()
								b.locks[lix].RUnlock()
								return count
							}
							continue NEXT_ITEM_A
						}
						break
					}
				}
				count++
				if count > _GLH_BLOOM_FILTER_N {
					a.locks[lix].RUnlock()
					b.locks[lix].RUnlock()
					return count
				}
			}
			a.locks[lix].RUnlock()
			b.locks[lix].RUnlock()
		}
	}
	return count
}

func (vln *valueLocNode) scanIntoBloomFilter(pstart uint64, pstop uint64, ktbf *ktBloomFilter) {
	if vln.rangeStart > pstop {
		return
	}
	if vln.rangeStop < pstart {
		return
	}
	c := (*valueLocNode)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&vln.c))))
	if c != nil {
		d := (*valueLocNode)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&vln.d))))
		c.scanIntoBloomFilter(pstart, pstop, ktbf)
		d.scanIntoBloomFilter(pstart, pstop, ktbf)
		return
	}
	a := (*valuesLocStore)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&vln.a))))
	b := (*valuesLocStore)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&vln.b))))
	if b == nil {
		if atomic.LoadInt32(&a.used) <= 0 {
			return
		}
		for bix := len(a.buckets) - 1; bix >= 0; bix-- {
			lix := bix % len(a.locks)
			a.locks[lix].RLock()
			for itemA := &a.buckets[bix]; itemA != nil; itemA = itemA.next {
				if itemA.blockID == 0 || itemA.keyA < pstart || itemA.keyA > pstop {
					continue
				}
				ktbf.add(itemA.keyA, itemA.keyB, itemA.timestamp)
			}
			a.locks[lix].RUnlock()
		}
	} else {
		if atomic.LoadInt32(&a.used) <= 0 && atomic.LoadInt32(&b.used) <= 0 {
			return
		}
		for bix := len(b.buckets) - 1; bix >= 0; bix-- {
			lix := bix % len(b.locks)
			b.locks[lix].RLock()
			for itemB := &b.buckets[bix]; itemB != nil; itemB = itemB.next {
				if itemB.blockID == 0 || itemB.keyA < pstart || itemB.keyA > pstop {
					continue
				}
				ktbf.add(itemB.keyA, itemB.keyB, itemB.timestamp)
			}
			b.locks[lix].RUnlock()
		}
		for bix := len(a.buckets) - 1; bix >= 0; bix-- {
			lix := bix % len(a.locks)
			b.locks[lix].RLock()
			a.locks[lix].RLock()
		NEXT_ITEM_A:
			for itemA := &a.buckets[bix]; itemA != nil; itemA = itemA.next {
				if itemA.blockID == 0 || itemA.keyA < pstart || itemA.keyA > pstop {
					continue
				}
				for itemB := &a.buckets[bix]; itemB != nil; itemB = itemB.next {
					if itemB.blockID == 0 {
						continue
					}
					if itemB.keyA == itemA.keyA && itemB.keyB == itemA.keyB {
						if itemB.timestamp >= itemA.timestamp {
							ktbf.add(itemB.keyA, itemB.keyB, itemB.timestamp)
							continue NEXT_ITEM_A
						}
						break
					}
				}
				ktbf.add(itemA.keyA, itemA.keyB, itemA.timestamp)
			}
			a.locks[lix].RUnlock()
			b.locks[lix].RUnlock()
		}
	}
}

func (vln *valueLocNode) backgroundHelper(bg *valuesLocMapBackground, vlmPrev *valueLocNode) {
	c := (*valueLocNode)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&vln.c))))
	if c != nil {
		d := (*valueLocNode)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&vln.d))))
		c.backgroundHelper(bg, vln)
		d.backgroundHelper(bg, vln)
		return
	}
	f := func(s *valuesLocStore) {
		var tombstonesDiscarded uint64
		var tombstonesRetained uint64
		for bix := len(s.buckets) - 1; bix >= 0; bix-- {
			lix := bix % len(s.locks)
			s.locks[lix].RLock()
			for item := &s.buckets[bix]; item != nil; item = item.next {
				if item.blockID > 0 && item.timestamp&1 == 1 {
					if item.timestamp < bg.tombstoneCutoff {
						atomic.AddInt32(&s.used, -1)
						item.blockID = 0
						tombstonesDiscarded++
					} else {
						tombstonesRetained++
					}
				}
			}
			s.locks[lix].RUnlock()
		}
		if tombstonesDiscarded > 0 {
			atomic.AddUint64(&bg.tombstonesDiscarded, tombstonesDiscarded)
		}
		if tombstonesRetained > 0 {
			atomic.AddUint64(&bg.tombstonesRetained, tombstonesRetained)
		}
		if atomic.LoadInt32(&s.used) == 0 && vlmPrev != nil {
			vlmPrev.unsplit()
		}
		bg.wg.Done()
	}
	a := (*valuesLocStore)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&vln.a))))
	if a != nil {
		bg.wg.Add(1)
		go f(a)
	}
	b := (*valuesLocStore)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&vln.b))))
	if b != nil {
		bg.wg.Add(1)
		go f(b)
	}
	e := (*valuesLocStore)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&vln.e))))
	if e != nil {
		bg.wg.Add(1)
		go f(e)
	}
}
*/
