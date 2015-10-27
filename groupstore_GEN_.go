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

// DefaultGroupStore instances are created with NewGroupStore.
type DefaultGroupStore struct {
	logCritical             LogFunc
	logError                LogFunc
	logWarning              LogFunc
	logInfo                 LogFunc
	logDebug                LogFunc
	randMutex               sync.Mutex
	rand                    *rand.Rand
	freeableMemBlockChans   []chan *groupMemBlock
	freeMemBlockChan        chan *groupMemBlock
	freeWriteReqChans       []chan *groupWriteReq
	pendingWriteReqChans    []chan *groupWriteReq
	fileMemBlockChan        chan *groupMemBlock
	freeTOCBlockChan        chan []byte
	pendingTOCBlockChan     chan []byte
	activeTOCA              uint64
	activeTOCB              uint64
	flushedChan             chan struct{}
	locBlocks               []groupLocBlock
	locBlockIDer            uint64
	path                    string
	pathtoc                 string
	locmap                  valuelocmap.GroupLocMap
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
	tombstoneDiscardState   groupTombstoneDiscardState
	replicationIgnoreRecent uint64
	pullReplicationState    groupPullReplicationState
	pushReplicationState    groupPushReplicationState
	compactionState         groupCompactionState
	bulkSetState            groupBulkSetState
	bulkSetAckState         groupBulkSetAckState
	disableEnableWritesLock sync.Mutex
	userDisabled            bool
	diskWatcherState        groupDiskWatcherState

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

type groupWriteReq struct {
	keyA uint64
	keyB uint64

	nameKeyA uint64
	nameKeyB uint64

	timestampbits uint64
	value         []byte
	errChan       chan error
	internal      bool
}

var enableGroupWriteReq *groupWriteReq = &groupWriteReq{}
var disableGroupWriteReq *groupWriteReq = &groupWriteReq{}
var flushGroupWriteReq *groupWriteReq = &groupWriteReq{}
var flushGroupMemBlock *groupMemBlock = &groupMemBlock{}

type groupLocBlock interface {
	timestampnano() int64
	read(keyA uint64, keyB uint64, nameKeyA uint64, nameKeyB uint64, timestampbits uint64, offset uint32, length uint32, value []byte) (uint64, []byte, error)
	close() error
}

// NewGroupStore creates a DefaultGroupStore for use in storing []byte values
// referenced by 128 bit keys.
//
// Note that a lot of buffering, multiple cores, and background processes can
// be in use and therefore DisableAll() and Flush() should be called prior to
// the process exiting to ensure all processing is done and the buffers are
// flushed.
func NewGroupStore(c *GroupStoreConfig) (*DefaultGroupStore, error) {
	cfg := resolveGroupStoreConfig(c)
	locmap := cfg.GroupLocMap
	if locmap == nil {
		locmap = valuelocmap.NewGroupLocMap(nil)
	}
	locmap.SetInactiveMask(_TSB_INACTIVE)
	store := &DefaultGroupStore{
		logCritical:             cfg.LogCritical,
		logError:                cfg.LogError,
		logWarning:              cfg.LogWarning,
		logInfo:                 cfg.LogInfo,
		logDebug:                cfg.LogDebug,
		rand:                    cfg.Rand,
		locBlocks:               make([]groupLocBlock, math.MaxUint16),
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
	store.freeableMemBlockChans = make([]chan *groupMemBlock, store.workers)
	for i := 0; i < cap(store.freeableMemBlockChans); i++ {
		store.freeableMemBlockChans[i] = make(chan *groupMemBlock, store.workers)
	}
	store.freeMemBlockChan = make(chan *groupMemBlock, store.workers*store.writePagesPerWorker)
	store.freeWriteReqChans = make([]chan *groupWriteReq, store.workers)
	store.pendingWriteReqChans = make([]chan *groupWriteReq, store.workers)
	store.fileMemBlockChan = make(chan *groupMemBlock, store.workers)
	store.freeTOCBlockChan = make(chan []byte, store.workers*2)
	store.pendingTOCBlockChan = make(chan []byte, store.workers)
	store.flushedChan = make(chan struct{}, 1)
	for i := 0; i < cap(store.freeMemBlockChan); i++ {
		memBlock := &groupMemBlock{
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
		store.freeWriteReqChans[i] = make(chan *groupWriteReq, store.workers*2)
		for j := 0; j < store.workers*2; j++ {
			store.freeWriteReqChans[i] <- &groupWriteReq{errChan: make(chan error, 1)}
		}
	}
	for i := 0; i < len(store.pendingWriteReqChans); i++ {
		store.pendingWriteReqChans[i] = make(chan *groupWriteReq)
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

// ValueCap returns the maximum length of a value the GroupStore can accept.
func (store *DefaultGroupStore) ValueCap() uint32 {
	return store.valueCap
}

// DisableAll calls DisableAllBackground(), and DisableWrites().
func (store *DefaultGroupStore) DisableAll() {
	store.DisableAllBackground()
	store.DisableWrites()
}

// DisableAllBackground calls DisableTombstoneDiscard(), DisableCompaction(),
// DisableOutPullReplication(), DisableOutPushReplication(), but does *not*
// call DisableWrites().
func (store *DefaultGroupStore) DisableAllBackground() {
	store.DisableTombstoneDiscard()
	store.DisableCompaction()
	store.DisableOutPullReplication()
	store.DisableOutPushReplication()
}

// EnableAll calls EnableTombstoneDiscard(), EnableCompaction(),
// EnableOutPullReplication(), EnableOutPushReplication(), and EnableWrites().
func (store *DefaultGroupStore) EnableAll() {
	store.EnableTombstoneDiscard()
	store.EnableOutPullReplication()
	store.EnableOutPushReplication()
	store.EnableWrites()
	store.EnableCompaction()
}

// DisableWrites will cause any incoming Write or Delete requests to respond
// with ErrDisabled until EnableWrites is called.
func (store *DefaultGroupStore) DisableWrites() {
	store.disableWrites(true)
}

func (store *DefaultGroupStore) disableWrites(userCall bool) {
	store.disableEnableWritesLock.Lock()
	if userCall {
		store.userDisabled = true
	}
	for _, c := range store.pendingWriteReqChans {
		c <- disableGroupWriteReq
	}
	store.disableEnableWritesLock.Unlock()
}

// EnableWrites will resume accepting incoming Write and Delete requests.
func (store *DefaultGroupStore) EnableWrites() {
	store.enableWrites(true)
}

func (store *DefaultGroupStore) enableWrites(userCall bool) {
	store.disableEnableWritesLock.Lock()
	if userCall || !store.userDisabled {
		store.userDisabled = false
		for _, c := range store.pendingWriteReqChans {
			c <- enableGroupWriteReq
		}
	}
	store.disableEnableWritesLock.Unlock()
}

// Flush will ensure buffered data (at the time of the call) is written to
// disk.
func (store *DefaultGroupStore) Flush() {
	for _, c := range store.pendingWriteReqChans {
		c <- flushGroupWriteReq
	}
	<-store.flushedChan
}

// Lookup will return timestampmicro, length, err for keyA, keyB, nameKeyA, nameKeyB.
//
// Note that err == ErrNotFound with timestampmicro == 0 indicates keyA, keyB, nameKeyA, nameKeyB
// was not known at all whereas err == ErrNotFound with timestampmicro != 0
// indicates keyA, keyB, nameKeyA, nameKeyB
// was known and had a deletion marker (aka tombstone).
func (store *DefaultGroupStore) Lookup(keyA uint64, keyB uint64, nameKeyA uint64, nameKeyB uint64) (int64, uint32, error) {
	atomic.AddInt32(&store.lookups, 1)
	timestampbits, _, length, err := store.lookup(keyA, keyB, nameKeyA, nameKeyB)
	if err != nil {
		atomic.AddInt32(&store.lookupErrors, 1)
	}
	return int64(timestampbits >> _TSB_UTIL_BITS), length, err
}

func (store *DefaultGroupStore) lookup(keyA uint64, keyB uint64, nameKeyA uint64, nameKeyB uint64) (uint64, uint32, uint32, error) {
	timestampbits, id, _, length := store.locmap.Get(keyA, keyB, nameKeyA, nameKeyB)
	if id == 0 || timestampbits&_TSB_DELETION != 0 {
		return timestampbits, id, 0, ErrNotFound
	}
	return timestampbits, id, length, nil
}

type LookupGroupItem struct {
	NameKeyA       uint64
	NameKeyB       uint64
	TimestampMicro uint64
}

// LookupGroup returns all the nameKeyA, nameKeyB, TimestampMicro items
// matching under keyA, keyB.
func (store *DefaultGroupStore) LookupGroup(keyA uint64, keyB uint64) []LookupGroupItem {
	atomic.AddInt32(&store.lookupGroups, 1)
	items := store.locmap.GetGroup(keyA, keyB)
	if len(items) == 0 {
		return nil
	}
	atomic.AddInt32(&store.lookupGroupItems, int32(len(items)))
	rv := make([]LookupGroupItem, len(items))
	for i, item := range items {
		rv[i].NameKeyA = item.NameKeyA
		rv[i].NameKeyB = item.NameKeyB
		rv[i].TimestampMicro = item.Timestamp >> _TSB_UTIL_BITS
	}
	return rv
}

// Read will return timestampmicro, value, err for keyA, keyB, nameKeyA, nameKeyB;
// if an incoming value is provided, the read value will be appended to it and
// the whole returned (useful to reuse an existing []byte).
//
// Note that err == ErrNotFound with timestampmicro == 0 indicates keyA, keyB, nameKeyA, nameKeyB
// was not known at all whereas err == ErrNotFound with timestampmicro != 0
// indicates keyA, keyB, nameKeyA, nameKeyB was known and had a deletion marker (aka tombstone).
func (store *DefaultGroupStore) Read(keyA uint64, keyB uint64, nameKeyA uint64, nameKeyB uint64, value []byte) (int64, []byte, error) {
	atomic.AddInt32(&store.reads, 1)
	timestampbits, value, err := store.read(keyA, keyB, nameKeyA, nameKeyB, value)
	if err != nil {
		atomic.AddInt32(&store.readErrors, 1)
	}
	return int64(timestampbits >> _TSB_UTIL_BITS), value, err
}

func (store *DefaultGroupStore) read(keyA uint64, keyB uint64, nameKeyA uint64, nameKeyB uint64, value []byte) (uint64, []byte, error) {
	timestampbits, id, offset, length := store.locmap.Get(keyA, keyB, nameKeyA, nameKeyB)
	if id == 0 || timestampbits&_TSB_DELETION != 0 || timestampbits&_TSB_LOCAL_REMOVAL != 0 {
		return timestampbits, value, ErrNotFound
	}
	return store.locBlock(id).read(keyA, keyB, nameKeyA, nameKeyB, timestampbits, offset, length, value)
}

type ReadGroupItem struct {
	Error          error
	NameKeyA       uint64
	NameKeyB       uint64
	TimestampMicro uint64
	Value          []byte
}

// ReadGroup returns all the items with keyA, keyB; the returned int indicates
// a an estimate of the item count and the items are return through the
// channel. Note that the int is just an estimate; a different number of items
// may be returned.
func (store *DefaultGroupStore) ReadGroup(keyA uint64, keyB uint64) (int, chan *ReadGroupItem) {
	atomic.AddInt32(&store.readGroups, 1)
	items := store.locmap.GetGroup(keyA, keyB)
	c := make(chan *ReadGroupItem, store.workers)
	if len(items) == 0 {
		close(c)
		return 0, c
	}
	atomic.AddInt32(&store.readGroupItems, int32(len(items)))
	go func() {
		for _, item := range items {
			t, v, err := store.read(keyA, keyB, item.NameKeyA, item.NameKeyB, nil)
			if err != nil {
				if err != ErrNotFound {
					c <- &ReadGroupItem{Error: err}
					break
				}
			}
			c <- &ReadGroupItem{
				NameKeyA:       item.NameKeyA,
				NameKeyB:       item.NameKeyB,
				TimestampMicro: t,
				Value:          v,
			}
		}
		close(c)
	}()
	return len(items), c
}

// Write stores timestampmicro, value for keyA, keyB, nameKeyA, nameKeyB
// and returns the previously stored timestampmicro or returns any error; a
// newer timestampmicro already in place is not reported as an error. Note that
// with a write and a delete for the exact same timestampmicro, the delete
// wins.
func (store *DefaultGroupStore) Write(keyA uint64, keyB uint64, nameKeyA uint64, nameKeyB uint64, timestampmicro int64, value []byte) (int64, error) {
	atomic.AddInt32(&store.writes, 1)
	if timestampmicro < TIMESTAMPMICRO_MIN {
		atomic.AddInt32(&store.writeErrors, 1)
		return 0, fmt.Errorf("timestamp %d < %d", timestampmicro, TIMESTAMPMICRO_MIN)
	}
	if timestampmicro > TIMESTAMPMICRO_MAX {
		atomic.AddInt32(&store.writeErrors, 1)
		return 0, fmt.Errorf("timestamp %d > %d", timestampmicro, TIMESTAMPMICRO_MAX)
	}
	timestampbits, err := store.write(keyA, keyB, nameKeyA, nameKeyB, uint64(timestampmicro)<<_TSB_UTIL_BITS, value, false)
	if err != nil {
		atomic.AddInt32(&store.writeErrors, 1)
	}
	if timestampmicro <= int64(timestampbits>>_TSB_UTIL_BITS) {
		atomic.AddInt32(&store.writesOverridden, 1)
	}
	return int64(timestampbits >> _TSB_UTIL_BITS), err
}

func (store *DefaultGroupStore) write(keyA uint64, keyB uint64, nameKeyA uint64, nameKeyB uint64, timestampbits uint64, value []byte, internal bool) (uint64, error) {
	i := int(keyA>>1) % len(store.freeWriteReqChans)
	writeReq := <-store.freeWriteReqChans[i]
	writeReq.keyA = keyA
	writeReq.keyB = keyB

	writeReq.nameKeyA = nameKeyA
	writeReq.nameKeyB = nameKeyB

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

// Delete stores timestampmicro for keyA, keyB, nameKeyA, nameKeyB
// and returns the previously stored timestampmicro or returns any error; a
// newer timestampmicro already in place is not reported as an error. Note that
// with a write and a delete for the exact same timestampmicro, the delete
// wins.
func (store *DefaultGroupStore) Delete(keyA uint64, keyB uint64, nameKeyA uint64, nameKeyB uint64, timestampmicro int64) (int64, error) {
	atomic.AddInt32(&store.deletes, 1)
	if timestampmicro < TIMESTAMPMICRO_MIN {
		atomic.AddInt32(&store.deleteErrors, 1)
		return 0, fmt.Errorf("timestamp %d < %d", timestampmicro, TIMESTAMPMICRO_MIN)
	}
	if timestampmicro > TIMESTAMPMICRO_MAX {
		atomic.AddInt32(&store.deleteErrors, 1)
		return 0, fmt.Errorf("timestamp %d > %d", timestampmicro, TIMESTAMPMICRO_MAX)
	}
	ptimestampbits, err := store.write(keyA, keyB, nameKeyA, nameKeyB, (uint64(timestampmicro)<<_TSB_UTIL_BITS)|_TSB_DELETION, nil, true)
	if err != nil {
		atomic.AddInt32(&store.deleteErrors, 1)
	}
	if timestampmicro <= int64(ptimestampbits>>_TSB_UTIL_BITS) {
		atomic.AddInt32(&store.deletesOverridden, 1)
	}
	return int64(ptimestampbits >> _TSB_UTIL_BITS), err
}

func (store *DefaultGroupStore) locBlock(locBlockID uint32) groupLocBlock {
	return store.locBlocks[locBlockID]
}

func (store *DefaultGroupStore) addLocBlock(block groupLocBlock) (uint32, error) {
	id := atomic.AddUint64(&store.locBlockIDer, 1)
	if id >= math.MaxUint32 {
		return 0, errors.New("too many loc blocks")
	}
	store.locBlocks[id] = block
	return uint32(id), nil
}

func (store *DefaultGroupStore) locBlockIDFromTimestampnano(tsn int64) uint32 {
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

func (store *DefaultGroupStore) closeLocBlock(locBlockID uint32) error {
	return store.locBlocks[locBlockID].close()
}

func (store *DefaultGroupStore) memClearer(freeableMemBlockChan chan *groupMemBlock) {
	var tb []byte
	var tbTS int64
	var tbOffset int
	for {
		memBlock := <-freeableMemBlockChan
		if memBlock == flushGroupMemBlock {
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
		for memBlockTOCOffset := 0; memBlockTOCOffset < len(memBlock.toc); memBlockTOCOffset += _GROUP_FILE_ENTRY_SIZE {

			keyA := binary.BigEndian.Uint64(memBlock.toc[memBlockTOCOffset:])
			keyB := binary.BigEndian.Uint64(memBlock.toc[memBlockTOCOffset+8:])
			nameKeyA := binary.BigEndian.Uint64(memBlock.toc[memBlockTOCOffset+16:])
			nameKeyB := binary.BigEndian.Uint64(memBlock.toc[memBlockTOCOffset+24:])
			timestampbits := binary.BigEndian.Uint64(memBlock.toc[memBlockTOCOffset+32:])

			var blockID uint32
			var offset uint32
			var length uint32
			if timestampbits&_TSB_LOCAL_REMOVAL == 0 {
				blockID = memBlock.fileID

				offset = memBlock.fileOffset + binary.BigEndian.Uint32(memBlock.toc[memBlockTOCOffset+40:])
				length = binary.BigEndian.Uint32(memBlock.toc[memBlockTOCOffset+44:])

			}
			if store.locmap.Set(keyA, keyB, nameKeyA, nameKeyB, timestampbits, blockID, offset, length, true) > timestampbits {
				continue
			}
			if tb != nil && tbOffset+_GROUP_FILE_ENTRY_SIZE > cap(tb) {
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
			tb = tb[:tbOffset+_GROUP_FILE_ENTRY_SIZE]

			binary.BigEndian.PutUint64(tb[tbOffset:], keyA)
			binary.BigEndian.PutUint64(tb[tbOffset+8:], keyB)
			binary.BigEndian.PutUint64(tb[tbOffset+16:], nameKeyA)
			binary.BigEndian.PutUint64(tb[tbOffset+24:], nameKeyB)
			binary.BigEndian.PutUint64(tb[tbOffset+32:], timestampbits)
			binary.BigEndian.PutUint32(tb[tbOffset+40:], offset)
			binary.BigEndian.PutUint32(tb[tbOffset+44:], length)

			tbOffset += _GROUP_FILE_ENTRY_SIZE
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

func (store *DefaultGroupStore) memWriter(pendingWriteReqChan chan *groupWriteReq) {
	var enabled bool
	var memBlock *groupMemBlock
	var memBlockTOCOffset int
	var memBlockMemOffset int
	for {
		writeReq := <-pendingWriteReqChan
		if writeReq == enableGroupWriteReq {
			enabled = true
			continue
		}
		if writeReq == disableGroupWriteReq {
			enabled = false
			continue
		}
		if writeReq == flushGroupWriteReq {
			if memBlock != nil && len(memBlock.toc) > 0 {
				store.fileMemBlockChan <- memBlock
				memBlock = nil
			}
			store.fileMemBlockChan <- flushGroupMemBlock
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
		if memBlock != nil && (memBlockTOCOffset+_GROUP_FILE_ENTRY_SIZE > cap(memBlock.toc) || memBlockMemOffset+alloc > cap(memBlock.values)) {
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
		ptimestampbits := store.locmap.Set(writeReq.keyA, writeReq.keyB, writeReq.nameKeyA, writeReq.nameKeyB, writeReq.timestampbits, memBlock.id, uint32(memBlockMemOffset), uint32(length), false)
		if ptimestampbits < writeReq.timestampbits {
			memBlock.toc = memBlock.toc[:memBlockTOCOffset+_GROUP_FILE_ENTRY_SIZE]

			binary.BigEndian.PutUint64(memBlock.toc[memBlockTOCOffset:], writeReq.keyA)
			binary.BigEndian.PutUint64(memBlock.toc[memBlockTOCOffset+8:], writeReq.keyB)
			binary.BigEndian.PutUint64(memBlock.toc[memBlockTOCOffset+16:], writeReq.nameKeyA)
			binary.BigEndian.PutUint64(memBlock.toc[memBlockTOCOffset+24:], writeReq.nameKeyB)
			binary.BigEndian.PutUint64(memBlock.toc[memBlockTOCOffset+32:], writeReq.timestampbits)
			binary.BigEndian.PutUint32(memBlock.toc[memBlockTOCOffset+40:], uint32(memBlockMemOffset))
			binary.BigEndian.PutUint32(memBlock.toc[memBlockTOCOffset+44:], uint32(length))

			memBlockTOCOffset += _GROUP_FILE_ENTRY_SIZE
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

func (store *DefaultGroupStore) fileWriter() {
	var fl *groupFile
	memWritersFlushLeft := len(store.pendingWriteReqChans)
	var tocLen uint64
	var valueLen uint64
	for {
		memBlock := <-store.fileMemBlockChan
		if memBlock == flushGroupMemBlock {
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
				store.freeableMemBlockChans[i] <- flushGroupMemBlock
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
			fl, err = createGroupFile(store, osCreateWriteCloser, osOpenReadSeeker)
			if err != nil {
				store.logCritical("fileWriter: %s\n", err)
				break
			}
			tocLen = _GROUP_FILE_HEADER_SIZE
			valueLen = _GROUP_FILE_HEADER_SIZE
		}
		fl.write(memBlock)
		tocLen += uint64(len(memBlock.toc))
		valueLen += uint64(len(memBlock.values))
	}
}

func (store *DefaultGroupStore) tocWriter() {
	// writerA is the current toc file while writerB is the previously active
	// toc writerB is kept around in case a "late" key arrives to be flushed
	// whom's value is actually in the previous value file.
	memClearersFlushLeft := len(store.freeableMemBlockChans)
	var writerA io.WriteCloser
	var offsetA uint64
	var writerB io.WriteCloser
	var offsetB uint64
	var err error
	head := []byte("GROUPSTORETOC v0                ")
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
				fp, err = os.Create(path.Join(store.pathtoc, fmt.Sprintf("%d.grouptoc", bts)))
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
				offsetA = _GROUP_FILE_HEADER_SIZE + uint64(len(t)-8)
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

func (store *DefaultGroupStore) recovery() error {
	start := time.Now()
	fromDiskCount := 0
	causedChangeCount := int64(0)
	type writeReq struct {
		keyA uint64
		keyB uint64

		nameKeyA uint64
		nameKeyB uint64

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
						if store.locmap.Set(wr.keyA, wr.keyB, wr.nameKeyA, wr.nameKeyB, wr.timestampbits, wr.blockID, wr.offset, wr.length, true) < wr.timestampbits {
							atomic.AddInt64(&causedChangeCount, 1)
						}
					} else {
						store.locmap.Set(wr.keyA, wr.keyB, wr.nameKeyA, wr.nameKeyB, wr.timestampbits, wr.blockID, wr.offset, wr.length, true)
					}
				}
				freeBatchChan <- batch
			}
			wg.Done()
		}(pendingBatchChans[i], freeBatchChans[i])
	}
	fromDiskBuf := make([]byte, store.checksumInterval+4)
	fromDiskOverflow := make([]byte, 0, _GROUP_FILE_ENTRY_SIZE)
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
		if !strings.HasSuffix(names[i], ".grouptoc") {
			continue
		}
		namets := int64(0)
		if namets, err = strconv.ParseInt(names[i][:len(names[i])-len(".grouptoc")], 10, 64); err != nil {
			store.logError("bad timestamp in name: %#v\n", names[i])
			continue
		}
		if namets == 0 {
			store.logError("bad timestamp in name: %#v\n", names[i])
			continue
		}
		fl, err := newGroupFile(store, namets, osOpenReadSeeker)
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
					if !bytes.Equal(fromDiskBuf[:_GROUP_FILE_HEADER_SIZE-4], []byte("GROUPSTORETOC v0            ")) {
						store.logError("bad header: %s\n", names[i])
						break
					}
					if binary.BigEndian.Uint32(fromDiskBuf[_GROUP_FILE_HEADER_SIZE-4:]) != store.checksumInterval {
						store.logError("bad header checksum interval: %s\n", names[i])
						break
					}
					j += _GROUP_FILE_HEADER_SIZE
					first = false
				}
				if n < int(store.checksumInterval) {
					if binary.BigEndian.Uint32(fromDiskBuf[n-_GROUP_FILE_TRAILER_SIZE:]) != 0 {
						store.logError("bad terminator size marker: %s\n", names[i])
						break
					}
					if !bytes.Equal(fromDiskBuf[n-4:n], []byte("TERM")) {
						store.logError("bad terminator: %s\n", names[i])
						break
					}
					n -= _GROUP_FILE_TRAILER_SIZE
					terminated = true
				}
				if len(fromDiskOverflow) > 0 {
					j += _GROUP_FILE_ENTRY_SIZE - len(fromDiskOverflow)
					fromDiskOverflow = append(fromDiskOverflow, fromDiskBuf[j-_GROUP_FILE_ENTRY_SIZE+len(fromDiskOverflow):j]...)
					keyB := binary.BigEndian.Uint64(fromDiskOverflow[8:])
					k := keyB % workers
					if batches[k] == nil {
						batches[k] = <-freeBatchChans[k]
						batchesPos[k] = 0
					}
					wr := &batches[k][batchesPos[k]]

					wr.keyA = binary.BigEndian.Uint64(fromDiskOverflow)
					wr.keyB = keyB
					wr.nameKeyA = binary.BigEndian.Uint64(fromDiskOverflow[16:])
					wr.nameKeyB = binary.BigEndian.Uint64(fromDiskOverflow[24:])
					wr.timestampbits = binary.BigEndian.Uint64(fromDiskOverflow[32:])
					wr.blockID = fl.id
					wr.offset = binary.BigEndian.Uint32(fromDiskOverflow[40:])
					wr.length = binary.BigEndian.Uint32(fromDiskOverflow[44:])

					batchesPos[k]++
					if batchesPos[k] >= store.recoveryBatchSize {
						pendingBatchChans[k] <- batches[k]
						batches[k] = nil
					}
					fromDiskCount++
					fromDiskOverflow = fromDiskOverflow[:0]
				}
				for ; j+_GROUP_FILE_ENTRY_SIZE <= n; j += _GROUP_FILE_ENTRY_SIZE {
					keyB := binary.BigEndian.Uint64(fromDiskBuf[j+8:])
					k := keyB % workers
					if batches[k] == nil {
						batches[k] = <-freeBatchChans[k]
						batchesPos[k] = 0
					}
					wr := &batches[k][batchesPos[k]]

					wr.keyA = binary.BigEndian.Uint64(fromDiskBuf[j:])
					wr.keyB = keyB
					wr.nameKeyA = binary.BigEndian.Uint64(fromDiskBuf[j+16:])
					wr.nameKeyB = binary.BigEndian.Uint64(fromDiskBuf[j+24:])
					wr.timestampbits = binary.BigEndian.Uint64(fromDiskBuf[j+32:])
					wr.blockID = fl.id
					wr.offset = binary.BigEndian.Uint32(fromDiskBuf[j+40:])
					wr.length = binary.BigEndian.Uint32(fromDiskBuf[j+44:])

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
		stats := store.Stats(false).(*GroupStoreStats)
		store.logInfo("%d key locations loaded in %s, %.0f/s; %d caused change; %d resulting locations referencing %d bytes.\n", fromDiskCount, dur, float64(fromDiskCount)/(float64(dur)/float64(time.Second)), causedChangeCount, stats.Values, stats.ValueBytes)
	}
	return nil
}