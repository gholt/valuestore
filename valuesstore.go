package brimstore

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gholt/brimtext"
	"github.com/gholt/brimutil"
	"github.com/spaolacci/murmur3"
)

var ErrValueNotFound error = errors.New("value not found")

// ValuesStoreOpts allows configuration of the ValuesStore, although normally
// the defaults are best.
type ValuesStoreOpts struct {
	Cores                       int
	MaxValueSize                int
	MemTOCPageSize              int
	MemValuesPageSize           int
	ValuesLocMapPageSize        int
	ValuesLocMapSplitMultiplier float64
	ValuesFileSize              int
	ValuesFileReaders           int
	ChecksumInterval            int
}

func NewValuesStoreOpts(envPrefix string) *ValuesStoreOpts {
	if envPrefix == "" {
		envPrefix = "BRIMSTORE_VALUESSTORE_"
	}
	opts := &ValuesStoreOpts{}
	if env := os.Getenv(envPrefix + "CORES"); env != "" {
		if val, err := strconv.Atoi(env); err == nil {
			opts.Cores = val
		}
	}
	if opts.Cores <= 0 {
		opts.Cores = runtime.GOMAXPROCS(0)
	}
	if env := os.Getenv(envPrefix + "MAX_VALUE_SIZE"); env != "" {
		if val, err := strconv.Atoi(env); err == nil {
			opts.MaxValueSize = val
		}
	}
	if opts.MaxValueSize <= 0 {
		opts.MaxValueSize = 4 * 1024 * 1024
	}
	if env := os.Getenv(envPrefix + "MEM_TOC_PAGE_SIZE"); env != "" {
		if val, err := strconv.Atoi(env); err == nil {
			opts.MemTOCPageSize = val
		}
	}
	if opts.MemTOCPageSize <= 0 {
		opts.MemTOCPageSize = 8 * 1024 * 1024
	}
	if env := os.Getenv(envPrefix + "MEM_VALUES_PAGE_SIZE"); env != "" {
		if val, err := strconv.Atoi(env); err == nil {
			opts.MemValuesPageSize = val
		}
	}
	if opts.MemValuesPageSize <= 0 {
		opts.MemValuesPageSize = 1 << brimutil.PowerOfTwoNeeded(uint64(opts.MaxValueSize+4))
		if opts.MemValuesPageSize < 8*1024*1024 {
			opts.MemValuesPageSize = 8 * 1024 * 1024
		}
	}
	if env := os.Getenv(envPrefix + "VALUES_LOC_MAP_PAGE_SIZE"); env != "" {
		if val, err := strconv.Atoi(env); err == nil {
			opts.ValuesLocMapPageSize = val
		}
	}
	if opts.ValuesLocMapPageSize <= 0 {
		opts.ValuesLocMapPageSize = 8 * 1024 * 1024
	}
	if env := os.Getenv(envPrefix + "VALUES_LOC_MAP_SPLIT_MULTIPLIER"); env != "" {
		if val, err := strconv.ParseFloat(env, 64); err == nil {
			opts.ValuesLocMapSplitMultiplier = val
		}
	}
	if opts.ValuesLocMapSplitMultiplier <= 0 {
		opts.ValuesLocMapSplitMultiplier = 3.0
	}
	if env := os.Getenv(envPrefix + "VALUES_FILE_SIZE"); env != "" {
		if val, err := strconv.Atoi(env); err == nil {
			opts.ValuesFileSize = val
		}
	}
	if opts.ValuesFileSize <= 0 {
		opts.ValuesFileSize = math.MaxUint32
	}
	if env := os.Getenv(envPrefix + "VALUES_FILE_READERS"); env != "" {
		if val, err := strconv.Atoi(env); err == nil {
			opts.ValuesFileReaders = val
		}
	}
	if opts.ValuesFileReaders <= 0 {
		opts.ValuesFileReaders = opts.Cores
	}
	if env := os.Getenv(envPrefix + "CHECKSUM_INTERVAL"); env != "" {
		if val, err := strconv.Atoi(env); err == nil {
			opts.ChecksumInterval = val
		}
	}
	if opts.ChecksumInterval <= 0 {
		opts.ChecksumInterval = 65532
	}
	return opts
}

// ValuesStore: See NewValuesStore.
type ValuesStore struct {
	freeableVMChan        chan *valuesMem
	freeVMChan            chan *valuesMem
	freeVWRChans          []chan *valueWriteReq
	pendingVWRChans       []chan *valueWriteReq
	vfVMChan              chan *valuesMem
	freeTOCBlockChan      chan []byte
	pendingTOCBlockChan   chan []byte
	tocWriterDoneChan     chan struct{}
	valuesLocBlocks       []valuesLocBlock
	atValuesLocBlocksIDer uint32
	vlm                   *valuesLocMap
	cores                 int
	maxValueSize          uint32
	memTOCPageSize        uint32
	memValuesPageSize     uint32
	valuesFileSize        uint32
	valuesFileReaders     int
	checksumInterval      uint32
}

// NewValuesStore creates a ValuesStore for use in storing []byte values
// referenced by 128 bit keys; opts may be nil to use the defaults.
//
// Note that a lot of buffering and multiple cores can be in use and therefore
// Close should be called prior to the process exiting to ensure all processing
// is done and the buffers are flushed.
func NewValuesStore(opts *ValuesStoreOpts) *ValuesStore {
	if opts == nil {
		opts = NewValuesStoreOpts("")
	}
	cores := opts.Cores
	if cores < 1 {
		cores = 1
	}
	maxValueSize := opts.MaxValueSize
	if maxValueSize < 0 {
		maxValueSize = 0
	}
	if maxValueSize > math.MaxUint32 {
		maxValueSize = math.MaxUint32
	}
	memTOCPageSize := opts.MemTOCPageSize
	if memTOCPageSize < 4096 {
		memTOCPageSize = 4096
	}
	memValuesPageSize := opts.MemValuesPageSize
	if memValuesPageSize < 4096 {
		memValuesPageSize = 4096
	}
	valuesFileSize := opts.ValuesFileSize
	if valuesFileSize <= 0 || valuesFileSize > math.MaxUint32 {
		valuesFileSize = math.MaxUint32
	}
	valuesFileReaders := opts.ValuesFileReaders
	if valuesFileReaders < 1 {
		valuesFileReaders = 1
	}
	checksumInterval := opts.ChecksumInterval
	if checksumInterval < 1024 {
		checksumInterval = 1024
	} else if checksumInterval >= 4294967296 {
		checksumInterval = 4294967295
	}
	if memTOCPageSize < checksumInterval/2+1 {
		memTOCPageSize = checksumInterval/2 + 1
	}
	if memTOCPageSize > math.MaxUint32 {
		memTOCPageSize = math.MaxUint32
	}
	if memValuesPageSize < checksumInterval/2+1 {
		memValuesPageSize = checksumInterval/2 + 1
	}
	if memValuesPageSize > math.MaxUint32 {
		memValuesPageSize = math.MaxUint32
	}
	vs := &ValuesStore{
		valuesLocBlocks:       make([]valuesLocBlock, 65536),
		atValuesLocBlocksIDer: _VALUESBLOCK_IDOFFSET - 1,
		vlm:               newValuesLocMap(opts),
		cores:             cores,
		maxValueSize:      uint32(maxValueSize),
		memTOCPageSize:    uint32(memTOCPageSize),
		memValuesPageSize: uint32(memValuesPageSize),
		valuesFileSize:    uint32(valuesFileSize),
		checksumInterval:  uint32(checksumInterval),
		valuesFileReaders: valuesFileReaders,
	}
	vs.freeableVMChan = make(chan *valuesMem, vs.cores)
	vs.freeVMChan = make(chan *valuesMem, vs.cores*2)
	vs.freeVWRChans = make([]chan *valueWriteReq, vs.cores)
	vs.pendingVWRChans = make([]chan *valueWriteReq, vs.cores)
	vs.vfVMChan = make(chan *valuesMem, vs.cores)
	vs.freeTOCBlockChan = make(chan []byte, vs.cores*2)
	vs.pendingTOCBlockChan = make(chan []byte, vs.cores)
	vs.tocWriterDoneChan = make(chan struct{}, 1)
	for i := 0; i < cap(vs.freeVMChan); i++ {
		vm := &valuesMem{
			vs:     vs,
			toc:    make([]byte, 0, vs.memTOCPageSize),
			values: make([]byte, 0, vs.memValuesPageSize),
		}
		vm.id = vs.addValuesLocBock(vm)
		vs.freeVMChan <- vm
	}
	for i := 0; i < len(vs.freeVWRChans); i++ {
		vs.freeVWRChans[i] = make(chan *valueWriteReq, vs.cores*2)
		for j := 0; j < vs.cores*2; j++ {
			vs.freeVWRChans[i] <- &valueWriteReq{errChan: make(chan error, 1)}
		}
	}
	for i := 0; i < len(vs.pendingVWRChans); i++ {
		vs.pendingVWRChans[i] = make(chan *valueWriteReq)
	}
	for i := 0; i < cap(vs.freeTOCBlockChan); i++ {
		vs.freeTOCBlockChan <- make([]byte, 0, vs.memTOCPageSize)
	}
	go vs.tocWriter()
	go vs.vfWriter()
	for i := 0; i < vs.cores; i++ {
		go vs.memClearer()
	}
	for i := 0; i < len(vs.pendingVWRChans); i++ {
		go vs.memWriter(vs.pendingVWRChans[i])
	}
	vs.recovery()
	return vs
}

func (vs *ValuesStore) MaxValueSize() uint32 {
	return vs.maxValueSize
}

func (vs *ValuesStore) Close() {
	for _, c := range vs.pendingVWRChans {
		c <- nil
	}
	<-vs.tocWriterDoneChan
	for vs.vlm.isResizing() {
		time.Sleep(10 * time.Millisecond)
	}
}

// Lookup will return seq, length, err for keyA, keyB.
func (vs *ValuesStore) Lookup(keyA uint64, keyB uint64) (uint64, uint32, error) {
	seq, id, _, length := vs.vlm.get(keyA, keyB)
	if id < _VALUESBLOCK_IDOFFSET {
		return 0, 0, ErrValueNotFound
	}
	return seq, length, nil
}

// Read will return seq, value, err for keyA, keyB; if an incoming value
// is provided, the read value will be appended to it and the whole returned
// (useful to reuse an existing []byte).
func (vs *ValuesStore) Read(keyA uint64, keyB uint64, value []byte) (uint64, []byte, error) {
	seq, id, offset, length := vs.vlm.get(keyA, keyB)
	if id < _VALUESBLOCK_IDOFFSET {
		return 0, value, ErrValueNotFound
	}
	return vs.valuesLocBlock(id).read(keyA, keyB, seq, offset, length, value)
}

// Write stores seq, value for keyA, keyB or returns any error; a newer
// seq already in place is not reported as an error.
func (vs *ValuesStore) Write(keyA uint64, keyB uint64, seq uint64, value []byte) (uint64, error) {
	i := int(keyA>>1) % len(vs.freeVWRChans)
	vwr := <-vs.freeVWRChans[i]
	vwr.keyA = keyA
	vwr.keyB = keyB
	vwr.seq = seq
	vwr.value = value
	vs.pendingVWRChans[i] <- vwr
	err := <-vwr.errChan
	oldSeq := vwr.seq
	vwr.value = nil
	vs.freeVWRChans[i] <- vwr
	return oldSeq, err
}

func (vs *ValuesStore) GatherStats(extended bool) *ValuesStoreStats {
	stats := &ValuesStoreStats{}
	if extended {
		stats.extended = extended
		stats.freeableVMChanCap = cap(vs.freeableVMChan)
		stats.freeableVMChanIn = len(vs.freeableVMChan)
		stats.freeVMChanCap = cap(vs.freeVMChan)
		stats.freeVMChanIn = len(vs.freeVMChan)
		stats.freeVWRChans = len(vs.freeVWRChans)
		for i := 0; i < len(vs.freeVWRChans); i++ {
			stats.freeVWRChansCap += cap(vs.freeVWRChans[i])
			stats.freeVWRChansIn += len(vs.freeVWRChans[i])
		}
		stats.pendingVWRChans = len(vs.pendingVWRChans)
		for i := 0; i < len(vs.pendingVWRChans); i++ {
			stats.pendingVWRChansCap += cap(vs.pendingVWRChans[i])
			stats.pendingVWRChansIn += len(vs.pendingVWRChans[i])
		}
		stats.vfVMChanCap = cap(vs.vfVMChan)
		stats.vfVMChanIn = len(vs.vfVMChan)
		stats.freeTOCBlockChanCap = cap(vs.freeTOCBlockChan)
		stats.freeTOCBlockChanIn = len(vs.freeTOCBlockChan)
		stats.pendingTOCBlockChanCap = cap(vs.pendingTOCBlockChan)
		stats.pendingTOCBlockChanIn = len(vs.pendingTOCBlockChan)
		stats.maxValuesLocBlockID = atomic.LoadUint32(&vs.atValuesLocBlocksIDer)
		stats.cores = vs.cores
		stats.maxValueSize = vs.maxValueSize
		stats.memTOCPageSize = vs.memTOCPageSize
		stats.memValuesPageSize = vs.memValuesPageSize
		stats.valuesFileSize = vs.valuesFileSize
		stats.valuesFileReaders = vs.valuesFileReaders
		stats.checksumInterval = vs.checksumInterval
		stats.vlmStats = vs.vlm.gatherStats(true)
	} else {
		stats.vlmStats = vs.vlm.gatherStats(false)
	}
	return stats
}

func (vs *ValuesStore) valuesLocBlock(valuesLocBlockID uint16) valuesLocBlock {
	return vs.valuesLocBlocks[valuesLocBlockID]
}

func (vs *ValuesStore) addValuesLocBock(block valuesLocBlock) uint16 {
	id := atomic.AddUint32(&vs.atValuesLocBlocksIDer, 1)
	if id >= 65536 {
		panic("too many valuesLocBlocks")
	}
	vs.valuesLocBlocks[id] = block
	return uint16(id)
}

func (vs *ValuesStore) memClearer() {
	var tb []byte
	var tbTS int64
	var tbOffset int
	for {
		vm := <-vs.freeableVMChan
		if vm == nil {
			if tb != nil {
				vs.pendingTOCBlockChan <- tb
			}
			vs.pendingTOCBlockChan <- nil
			break
		}
		vf := vs.valuesLocBlock(vm.vfID)
		if tb != nil && tbTS != vf.timestamp() {
			vs.pendingTOCBlockChan <- tb
			tb = nil
		}
		for vmTOCOffset := 0; vmTOCOffset < len(vm.toc); vmTOCOffset += 32 {
			a := binary.BigEndian.Uint64(vm.toc[vmTOCOffset:])
			b := binary.BigEndian.Uint64(vm.toc[vmTOCOffset+8:])
			q := binary.BigEndian.Uint64(vm.toc[vmTOCOffset+16:])
			vmMemOffset := binary.BigEndian.Uint32(vm.toc[vmTOCOffset+24:])
			z := binary.BigEndian.Uint32(vm.toc[vmTOCOffset+28:])
			oldSeq := vs.vlm.set(a, b, q, vm.vfID, vm.vfOffset+vmMemOffset, z, true)
			if oldSeq != q {
				continue
			}
			if tb != nil && tbOffset+32 > cap(tb) {
				vs.pendingTOCBlockChan <- tb
				tb = nil
			}
			if tb == nil {
				tb = <-vs.freeTOCBlockChan
				tbTS = vf.timestamp()
				tb = tb[:8]
				binary.BigEndian.PutUint64(tb, uint64(tbTS))
				tbOffset = 8
			}
			tb = tb[:tbOffset+32]
			binary.BigEndian.PutUint64(tb[tbOffset:], a)
			binary.BigEndian.PutUint64(tb[tbOffset+8:], b)
			binary.BigEndian.PutUint64(tb[tbOffset+16:], q)
			binary.BigEndian.PutUint32(tb[tbOffset+24:], vm.vfOffset+uint32(vmMemOffset))
			binary.BigEndian.PutUint32(tb[tbOffset+28:], z)
			tbOffset += 32
		}
		vm.discardLock.Lock()
		vm.vfID = 0
		vm.vfOffset = 0
		vm.toc = vm.toc[:0]
		vm.values = vm.values[:0]
		vm.discardLock.Unlock()
		vs.freeVMChan <- vm
	}
}

func (vs *ValuesStore) memWriter(VWRChan chan *valueWriteReq) {
	var vm *valuesMem
	var vmTOCOffset int
	var vmMemOffset int
	for {
		vwr := <-VWRChan
		if vwr == nil {
			if vm != nil && len(vm.toc) > 0 {
				vs.vfVMChan <- vm
			}
			vs.vfVMChan <- nil
			break
		}
		z := len(vwr.value)
		if z > int(vs.maxValueSize) {
			vwr.errChan <- fmt.Errorf("value length of %d > %d", z, vs.maxValueSize)
			continue
		}
		if vm != nil && (vmTOCOffset+32 > cap(vm.toc) || vmMemOffset+z > cap(vm.values)) {
			vs.vfVMChan <- vm
			vm = nil
		}
		if vm == nil {
			vm = <-vs.freeVMChan
			vmTOCOffset = 0
			vmMemOffset = 0
		}
		vm.discardLock.Lock()
		vm.values = vm.values[:vmMemOffset+z]
		vm.discardLock.Unlock()
		copy(vm.values[vmMemOffset:], vwr.value)
		oldSeq := vs.vlm.set(vwr.keyA, vwr.keyB, vwr.seq, vm.id, uint32(vmMemOffset), uint32(z), false)
		if oldSeq < vwr.seq {
			vm.toc = vm.toc[:vmTOCOffset+32]
			binary.BigEndian.PutUint64(vm.toc[vmTOCOffset:], vwr.keyA)
			binary.BigEndian.PutUint64(vm.toc[vmTOCOffset+8:], vwr.keyB)
			binary.BigEndian.PutUint64(vm.toc[vmTOCOffset+16:], vwr.seq)
			binary.BigEndian.PutUint32(vm.toc[vmTOCOffset+24:], uint32(vmMemOffset))
			binary.BigEndian.PutUint32(vm.toc[vmTOCOffset+28:], uint32(z))
			vmTOCOffset += 32
			vmMemOffset += z
		} else {
			vm.discardLock.Lock()
			vm.values = vm.values[:vmMemOffset]
			vm.discardLock.Unlock()
		}
		vwr.seq = oldSeq
		vwr.errChan <- nil
	}
}

func (vs *ValuesStore) vfWriter() {
	var vf *valuesFile
	memWritersLeft := vs.cores
	var tocLen uint64
	var valuesLen uint64
	for {
		vm := <-vs.vfVMChan
		if vm == nil {
			memWritersLeft--
			if memWritersLeft < 1 {
				if vf != nil {
					vf.close()
				}
				for i := 0; i < vs.cores; i++ {
					vs.freeableVMChan <- nil
				}
				break
			}
			continue
		}
		if vf != nil && (tocLen+uint64(len(vm.toc)) >= uint64(vs.valuesFileSize) || valuesLen+uint64(len(vm.values)) > uint64(vs.valuesFileSize)) {
			vf.close()
			vf = nil
		}
		if vf == nil {
			vf = createValuesFile(vs)
			tocLen = 32
			valuesLen = 32
		}
		vf.write(vm)
		tocLen += uint64(len(vm.toc))
		valuesLen += uint64(len(vm.values))
	}
}

func (vs *ValuesStore) tocWriter() {
	var tsA uint64
	var writerA io.WriteCloser
	var offsetA uint64
	var tsB uint64
	var writerB io.WriteCloser
	var offsetB uint64
	head := []byte("BRIMSTORE VALUESTOC v0          ")
	binary.BigEndian.PutUint32(head[28:], uint32(vs.checksumInterval))
	term := make([]byte, 16)
	copy(term[12:], "TERM")
	memClearersLeft := vs.cores
	for {
		t := <-vs.pendingTOCBlockChan
		if t == nil {
			memClearersLeft--
			if memClearersLeft < 1 {
				if writerB != nil {
					binary.BigEndian.PutUint64(term[4:], offsetB)
					if _, err := writerB.Write(term); err != nil {
						panic(err)
					}
					if err := writerB.Close(); err != nil {
						panic(err)
					}
				}
				if writerA != nil {
					binary.BigEndian.PutUint64(term[4:], offsetA)
					if _, err := writerA.Write(term); err != nil {
						panic(err)
					}
					if err := writerA.Close(); err != nil {
						panic(err)
					}
				}
				break
			}
			continue
		}
		if len(t) > 8 {
			ts := binary.BigEndian.Uint64(t)
			switch ts {
			case tsA:
				if _, err := writerA.Write(t[8:]); err != nil {
					panic(err)
				}
				offsetA += uint64(len(t) - 8)
			case tsB:
				if _, err := writerB.Write(t[8:]); err != nil {
					panic(err)
				}
				offsetB += uint64(len(t) - 8)
			default:
				// An assumption is made here: If the timestamp for this toc
				// block doesn't match the last two seen timestamps then we
				// expect no more toc blocks for the oldest timestamp and can
				// close that toc file.
				if writerB != nil {
					binary.BigEndian.PutUint64(term[4:], offsetB)
					if _, err := writerB.Write(term); err != nil {
						panic(err)
					}
					if err := writerB.Close(); err != nil {
						panic(err)
					}
				}
				tsB = tsA
				writerB = writerA
				offsetB = offsetA
				tsA = ts
				fp, err := os.Create(fmt.Sprintf("%d.valuestoc", ts))
				if err != nil {
					panic(err)
				}
				writerA = brimutil.NewMultiCoreChecksummedWriter(fp, int(vs.checksumInterval), murmur3.New32, vs.cores)
				if _, err := writerA.Write(head); err != nil {
					panic(err)
				}
				if _, err := writerA.Write(t[8:]); err != nil {
					panic(err)
				}
				offsetA = 32 + uint64(len(t)-8)
			}
		}
		vs.freeTOCBlockChan <- t[:0]
	}
	vs.tocWriterDoneChan <- struct{}{}
}

func (vs *ValuesStore) recovery() {
	start := time.Now()
	dfp, err := os.Open(".")
	if err != nil {
		panic(err)
	}
	names, err := dfp.Readdirnames(-1)
	if err != nil {
		panic(err)
	}
	sort.Strings(names)
	fromDiskCount := 0
	count := int64(0)
	type writeReq struct {
		keyA    uint64
		keyB    uint64
		seq     uint64
		blockID uint16
		offset  uint32
		length  uint32
	}
	pendingChan := make(chan []writeReq, vs.cores)
	freeChan := make(chan []writeReq, cap(pendingChan)*2)
	for i := 0; i < cap(freeChan); i++ {
		freeChan <- make([]writeReq, 0, 65536)
	}
	wg := &sync.WaitGroup{}
	wg.Add(cap(pendingChan))
	for i := 0; i < cap(pendingChan); i++ {
		go func() {
			for {
				wrs := <-pendingChan
				if wrs == nil {
					break
				}
				for i := len(wrs) - 1; i >= 0; i-- {
					wr := &wrs[i]
					if vs.vlm.set(wr.keyA, wr.keyB, wr.seq, wr.blockID, wr.offset, wr.length, false) < wr.seq {
						atomic.AddInt64(&count, 1)
					}
				}
				freeChan <- wrs[:0]
			}
			wg.Done()
		}()
	}
	wrs := <-freeChan
	wix := 0
	maxwix := cap(wrs) - 1
	wrs = wrs[:maxwix+1]
	for i := len(names) - 1; i >= 0; i-- {
		if !strings.HasSuffix(names[i], ".valuestoc") {
			continue
		}
		ts := int64(0)
		if ts, err = strconv.ParseInt(names[i][:len(names[i])-len(".valuestoc")], 10, 64); err != nil {
			log.Printf("bad timestamp name: %#v\n", names[i])
			continue
		}
		if ts == 0 {
			log.Printf("bad timestamp name: %#v\n", names[i])
			continue
		}
		vf := newValuesFile(vs, ts)
		fp, err := os.Open(names[i])
		if err != nil {
			log.Printf("error opening %s: %s\n", names[i], err)
			continue
		}
		buf := make([]byte, vs.checksumInterval+4)
		checksumFailures := 0
		overflow := make([]byte, 0, 32)
		first := true
		terminated := false
		for {
			n, err := io.ReadFull(fp, buf)
			if n < 4 {
				if err != io.EOF && err != io.ErrUnexpectedEOF {
					log.Printf("error reading %s: %s\n", names[i], err)
				}
				break
			}
			n -= 4
			if murmur3.Sum32(buf[:n]) != binary.BigEndian.Uint32(buf[n:]) {
				checksumFailures++
			} else {
				i := 0
				if first {
					if !bytes.Equal(buf[:28], []byte("BRIMSTORE VALUESTOC v0      ")) {
						log.Printf("bad header: %s\n", names[i])
						break
					}
					if binary.BigEndian.Uint32(buf[28:]) != vs.checksumInterval {
						log.Printf("bad header checksum interval: %s\n", names[i])
						break
					}
					i += 32
					first = false
				}
				if n < int(vs.checksumInterval) {
					if binary.BigEndian.Uint32(buf[n-16:]) != 0 {
						log.Printf("bad terminator size marker: %s\n", names[i])
						break
					}
					if !bytes.Equal(buf[n-4:n], []byte("TERM")) {
						log.Printf("bad terminator: %s\n", names[i])
						break
					}
					n -= 16
					terminated = true
				}
				if len(overflow) > 0 {
					i += 32 - len(overflow)
					overflow = append(overflow, buf[i-32+len(overflow):i]...)
					if wrs == nil {
						wrs = (<-freeChan)[:maxwix+1]
						wix = 0
					}
					wr := &wrs[wix]
					wr.keyA = binary.BigEndian.Uint64(overflow)
					wr.keyB = binary.BigEndian.Uint64(overflow[8:])
					wr.seq = binary.BigEndian.Uint64(overflow[16:])
					wr.blockID = vf.id
					wr.offset = binary.BigEndian.Uint32(overflow[24:])
					wr.length = binary.BigEndian.Uint32(overflow[28:])
					wix++
					if wix > maxwix {
						pendingChan <- wrs
						wrs = nil
					}
					fromDiskCount++
					overflow = overflow[:0]
				}
				for ; i+32 <= n; i += 32 {
					if wrs == nil {
						wrs = (<-freeChan)[:maxwix+1]
						wix = 0
					}
					wr := &wrs[wix]
					wr.keyA = binary.BigEndian.Uint64(buf[i:])
					wr.keyB = binary.BigEndian.Uint64(buf[i+8:])
					wr.seq = binary.BigEndian.Uint64(buf[i+16:])
					wr.blockID = vf.id
					wr.offset = binary.BigEndian.Uint32(buf[i+24:])
					wr.length = binary.BigEndian.Uint32(buf[i+28:])
					wix++
					if wix > maxwix {
						pendingChan <- wrs
						wrs = nil
					}
					fromDiskCount++
				}
				if i != n {
					overflow = overflow[:n-i]
					copy(overflow, buf[i:])
				}
			}
			if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
				log.Printf("error reading %s: %s\n", names[i], err)
				break
			}
		}
		fp.Close()
		if !terminated {
			log.Printf("early end of file: %s\n", names[i])
		}
		if checksumFailures > 0 {
			log.Printf("%d checksum failures for %s\n", checksumFailures, names[i])
		}
	}
	if wix > 0 {
		pendingChan <- wrs[:wix]
	}
	for i := 0; i < cap(pendingChan); i++ {
		pendingChan <- nil
	}
	wg.Wait()
	if fromDiskCount > 0 {
		dur := time.Now().Sub(start)
		log.Printf("%d key locations loaded in %s, %.0f/s; %d caused change; %d resulting locations.\n", fromDiskCount, dur, float64(fromDiskCount)/(float64(dur)/float64(time.Second)), count, vs.GatherStats(false).ValueCount())
	}
}

type valueWriteReq struct {
	keyA    uint64
	keyB    uint64
	seq     uint64
	value   []byte
	errChan chan error
}

type valuesLocBlock interface {
	timestamp() int64
	read(keyA uint64, keyB uint64, seq uint64, offset uint32, length uint32, value []byte) (uint64, []byte, error)
}

type ValuesStoreStats struct {
	extended               bool
	freeableVMChanCap      int
	freeableVMChanIn       int
	freeVMChanCap          int
	freeVMChanIn           int
	freeVWRChans           int
	freeVWRChansCap        int
	freeVWRChansIn         int
	pendingVWRChans        int
	pendingVWRChansCap     int
	pendingVWRChansIn      int
	vfVMChanCap            int
	vfVMChanIn             int
	freeTOCBlockChanCap    int
	freeTOCBlockChanIn     int
	pendingTOCBlockChanCap int
	pendingTOCBlockChanIn  int
	maxValuesLocBlockID    uint32
	cores                  int
	maxValueSize           uint32
	memTOCPageSize         uint32
	memValuesPageSize      uint32
	valuesFileSize         uint32
	valuesFileReaders      int
	checksumInterval       uint32
	vlmStats               *valuesLocMapStats
}

func (stats *ValuesStoreStats) String() string {
	if stats.extended {
		return brimtext.Align([][]string{
			[]string{"freeableVMChanCap", fmt.Sprintf("%d", stats.freeableVMChanCap)},
			[]string{"freeableVMChanIn", fmt.Sprintf("%d", stats.freeableVMChanIn)},
			[]string{"freeVMChanCap", fmt.Sprintf("%d", stats.freeVMChanCap)},
			[]string{"freeVMChanIn", fmt.Sprintf("%d", stats.freeVMChanIn)},
			[]string{"freeVWRChans", fmt.Sprintf("%d", stats.freeVWRChans)},
			[]string{"freeVWRChansCap", fmt.Sprintf("%d", stats.freeVWRChansCap)},
			[]string{"freeVWRChansIn", fmt.Sprintf("%d", stats.freeVWRChansIn)},
			[]string{"pendingVWRChans", fmt.Sprintf("%d", stats.pendingVWRChans)},
			[]string{"pendingVWRChansCap", fmt.Sprintf("%d", stats.pendingVWRChansCap)},
			[]string{"pendingVWRChansIn", fmt.Sprintf("%d", stats.pendingVWRChansIn)},
			[]string{"vfVMChanCap", fmt.Sprintf("%d", stats.vfVMChanCap)},
			[]string{"vfVMChanIn", fmt.Sprintf("%d", stats.vfVMChanIn)},
			[]string{"freeTOCBlockChanCap", fmt.Sprintf("%d", stats.freeTOCBlockChanCap)},
			[]string{"freeTOCBlockChanIn", fmt.Sprintf("%d", stats.freeTOCBlockChanIn)},
			[]string{"pendingTOCBlockChanCap", fmt.Sprintf("%d", stats.pendingTOCBlockChanCap)},
			[]string{"pendingTOCBlockChanIn", fmt.Sprintf("%d", stats.pendingTOCBlockChanIn)},
			[]string{"maxValuesLocBlockID", fmt.Sprintf("%d", stats.maxValuesLocBlockID)},
			[]string{"cores", fmt.Sprintf("%d", stats.cores)},
			[]string{"maxValueSize", fmt.Sprintf("%d", stats.maxValueSize)},
			[]string{"memTOCPageSize", fmt.Sprintf("%d", stats.memTOCPageSize)},
			[]string{"memValuesPageSize", fmt.Sprintf("%d", stats.memValuesPageSize)},
			[]string{"valuesFileSize", fmt.Sprintf("%d", stats.valuesFileSize)},
			[]string{"valuesFileReaders", fmt.Sprintf("%d", stats.valuesFileReaders)},
			[]string{"checksumInterval", fmt.Sprintf("%d", stats.checksumInterval)},
			[]string{"vlm", stats.vlmStats.String()},
		}, nil)
	} else {
		return brimtext.Align([][]string{
			[]string{"vlm", stats.vlmStats.String()},
		}, nil)
	}
}

func (stats *ValuesStoreStats) ValueCount() uint64 {
	return stats.vlmStats.used
}

func (stats *ValuesStoreStats) ValuesLength() uint64 {
	return stats.vlmStats.length
}
