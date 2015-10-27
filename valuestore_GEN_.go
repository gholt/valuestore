package valuestore

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gholt/ring"
	"github.com/gholt/valuelocmap"
	"github.com/spaolacci/murmur3"
	"gopkg.in/gholt/brimutil.v1"
)

// DefaultValueStore instances are created with NewValueStore.
type DefaultValueStore struct {
	logCritical             LogFunc
	logError                LogFunc
	logWarning              LogFunc
	logInfo                 LogFunc
	logDebug                LogFunc
	randMutex               sync.Mutex
	rand                    *rand.Rand
	freeableMemBlockChans   []chan *valueMemBlock
	freeMemBlockChan        chan *valueMemBlock
	freeWriteReqChans       []chan *valueWriteReq
	pendingWriteReqChans    []chan *valueWriteReq
	fileMemBlockChan        chan *valueMemBlock
	freeTOCBlockChan        chan []byte
	pendingTOCBlockChan     chan []byte
	activeTOCA              uint64
	activeTOCB              uint64
	flushedChan             chan struct{}
	locBlocks               []valueLocBlock
	locBlockIDer            uint64
	path                    string
	pathtoc                 string
	locmap                  valuelocmap.ValueLocMap
	workers                 int
	recoveryBatchSize       int
	valueCap                uint32
	pageSize                uint32
	minValueAlloc           int
	writePagesPerWorker     int
	fileCap                 uint32
	fileReaders             int
	checksumInterval        uint32
	msgRing                 ring.MsgRing
	tombstoneDiscardState   valueTombstoneDiscardState
	replicationIgnoreRecent uint64
	pullReplicationState    valuePullReplicationState
	pushReplicationState    valuePushReplicationState
	compactionState         valueCompactionState
	bulkSetState            valueBulkSetState
	bulkSetAckState         valueBulkSetAckState
	disableEnableWritesLock sync.Mutex
	userDisabled            bool
	diskWatcherState        valueDiskWatcherState

	statsLock                    sync.Mutex
	lookups                      int32
	lookupErrors                 int32
	lookupGroups                 int32
	lookupGroupItems             int32
	reads                        int32
	readErrors                   int32
	readGroups                   int32
	readGroupItems               int32
	writes                       int32
	writeErrors                  int32
	writesOverridden             int32
	deletes                      int32
	deleteErrors                 int32
	deletesOverridden            int32
	outBulkSets                  int32
	outBulkSetValues             int32
	outBulkSetPushes             int32
	outBulkSetPushValues         int32
	inBulkSets                   int32
	inBulkSetDrops               int32
	inBulkSetInvalids            int32
	inBulkSetWrites              int32
	inBulkSetWriteErrors         int32
	inBulkSetWritesOverridden    int32
	outBulkSetAcks               int32
	inBulkSetAcks                int32
	inBulkSetAckDrops            int32
	inBulkSetAckInvalids         int32
	inBulkSetAckWrites           int32
	inBulkSetAckWriteErrors      int32
	inBulkSetAckWritesOverridden int32
	outPullReplications          int32
	inPullReplications           int32
	inPullReplicationDrops       int32
	inPullReplicationInvalids    int32
	expiredDeletions             int32
	compactions                  int32
	smallFileCompactions         int32
}

type valueWriteReq struct {
	keyA uint64
	keyB uint64

	timestampbits uint64
	value         []byte
	errChan       chan error
	internal      bool
}

var enableValueWriteReq *valueWriteReq = &valueWriteReq{}
var disableValueWriteReq *valueWriteReq = &valueWriteReq{}
var flushValueWriteReq *valueWriteReq = &valueWriteReq{}
var flushValueMemBlock *valueMemBlock = &valueMemBlock{}

type valueLocBlock interface {
	timestampnano() int64
	read(keyA uint64, keyB uint64, timestampbits uint64, offset uint32, length uint32, value []byte) (uint64, []byte, error)
	close() error
}

// NewValueStore creates a DefaultValueStore for use in storing []byte values
// referenced by 128 bit keys.
//
// Note that a lot of buffering, multiple cores, and background processes can
// be in use and therefore DisableAll() and Flush() should be called prior to
// the process exiting to ensure all processing is done and the buffers are
// flushed.
func NewValueStore(c *ValueStoreConfig) (*DefaultValueStore, error) {
	cfg := resolveValueStoreConfig(c)
	locmap := cfg.ValueLocMap
	if locmap == nil {
		locmap = valuelocmap.NewValueLocMap(nil)
	}
	locmap.SetInactiveMask(_TSB_INACTIVE)
	store := &DefaultValueStore{
		logCritical:             cfg.LogCritical,
		logError:                cfg.LogError,
		logWarning:              cfg.LogWarning,
		logInfo:                 cfg.LogInfo,
		logDebug:                cfg.LogDebug,
		rand:                    cfg.Rand,
		locBlocks:               make([]valueLocBlock, math.MaxUint16),
		path:                    cfg.Path,
		pathtoc:                 cfg.PathTOC,
		locmap:                  locmap,
		workers:                 cfg.Workers,
		recoveryBatchSize:       cfg.RecoveryBatchSize,
		replicationIgnoreRecent: (uint64(cfg.ReplicationIgnoreRecent) * uint64(time.Second) / 1000) << _TSB_UTIL_BITS,
		valueCap:                uint32(cfg.ValueCap),
		pageSize:                uint32(cfg.PageSize),
		minValueAlloc:           cfg.minValueAlloc,
		writePagesPerWorker:     cfg.WritePagesPerWorker,
		fileCap:                 uint32(cfg.FileCap),
		fileReaders:             cfg.FileReaders,
		checksumInterval:        uint32(cfg.ChecksumInterval),
		msgRing:                 cfg.MsgRing,
	}
	store.freeableMemBlockChans = make([]chan *valueMemBlock, store.workers)
	for i := 0; i < cap(store.freeableMemBlockChans); i++ {
		store.freeableMemBlockChans[i] = make(chan *valueMemBlock, store.workers)
	}
	store.freeMemBlockChan = make(chan *valueMemBlock, store.workers*store.writePagesPerWorker)
	store.freeWriteReqChans = make([]chan *valueWriteReq, store.workers)
	store.pendingWriteReqChans = make([]chan *valueWriteReq, store.workers)
	store.fileMemBlockChan = make(chan *valueMemBlock, store.workers)
	store.freeTOCBlockChan = make(chan []byte, store.workers*2)
	store.pendingTOCBlockChan = make(chan []byte, store.workers)
	store.flushedChan = make(chan struct{}, 1)
	for i := 0; i < cap(store.freeMemBlockChan); i++ {
		memBlock := &valueMemBlock{
			store:  store,
			toc:    make([]byte, 0, store.pageSize),
			values: make([]byte, 0, store.pageSize),
		}
		var err error
		memBlock.id, err = store.addLocBlock(memBlock)
		if err != nil {
			return nil, err
		}
		store.freeMemBlockChan <- memBlock
	}
	for i := 0; i < len(store.freeWriteReqChans); i++ {
		store.freeWriteReqChans[i] = make(chan *valueWriteReq, store.workers*2)
		for j := 0; j < store.workers*2; j++ {
			store.freeWriteReqChans[i] <- &valueWriteReq{errChan: make(chan error, 1)}
		}
	}
	for i := 0; i < len(store.pendingWriteReqChans); i++ {
		store.pendingWriteReqChans[i] = make(chan *valueWriteReq)
	}
	for i := 0; i < cap(store.freeTOCBlockChan); i++ {
		store.freeTOCBlockChan <- make([]byte, 0, store.pageSize)
	}
	go store.tocWriter()
	go store.fileWriter()
	for i := 0; i < len(store.freeableMemBlockChans); i++ {
		go store.memClearer(store.freeableMemBlockChans[i])
	}
	for i := 0; i < len(store.pendingWriteReqChans); i++ {
		go store.memWriter(store.pendingWriteReqChans[i])
	}
	err := store.recovery()
	if err != nil {
		return nil, err
	}
	store.tombstoneDiscardConfig(cfg)
	store.compactionConfig(cfg)
	store.pullReplicationConfig(cfg)
	store.pushReplicationConfig(cfg)
	store.bulkSetConfig(cfg)
	store.bulkSetAckConfig(cfg)
	store.diskWatcherConfig(cfg)
	store.tombstoneDiscardLaunch()
	store.compactionLaunch()
	store.pullReplicationLaunch()
	store.pushReplicationLaunch()
	store.bulkSetLaunch()
	store.bulkSetAckLaunch()
	store.diskWatcherLaunch()
	return store, nil
}

// ValueCap returns the maximum length of a value the ValueStore can accept.
func (store *DefaultValueStore) ValueCap() uint32 {
	return store.valueCap
}

// DisableAll calls DisableAllBackground(), and DisableWrites().
func (store *DefaultValueStore) DisableAll() {
	store.DisableAllBackground()
	store.DisableWrites()
}

// DisableAllBackground calls DisableTombstoneDiscard(), DisableCompaction(),
// DisableOutPullReplication(), DisableOutPushReplication(), but does *not*
// call DisableWrites().
func (store *DefaultValueStore) DisableAllBackground() {
	store.DisableTombstoneDiscard()
	store.DisableCompaction()
	store.DisableOutPullReplication()
	store.DisableOutPushReplication()
}

// EnableAll calls EnableTombstoneDiscard(), EnableCompaction(),
// EnableOutPullReplication(), EnableOutPushReplication(), and EnableWrites().
func (store *DefaultValueStore) EnableAll() {
	store.EnableTombstoneDiscard()
	store.EnableOutPullReplication()
	store.EnableOutPushReplication()
	store.EnableWrites()
	store.EnableCompaction()
}

// DisableWrites will cause any incoming Write or Delete requests to respond
// with ErrDisabled until EnableWrites is called.
func (store *DefaultValueStore) DisableWrites() {
	store.disableWrites(true)
}

func (store *DefaultValueStore) disableWrites(userCall bool) {
	store.disableEnableWritesLock.Lock()
	if userCall {
		store.userDisabled = true
	}
	for _, c := range store.pendingWriteReqChans {
		c <- disableValueWriteReq
	}
	store.disableEnableWritesLock.Unlock()
}

// EnableWrites will resume accepting incoming Write and Delete requests.
func (store *DefaultValueStore) EnableWrites() {
	store.enableWrites(true)
}

func (store *DefaultValueStore) enableWrites(userCall bool) {
	store.disableEnableWritesLock.Lock()
	if userCall || !store.userDisabled {
		store.userDisabled = false
		for _, c := range store.pendingWriteReqChans {
			c <- enableValueWriteReq
		}
	}
	store.disableEnableWritesLock.Unlock()
}

// Flush will ensure buffered data (at the time of the call) is written to
// disk.
func (store *DefaultValueStore) Flush() {
	for _, c := range store.pendingWriteReqChans {
		c <- flushValueWriteReq
	}
	<-store.flushedChan
}

// Lookup will return timestampmicro, length, err for keyA, keyB.
//
// Note that err == ErrNotFound with timestampmicro == 0 indicates keyA, keyB
// was not known at all whereas err == ErrNotFound with timestampmicro != 0
// indicates keyA, keyB
// was known and had a deletion marker (aka tombstone).
func (store *DefaultValueStore) Lookup(keyA uint64, keyB uint64) (int64, uint32, error) {
	atomic.AddInt32(&store.lookups, 1)
	timestampbits, _, length, err := store.lookup(keyA, keyB)
	if err != nil {
		atomic.AddInt32(&store.lookupErrors, 1)
	}
	return int64(timestampbits >> _TSB_UTIL_BITS), length, err
}

func (store *DefaultValueStore) lookup(keyA uint64, keyB uint64) (uint64, uint32, uint32, error) {
	timestampbits, id, _, length := store.locmap.Get(keyA, keyB)
	if id == 0 || timestampbits&_TSB_DELETION != 0 {
		return timestampbits, id, 0, ErrNotFound
	}
	return timestampbits, id, length, nil
}

// Read will return timestampmicro, value, err for keyA, keyB;
// if an incoming value is provided, the read value will be appended to it and
// the whole returned (useful to reuse an existing []byte).
//
// Note that err == ErrNotFound with timestampmicro == 0 indicates keyA, keyB
// was not known at all whereas err == ErrNotFound with timestampmicro != 0
// indicates keyA, keyB was known and had a deletion marker (aka tombstone).
func (store *DefaultValueStore) Read(keyA uint64, keyB uint64, value []byte) (int64, []byte, error) {
	atomic.AddInt32(&store.reads, 1)
	timestampbits, value, err := store.read(keyA, keyB, value)
	if err != nil {
		atomic.AddInt32(&store.readErrors, 1)
	}
	return int64(timestampbits >> _TSB_UTIL_BITS), value, err
}

func (store *DefaultValueStore) read(keyA uint64, keyB uint64, value []byte) (uint64, []byte, error) {
	timestampbits, id, offset, length := store.locmap.Get(keyA, keyB)
	if id == 0 || timestampbits&_TSB_DELETION != 0 || timestampbits&_TSB_LOCAL_REMOVAL != 0 {
		return timestampbits, value, ErrNotFound
	}
	return store.locBlock(id).read(keyA, keyB, timestampbits, offset, length, value)
}

// Write stores timestampmicro, value for keyA, keyB
// and returns the previously stored timestampmicro or returns any error; a
// newer timestampmicro already in place is not reported as an error. Note that
// with a write and a delete for the exact same timestampmicro, the delete
// wins.
func (store *DefaultValueStore) Write(keyA uint64, keyB uint64, timestampmicro int64, value []byte) (int64, error) {
	atomic.AddInt32(&store.writes, 1)
	if timestampmicro < TIMESTAMPMICRO_MIN {
		atomic.AddInt32(&store.writeErrors, 1)
		return 0, fmt.Errorf("timestamp %d < %d", timestampmicro, TIMESTAMPMICRO_MIN)
	}
	if timestampmicro > TIMESTAMPMICRO_MAX {
		atomic.AddInt32(&store.writeErrors, 1)
		return 0, fmt.Errorf("timestamp %d > %d", timestampmicro, TIMESTAMPMICRO_MAX)
	}
	timestampbits, err := store.write(keyA, keyB, uint64(timestampmicro)<<_TSB_UTIL_BITS, value, false)
	if err != nil {
		atomic.AddInt32(&store.writeErrors, 1)
	}
	if timestampmicro <= int64(timestampbits>>_TSB_UTIL_BITS) {
		atomic.AddInt32(&store.writesOverridden, 1)
	}
	return int64(timestampbits >> _TSB_UTIL_BITS), err
}

func (store *DefaultValueStore) write(keyA uint64, keyB uint64, timestampbits uint64, value []byte, internal bool) (uint64, error) {
	i := int(keyA>>1) % len(store.freeWriteReqChans)
	writeReq := <-store.freeWriteReqChans[i]
	writeReq.keyA = keyA
	writeReq.keyB = keyB

	writeReq.timestampbits = timestampbits
	writeReq.value = value
	writeReq.internal = internal
	store.pendingWriteReqChans[i] <- writeReq
	err := <-writeReq.errChan
	ptimestampbits := writeReq.timestampbits
	writeReq.value = nil
	store.freeWriteReqChans[i] <- writeReq
	return ptimestampbits, err
}

// Delete stores timestampmicro for keyA, keyB
// and returns the previously stored timestampmicro or returns any error; a
// newer timestampmicro already in place is not reported as an error. Note that
// with a write and a delete for the exact same timestampmicro, the delete
// wins.
func (store *DefaultValueStore) Delete(keyA uint64, keyB uint64, timestampmicro int64) (int64, error) {
	atomic.AddInt32(&store.deletes, 1)
	if timestampmicro < TIMESTAMPMICRO_MIN {
		atomic.AddInt32(&store.deleteErrors, 1)
		return 0, fmt.Errorf("timestamp %d < %d", timestampmicro, TIMESTAMPMICRO_MIN)
	}
	if timestampmicro > TIMESTAMPMICRO_MAX {
		atomic.AddInt32(&store.deleteErrors, 1)
		return 0, fmt.Errorf("timestamp %d > %d", timestampmicro, TIMESTAMPMICRO_MAX)
	}
	ptimestampbits, err := store.write(keyA, keyB, (uint64(timestampmicro)<<_TSB_UTIL_BITS)|_TSB_DELETION, nil, true)
	if err != nil {
		atomic.AddInt32(&store.deleteErrors, 1)
	}
	if timestampmicro <= int64(ptimestampbits>>_TSB_UTIL_BITS) {
		atomic.AddInt32(&store.deletesOverridden, 1)
	}
	return int64(ptimestampbits >> _TSB_UTIL_BITS), err
}

func (store *DefaultValueStore) locBlock(locBlockID uint32) valueLocBlock {
	return store.locBlocks[locBlockID]
}

func (store *DefaultValueStore) addLocBlock(block valueLocBlock) (uint32, error) {
	id := atomic.AddUint64(&store.locBlockIDer, 1)
	if id >= math.MaxUint32 {
		return 0, errors.New("too many loc blocks")
	}
	store.locBlocks[id] = block
	return uint32(id), nil
}

func (store *DefaultValueStore) locBlockIDFromTimestampnano(tsn int64) uint32 {
	for i := 1; i <= len(store.locBlocks); i++ {
		if store.locBlocks[i] == nil {
			return 0
		} else {
			if tsn == store.locBlocks[i].timestampnano() {
				return uint32(i)
			}
		}
	}
	return 0
}

func (store *DefaultValueStore) closeLocBlock(locBlockID uint32) error {
	return store.locBlocks[locBlockID].close()
}

func (store *DefaultValueStore) memClearer(freeableMemBlockChan chan *valueMemBlock) {
	var tb []byte
	var tbTS int64
	var tbOffset int
	for {
		memBlock := <-freeableMemBlockChan
		if memBlock == flushValueMemBlock {
			if tb != nil {
				store.pendingTOCBlockChan <- tb
				tb = nil
			}
			store.pendingTOCBlockChan <- nil
			continue
		}
		fl := store.locBlock(memBlock.fileID)
		if tb != nil && tbTS != fl.timestampnano() {
			store.pendingTOCBlockChan <- tb
			tb = nil
		}
		for memBlockTOCOffset := 0; memBlockTOCOffset < len(memBlock.toc); memBlockTOCOffset += _VALUE_FILE_ENTRY_SIZE {

			keyA := binary.BigEndian.Uint64(memBlock.toc[memBlockTOCOffset:])
			keyB := binary.BigEndian.Uint64(memBlock.toc[memBlockTOCOffset+8:])
			timestampbits := binary.BigEndian.Uint64(memBlock.toc[memBlockTOCOffset+16:])

			var blockID uint32
			var offset uint32
			var length uint32
			if timestampbits&_TSB_LOCAL_REMOVAL == 0 {
				blockID = memBlock.fileID

				offset = memBlock.fileOffset + binary.BigEndian.Uint32(memBlock.toc[memBlockTOCOffset+24:])
				length = binary.BigEndian.Uint32(memBlock.toc[memBlockTOCOffset+28:])

			}
			if store.locmap.Set(keyA, keyB, timestampbits, blockID, offset, length, true) > timestampbits {
				continue
			}
			if tb != nil && tbOffset+_VALUE_FILE_ENTRY_SIZE > cap(tb) {
				store.pendingTOCBlockChan <- tb
				tb = nil
			}
			if tb == nil {
				tb = <-store.freeTOCBlockChan
				tbTS = fl.timestampnano()
				tb = tb[:8]
				binary.BigEndian.PutUint64(tb, uint64(tbTS))
				tbOffset = 8
			}
			tb = tb[:tbOffset+_VALUE_FILE_ENTRY_SIZE]

			binary.BigEndian.PutUint64(tb[tbOffset:], keyA)
			binary.BigEndian.PutUint64(tb[tbOffset+8:], keyB)
			binary.BigEndian.PutUint64(tb[tbOffset+16:], timestampbits)
			binary.BigEndian.PutUint32(tb[tbOffset+24:], offset)
			binary.BigEndian.PutUint32(tb[tbOffset+28:], length)

			tbOffset += _VALUE_FILE_ENTRY_SIZE
		}
		memBlock.discardLock.Lock()
		memBlock.fileID = 0
		memBlock.fileOffset = 0
		memBlock.toc = memBlock.toc[:0]
		memBlock.values = memBlock.values[:0]
		memBlock.discardLock.Unlock()
		store.freeMemBlockChan <- memBlock
	}
}

func (store *DefaultValueStore) memWriter(pendingWriteReqChan chan *valueWriteReq) {
	var enabled bool
	var memBlock *valueMemBlock
	var memBlockTOCOffset int
	var memBlockMemOffset int
	for {
		writeReq := <-pendingWriteReqChan
		if writeReq == enableValueWriteReq {
			enabled = true
			continue
		}
		if writeReq == disableValueWriteReq {
			enabled = false
			continue
		}
		if writeReq == flushValueWriteReq {
			if memBlock != nil && len(memBlock.toc) > 0 {
				store.fileMemBlockChan <- memBlock
				memBlock = nil
			}
			store.fileMemBlockChan <- flushValueMemBlock
			continue
		}
		if !enabled && !writeReq.internal {
			writeReq.errChan <- ErrDisabled
			continue
		}
		length := len(writeReq.value)
		if length > int(store.valueCap) {
			writeReq.errChan <- fmt.Errorf("value length of %d > %d", length, store.valueCap)
			continue
		}
		alloc := length
		if alloc < store.minValueAlloc {
			alloc = store.minValueAlloc
		}
		if memBlock != nil && (memBlockTOCOffset+_VALUE_FILE_ENTRY_SIZE > cap(memBlock.toc) || memBlockMemOffset+alloc > cap(memBlock.values)) {
			store.fileMemBlockChan <- memBlock
			memBlock = nil
		}
		if memBlock == nil {
			memBlock = <-store.freeMemBlockChan
			memBlockTOCOffset = 0
			memBlockMemOffset = 0
		}
		memBlock.discardLock.Lock()
		memBlock.values = memBlock.values[:memBlockMemOffset+alloc]
		memBlock.discardLock.Unlock()
		copy(memBlock.values[memBlockMemOffset:], writeReq.value)
		if alloc > length {
			for i, j := memBlockMemOffset+length, memBlockMemOffset+alloc; i < j; i++ {
				memBlock.values[i] = 0
			}
		}
		ptimestampbits := store.locmap.Set(writeReq.keyA, writeReq.keyB, writeReq.timestampbits, memBlock.id, uint32(memBlockMemOffset), uint32(length), false)
		if ptimestampbits < writeReq.timestampbits {
			memBlock.toc = memBlock.toc[:memBlockTOCOffset+_VALUE_FILE_ENTRY_SIZE]

			binary.BigEndian.PutUint64(memBlock.toc[memBlockTOCOffset:], writeReq.keyA)
			binary.BigEndian.PutUint64(memBlock.toc[memBlockTOCOffset+8:], writeReq.keyB)
			binary.BigEndian.PutUint64(memBlock.toc[memBlockTOCOffset+16:], writeReq.timestampbits)
			binary.BigEndian.PutUint32(memBlock.toc[memBlockTOCOffset+24:], uint32(memBlockMemOffset))
			binary.BigEndian.PutUint32(memBlock.toc[memBlockTOCOffset+28:], uint32(length))

			memBlockTOCOffset += _VALUE_FILE_ENTRY_SIZE
			memBlockMemOffset += alloc
		} else {
			memBlock.discardLock.Lock()
			memBlock.values = memBlock.values[:memBlockMemOffset]
			memBlock.discardLock.Unlock()
		}
		writeReq.timestampbits = ptimestampbits
		writeReq.errChan <- nil
	}
}

func (store *DefaultValueStore) fileWriter() {
	var fl *valueFile
	memWritersFlushLeft := len(store.pendingWriteReqChans)
	var tocLen uint64
	var valueLen uint64
	for {
		memBlock := <-store.fileMemBlockChan
		if memBlock == flushValueMemBlock {
			memWritersFlushLeft--
			if memWritersFlushLeft > 0 {
				continue
			}
			if fl != nil {
				err := fl.closeWriting()
				if err != nil {
					store.logCritical("error closing %s: %s\n", fl.name, err)
				}
				fl = nil
			}
			for i := 0; i < len(store.freeableMemBlockChans); i++ {
				store.freeableMemBlockChans[i] <- flushValueMemBlock
			}
			memWritersFlushLeft = len(store.pendingWriteReqChans)
			continue
		}
		if fl != nil && (tocLen+uint64(len(memBlock.toc)) >= uint64(store.fileCap) || valueLen+uint64(len(memBlock.values)) > uint64(store.fileCap)) {
			err := fl.closeWriting()
			if err != nil {
				store.logCritical("error closing %s: %s\n", fl.name, err)
			}
			fl = nil
		}
		if fl == nil {
			var err error
			fl, err = createValueFile(store, osCreateWriteCloser, osOpenReadSeeker)
			if err != nil {
				store.logCritical("fileWriter: %s\n", err)
				break
			}
			tocLen = _VALUE_FILE_HEADER_SIZE
			valueLen = _VALUE_FILE_HEADER_SIZE
		}
		fl.write(memBlock)
		tocLen += uint64(len(memBlock.toc))
		valueLen += uint64(len(memBlock.values))
	}
}

func (store *DefaultValueStore) tocWriter() {
	// writerA is the current toc file while writerB is the previously active
	// toc writerB is kept around in case a "late" key arrives to be flushed
	// whom's value is actually in the previous value file.
	memClearersFlushLeft := len(store.freeableMemBlockChans)
	var writerA io.WriteCloser
	var offsetA uint64
	var writerB io.WriteCloser
	var offsetB uint64
	var err error
	head := []byte("VALUESTORETOC v0                ")
	binary.BigEndian.PutUint32(head[28:], uint32(store.checksumInterval))
	term := make([]byte, 16)
	copy(term[12:], "TERM")
OuterLoop:
	for {
		t := <-store.pendingTOCBlockChan
		if t == nil {
			memClearersFlushLeft--
			if memClearersFlushLeft > 0 {
				continue
			}
			if writerB != nil {
				binary.BigEndian.PutUint64(term[4:], offsetB)
				if _, err = writerB.Write(term); err != nil {
					break OuterLoop
				}
				if err = writerB.Close(); err != nil {
					break OuterLoop
				}
				writerB = nil
				atomic.StoreUint64(&store.activeTOCB, 0)
				offsetB = 0
			}
			if writerA != nil {
				binary.BigEndian.PutUint64(term[4:], offsetA)
				if _, err = writerA.Write(term); err != nil {
					break OuterLoop
				}
				if err = writerA.Close(); err != nil {
					break OuterLoop
				}
				writerA = nil
				atomic.StoreUint64(&store.activeTOCA, 0)
				offsetA = 0
			}
			store.flushedChan <- struct{}{}
			memClearersFlushLeft = len(store.freeableMemBlockChans)
			continue
		}
		if len(t) > 8 {
			bts := binary.BigEndian.Uint64(t)
			switch bts {
			case atomic.LoadUint64(&store.activeTOCA):
				if _, err = writerA.Write(t[8:]); err != nil {
					break OuterLoop
				}
				offsetA += uint64(len(t) - 8)
			case atomic.LoadUint64(&store.activeTOCB):
				if _, err = writerB.Write(t[8:]); err != nil {
					break OuterLoop
				}
				offsetB += uint64(len(t) - 8)
			default:
				// An assumption is made here: If the timestampnano for this
				// toc block doesn't match the last two seen timestampnanos
				// then we expect no more toc blocks for the oldest
				// timestampnano and can close that toc file.
				if writerB != nil {
					binary.BigEndian.PutUint64(term[4:], offsetB)
					if _, err = writerB.Write(term); err != nil {
						break OuterLoop
					}
					if err = writerB.Close(); err != nil {
						break OuterLoop
					}
				}
				atomic.StoreUint64(&store.activeTOCB, atomic.LoadUint64(&store.activeTOCA))
				writerB = writerA
				offsetB = offsetA
				atomic.StoreUint64(&store.activeTOCA, bts)
				var fp *os.File
				fp, err = os.Create(path.Join(store.pathtoc, fmt.Sprintf("%d.valuetoc", bts)))
				if err != nil {
					break OuterLoop
				}
				writerA = brimutil.NewMultiCoreChecksummedWriter(fp, int(store.checksumInterval), murmur3.New32, store.workers)
				if _, err = writerA.Write(head); err != nil {
					break OuterLoop
				}
				if _, err = writerA.Write(t[8:]); err != nil {
					break OuterLoop
				}
				offsetA = _VALUE_FILE_HEADER_SIZE + uint64(len(t)-8)
			}
		}
		store.freeTOCBlockChan <- t[:0]
	}
	if err != nil {
		store.logCritical("tocWriter: %s\n", err)
	}
	if writerA != nil {
		writerA.Close()
	}
	if writerB != nil {
		writerB.Close()
	}
}

func (store *DefaultValueStore) recovery() error {
	start := time.Now()
	fromDiskCount := 0
	causedChangeCount := int64(0)
	type writeReq struct {
		keyA uint64
		keyB uint64

		timestampbits uint64
		blockID       uint32
		offset        uint32
		length        uint32
	}
	workers := uint64(store.workers)
	pendingBatchChans := make([]chan []writeReq, workers)
	freeBatchChans := make([]chan []writeReq, len(pendingBatchChans))
	for i := 0; i < len(pendingBatchChans); i++ {
		pendingBatchChans[i] = make(chan []writeReq, 4)
		freeBatchChans[i] = make(chan []writeReq, 4)
		for j := 0; j < cap(freeBatchChans[i]); j++ {
			freeBatchChans[i] <- make([]writeReq, store.recoveryBatchSize)
		}
	}
	wg := &sync.WaitGroup{}
	wg.Add(len(pendingBatchChans))
	for i := 0; i < len(pendingBatchChans); i++ {
		go func(pendingBatchChan chan []writeReq, freeBatchChan chan []writeReq) {
			for {
				batch := <-pendingBatchChan
				if batch == nil {
					break
				}
				for j := 0; j < len(batch); j++ {
					wr := &batch[j]
					if wr.timestampbits&_TSB_LOCAL_REMOVAL != 0 {
						wr.blockID = 0
					}
					if store.logDebug != nil {
						if store.locmap.Set(wr.keyA, wr.keyB, wr.timestampbits, wr.blockID, wr.offset, wr.length, true) < wr.timestampbits {
							atomic.AddInt64(&causedChangeCount, 1)
						}
					} else {
						store.locmap.Set(wr.keyA, wr.keyB, wr.timestampbits, wr.blockID, wr.offset, wr.length, true)
					}
				}
				freeBatchChan <- batch
			}
			wg.Done()
		}(pendingBatchChans[i], freeBatchChans[i])
	}
	fromDiskBuf := make([]byte, store.checksumInterval+4)
	fromDiskOverflow := make([]byte, 0, _VALUE_FILE_ENTRY_SIZE)
	batches := make([][]writeReq, len(freeBatchChans))
	batchesPos := make([]int, len(batches))
	fp, err := os.Open(store.pathtoc)
	if err != nil {
		return err
	}
	names, err := fp.Readdirnames(-1)
	fp.Close()
	if err != nil {
		return err
	}
	sort.Strings(names)
	for i := 0; i < len(names); i++ {
		if !strings.HasSuffix(names[i], ".valuetoc") {
			continue
		}
		namets := int64(0)
		if namets, err = strconv.ParseInt(names[i][:len(names[i])-len(".valuetoc")], 10, 64); err != nil {
			store.logError("bad timestamp in name: %#v\n", names[i])
			continue
		}
		if namets == 0 {
			store.logError("bad timestamp in name: %#v\n", names[i])
			continue
		}
		fl, err := newValueFile(store, namets, osOpenReadSeeker)
		if err != nil {
			store.logError("error opening %s: %s\n", names[i], err)
			continue
		}
		fp, err := os.Open(path.Join(store.pathtoc, names[i]))
		if err != nil {
			store.logError("error opening %s: %s\n", names[i], err)
			continue
		}
		checksumFailures := 0
		first := true
		terminated := false
		fromDiskOverflow = fromDiskOverflow[:0]
		for {
			n, err := io.ReadFull(fp, fromDiskBuf)
			if n < 4 {
				if err != io.EOF && err != io.ErrUnexpectedEOF {
					store.logError("error reading %s: %s\n", names[i], err)
				}
				break
			}
			n -= 4
			if murmur3.Sum32(fromDiskBuf[:n]) != binary.BigEndian.Uint32(fromDiskBuf[n:]) {
				checksumFailures++
			} else {
				j := 0
				if first {
					if !bytes.Equal(fromDiskBuf[:_VALUE_FILE_HEADER_SIZE-4], []byte("VALUESTORETOC v0            ")) {
						store.logError("bad header: %s\n", names[i])
						break
					}
					if binary.BigEndian.Uint32(fromDiskBuf[_VALUE_FILE_HEADER_SIZE-4:]) != store.checksumInterval {
						store.logError("bad header checksum interval: %s\n", names[i])
						break
					}
					j += _VALUE_FILE_HEADER_SIZE
					first = false
				}
				if n < int(store.checksumInterval) {
					if binary.BigEndian.Uint32(fromDiskBuf[n-_VALUE_FILE_TRAILER_SIZE:]) != 0 {
						store.logError("bad terminator size marker: %s\n", names[i])
						break
					}
					if !bytes.Equal(fromDiskBuf[n-4:n], []byte("TERM")) {
						store.logError("bad terminator: %s\n", names[i])
						break
					}
					n -= _VALUE_FILE_TRAILER_SIZE
					terminated = true
				}
				if len(fromDiskOverflow) > 0 {
					j += _VALUE_FILE_ENTRY_SIZE - len(fromDiskOverflow)
					fromDiskOverflow = append(fromDiskOverflow, fromDiskBuf[j-_VALUE_FILE_ENTRY_SIZE+len(fromDiskOverflow):j]...)
					keyB := binary.BigEndian.Uint64(fromDiskOverflow[8:])
					k := keyB % workers
					if batches[k] == nil {
						batches[k] = <-freeBatchChans[k]
						batchesPos[k] = 0
					}
					wr := &batches[k][batchesPos[k]]

					wr.keyA = binary.BigEndian.Uint64(fromDiskOverflow)
					wr.keyB = keyB
					wr.timestampbits = binary.BigEndian.Uint64(fromDiskOverflow[16:])
					wr.blockID = fl.id
					wr.offset = binary.BigEndian.Uint32(fromDiskOverflow[24:])
					wr.length = binary.BigEndian.Uint32(fromDiskOverflow[28:])

					batchesPos[k]++
					if batchesPos[k] >= store.recoveryBatchSize {
						pendingBatchChans[k] <- batches[k]
						batches[k] = nil
					}
					fromDiskCount++
					fromDiskOverflow = fromDiskOverflow[:0]
				}
				for ; j+_VALUE_FILE_ENTRY_SIZE <= n; j += _VALUE_FILE_ENTRY_SIZE {
					keyB := binary.BigEndian.Uint64(fromDiskBuf[j+8:])
					k := keyB % workers
					if batches[k] == nil {
						batches[k] = <-freeBatchChans[k]
						batchesPos[k] = 0
					}
					wr := &batches[k][batchesPos[k]]

					wr.keyA = binary.BigEndian.Uint64(fromDiskBuf[j:])
					wr.keyB = keyB
					wr.timestampbits = binary.BigEndian.Uint64(fromDiskBuf[j+16:])
					wr.blockID = fl.id
					wr.offset = binary.BigEndian.Uint32(fromDiskBuf[j+24:])
					wr.length = binary.BigEndian.Uint32(fromDiskBuf[j+28:])

					batchesPos[k]++
					if batchesPos[k] >= store.recoveryBatchSize {
						pendingBatchChans[k] <- batches[k]
						batches[k] = nil
					}
					fromDiskCount++
				}
				if j != n {
					fromDiskOverflow = fromDiskOverflow[:n-j]
					copy(fromDiskOverflow, fromDiskBuf[j:])
				}
			}
			if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
				store.logError("error reading %s: %s\n", names[i], err)
				break
			}
		}
		fp.Close()
		if !terminated {
			store.logError("early end of file: %s\n", names[i])
		}
		if checksumFailures > 0 {
			store.logWarning("%d checksum failures for %s\n", checksumFailures, names[i])
		}
	}
	for i := 0; i < len(batches); i++ {
		if batches[i] != nil {
			pendingBatchChans[i] <- batches[i][:batchesPos[i]]
		}
		pendingBatchChans[i] <- nil
	}
	wg.Wait()
	if store.logDebug != nil {
		dur := time.Now().Sub(start)
		stats := store.Stats(false).(*ValueStoreStats)
		store.logInfo("%d key locations loaded in %s, %.0f/s; %d caused change; %d resulting locations referencing %d bytes.\n", fromDiskCount, dur, float64(fromDiskCount)/(float64(dur)/float64(time.Second)), causedChangeCount, stats.Values, stats.ValueBytes)
	}
	return nil
}