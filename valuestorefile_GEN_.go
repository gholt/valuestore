package store

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"path"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spaolacci/murmur3"
	"gopkg.in/gholt/brimutil.v1"
)

//    "VALUESTORETOC v0            ":28, checksumInterval:4
// or "VALUESTORE v0               ":28, checksumInterval:4
const _VALUE_FILE_HEADER_SIZE = 32

// keyA:8, keyB:8, timestampbits:8, offset:4, length:4
const _VALUE_FILE_ENTRY_SIZE = 32

// "TERM v0 ":8
const _VALUE_FILE_TRAILER_SIZE = 8

type valueStoreFile struct {
	store                     *DefaultValueStore
	name                      string
	id                        uint32
	nameTimestamp             int64
	readerFPs                 []brimutil.ChecksummedReader
	readerLocks               []sync.Mutex
	readerLens                [][]byte
	writerFP                  io.WriteCloser
	writerOffset              uint32
	writerFreeBufChan         chan *valueStoreFileWriteBuf
	writerChecksumBufChan     chan *valueStoreFileWriteBuf
	writerToDiskBufChan       chan *valueStoreFileWriteBuf
	writerDoneChan            chan struct{}
	writerCurrentBuf          *valueStoreFileWriteBuf
	freeableMemBlockChanIndex int
}

type valueStoreFileWriteBuf struct {
	seq       int
	buf       []byte
	offset    uint32
	memBlocks []*valueMemBlock
}

func newValueReadFile(store *DefaultValueStore, nameTimestamp int64, openReadSeeker func(name string) (io.ReadSeeker, error)) (*valueStoreFile, error) {
	fl := &valueStoreFile{store: store, nameTimestamp: nameTimestamp}
	fl.name = path.Join(store.path, fmt.Sprintf("%019d.value", fl.nameTimestamp))
	fl.readerFPs = make([]brimutil.ChecksummedReader, store.fileReaders)
	fl.readerLocks = make([]sync.Mutex, len(fl.readerFPs))
	fl.readerLens = make([][]byte, len(fl.readerFPs))
	var checksumInterval uint32
	for i := 0; i < len(fl.readerFPs); i++ {
		fp, err := openReadSeeker(fl.name)
		if err != nil {
			return nil, err
		}
		if i == 0 {
			if checksumInterval, err = readValueHeader(fp); err != nil {
				return nil, err
			}
		}
		fl.readerFPs[i] = brimutil.NewChecksummedReader(fp, int(checksumInterval), murmur3.New32)
		fl.readerLens[i] = make([]byte, 4)
	}
	var err error
	fl.id, err = store.addLocBlock(fl)
	if err != nil {
		fl.close()
		return nil, err
	}
	return fl, nil
}

func createValueReadWriteFile(store *DefaultValueStore, createWriteCloser func(name string) (io.WriteCloser, error), openReadSeeker func(name string) (io.ReadSeeker, error)) (*valueStoreFile, error) {
	fl := &valueStoreFile{store: store, nameTimestamp: time.Now().UnixNano()}
	fl.name = path.Join(store.path, fmt.Sprintf("%019d.value", fl.nameTimestamp))
	fp, err := createWriteCloser(fl.name)
	if err != nil {
		return nil, err
	}
	fl.writerFP = fp
	fl.writerFreeBufChan = make(chan *valueStoreFileWriteBuf, store.workers)
	for i := 0; i < store.workers; i++ {
		fl.writerFreeBufChan <- &valueStoreFileWriteBuf{buf: make([]byte, store.checksumInterval+4)}
	}
	fl.writerChecksumBufChan = make(chan *valueStoreFileWriteBuf, store.workers)
	fl.writerToDiskBufChan = make(chan *valueStoreFileWriteBuf, store.workers)
	fl.writerDoneChan = make(chan struct{})
	fl.writerCurrentBuf = <-fl.writerFreeBufChan
	head := []byte("VALUESTORE v0                   ")
	binary.BigEndian.PutUint32(head[28:], store.checksumInterval)
	fl.writerCurrentBuf.offset = uint32(copy(fl.writerCurrentBuf.buf, head))
	atomic.StoreUint32(&fl.writerOffset, fl.writerCurrentBuf.offset)
	go fl.writer()
	for i := 0; i < store.workers; i++ {
		go fl.writingChecksummer()
	}
	fl.readerFPs = make([]brimutil.ChecksummedReader, store.fileReaders)
	fl.readerLocks = make([]sync.Mutex, len(fl.readerFPs))
	fl.readerLens = make([][]byte, len(fl.readerFPs))
	for i := 0; i < len(fl.readerFPs); i++ {
		fp, err := openReadSeeker(fl.name)
		if err != nil {
			fl.writerFP.Close()
			for j := 0; j < i; j++ {
				fl.readerFPs[j].Close()
			}
			return nil, err
		}
		fl.readerFPs[i] = brimutil.NewChecksummedReader(fp, int(store.checksumInterval), murmur3.New32)
		fl.readerLens[i] = make([]byte, 4)
	}
	fl.id, err = store.addLocBlock(fl)
	if err != nil {
		return nil, err
	}
	return fl, nil
}

func (fl *valueStoreFile) timestampnano() int64 {
	return fl.nameTimestamp
}

func (fl *valueStoreFile) read(keyA uint64, keyB uint64, timestampbits uint64, offset uint32, length uint32, value []byte) (uint64, []byte, error) {
	if timestampbits&_TSB_DELETION != 0 {
		return timestampbits, value, ErrNotFound
	}
	i := int(keyA>>1) % len(fl.readerFPs)
	fl.readerLocks[i].Lock()
	fl.readerFPs[i].Seek(int64(offset), 0)
	end := len(value) + int(length)
	if end <= cap(value) {
		value = value[:end]
	} else {
		value2 := make([]byte, end)
		copy(value2, value)
		value = value2
	}
	if _, err := io.ReadFull(fl.readerFPs[i], value[len(value)-int(length):]); err != nil {
		fl.readerLocks[i].Unlock()
		return timestampbits, value, err
	}
	fl.readerLocks[i].Unlock()
	return timestampbits, value, nil
}

func (fl *valueStoreFile) write(memBlock *valueMemBlock) {
	if memBlock == nil {
		return
	}
	memBlock.fileID = fl.id
	memBlock.fileOffset = atomic.LoadUint32(&fl.writerOffset)
	if len(memBlock.values) < 1 {
		fl.store.freeableMemBlockChans[fl.freeableMemBlockChanIndex] <- memBlock
		fl.freeableMemBlockChanIndex++
		if fl.freeableMemBlockChanIndex >= len(fl.store.freeableMemBlockChans) {
			fl.freeableMemBlockChanIndex = 0
		}
		return
	}
	left := len(memBlock.values)
	for left > 0 {
		n := copy(fl.writerCurrentBuf.buf[fl.writerCurrentBuf.offset:fl.store.checksumInterval], memBlock.values[len(memBlock.values)-left:])
		atomic.AddUint32(&fl.writerOffset, uint32(n))
		fl.writerCurrentBuf.offset += uint32(n)
		if fl.writerCurrentBuf.offset >= fl.store.checksumInterval {
			s := fl.writerCurrentBuf.seq
			fl.writerChecksumBufChan <- fl.writerCurrentBuf
			fl.writerCurrentBuf = <-fl.writerFreeBufChan
			fl.writerCurrentBuf.seq = s + 1
		}
		left -= n
	}
	if fl.writerCurrentBuf.offset == 0 {
		fl.store.freeableMemBlockChans[fl.freeableMemBlockChanIndex] <- memBlock
		fl.freeableMemBlockChanIndex++
		if fl.freeableMemBlockChanIndex >= len(fl.store.freeableMemBlockChans) {
			fl.freeableMemBlockChanIndex = 0
		}
	} else {
		fl.writerCurrentBuf.memBlocks = append(fl.writerCurrentBuf.memBlocks, memBlock)
	}
}

func (fl *valueStoreFile) closeWriting() error {
	if fl.writerChecksumBufChan == nil {
		return nil
	}
	var reterr error
	close(fl.writerChecksumBufChan)
	for i := 0; i < cap(fl.writerChecksumBufChan); i++ {
		<-fl.writerDoneChan
	}
	fl.writerToDiskBufChan <- nil
	<-fl.writerDoneChan
	term := []byte("TERM v0 ")
	left := len(term)
	for left > 0 {
		n := copy(fl.writerCurrentBuf.buf[fl.writerCurrentBuf.offset:fl.store.checksumInterval], term[len(term)-left:])
		left -= n
		fl.writerCurrentBuf.offset += uint32(n)
		if left > 0 {
			binary.BigEndian.PutUint32(fl.writerCurrentBuf.buf[fl.writerCurrentBuf.offset:], murmur3.Sum32(fl.writerCurrentBuf.buf[:fl.writerCurrentBuf.offset]))
			fl.writerCurrentBuf.offset += 4
		}
		if _, err := fl.writerFP.Write(fl.writerCurrentBuf.buf[:fl.writerCurrentBuf.offset]); err != nil {
			if reterr == nil {
				reterr = err
			}
			break
		}
		fl.writerCurrentBuf.offset = 0
	}
	if err := fl.writerFP.Close(); err != nil {
		if reterr == nil {
			reterr = err
		}
	}
	for _, memBlock := range fl.writerCurrentBuf.memBlocks {
		fl.store.freeableMemBlockChans[fl.freeableMemBlockChanIndex] <- memBlock
		fl.freeableMemBlockChanIndex++
		if fl.freeableMemBlockChanIndex >= len(fl.store.freeableMemBlockChans) {
			fl.freeableMemBlockChanIndex = 0
		}
	}
	fl.writerFP = nil
	fl.writerFreeBufChan = nil
	fl.writerChecksumBufChan = nil
	fl.writerToDiskBufChan = nil
	fl.writerDoneChan = nil
	fl.writerCurrentBuf = nil
	return reterr
}

func (fl *valueStoreFile) close() error {
	reterr := fl.closeWriting()
	for i, fp := range fl.readerFPs {
		// This will let any ongoing reads complete.
		fl.readerLocks[i].Lock()
		if err := fp.Close(); err != nil {
			if reterr == nil {
				reterr = err
			}
		}
		// This will release any pending reads, which will get errors
		// immediately. Essentially, there is a race between compaction
		// accomplishing its goal of rewriting all entries of a file to a new
		// file, and readers of those entries beginning to use the new entry
		// locations. It's a small window and the resulting errors should be
		// fairly few and easily recoverable on a re-read.
		fl.readerLocks[i].Unlock()
	}
	return reterr
}

func (fl *valueStoreFile) writingChecksummer() {
	for {
		buf := <-fl.writerChecksumBufChan
		if buf == nil {
			break
		}
		binary.BigEndian.PutUint32(buf.buf[fl.store.checksumInterval:], murmur3.Sum32(buf.buf[:fl.store.checksumInterval]))
		fl.writerToDiskBufChan <- buf
	}
	fl.writerDoneChan <- struct{}{}
}

func (fl *valueStoreFile) writer() {
	var seq int
	lastWasNil := false
	for {
		buf := <-fl.writerToDiskBufChan
		if buf == nil {
			if lastWasNil {
				break
			}
			lastWasNil = true
			fl.writerToDiskBufChan <- nil
			continue
		}
		lastWasNil = false
		if buf.seq != seq {
			fl.writerToDiskBufChan <- buf
			continue
		}
		if _, err := fl.writerFP.Write(buf.buf); err != nil {
			fl.store.logCritical("%s %s\n", fl.name, err)
			break
		}
		if len(buf.memBlocks) > 0 {
			for _, memBlock := range buf.memBlocks {
				fl.store.freeableMemBlockChans[fl.freeableMemBlockChanIndex] <- memBlock
				fl.freeableMemBlockChanIndex++
				if fl.freeableMemBlockChanIndex >= len(fl.store.freeableMemBlockChans) {
					fl.freeableMemBlockChanIndex = 0
				}
			}
			buf.memBlocks = buf.memBlocks[:0]
		}
		buf.offset = 0
		fl.writerFreeBufChan <- buf
		seq++
	}
	fl.writerDoneChan <- struct{}{}
}

// Returns the checksum interval stored in the header for a value file or any
// error discovered; fpr is assumed to be at file position 0.
func readValueHeader(fpr io.ReadSeeker) (uint32, error) {
	return _readValueHeader(fpr, false)
}

// Returns the checksum interval stored in the header for a TOC file or any
// error discovered; fpr is assumed to be at file position 0.
func readValueHeaderTOC(fpr io.ReadSeeker) (uint32, error) {
	return _readValueHeader(fpr, true)
}

func _readValueHeader(fpr io.ReadSeeker, toc bool) (uint32, error) {
	buf := make([]byte, _VALUE_FILE_HEADER_SIZE)
	if _, err := io.ReadFull(fpr, buf); err != nil {
		return 0, err
	}
	var cmp []byte
	if toc {
		cmp = []byte("VALUESTORETOC v0            ")
	} else {
		cmp = []byte("VALUESTORE v0               ")
	}
	if !bytes.Equal(buf[:28], cmp) {
		return 0, errors.New("unknown file type in header")
	}
	checksumInterval := binary.BigEndian.Uint32(buf[28:])
	if checksumInterval < _VALUE_FILE_HEADER_SIZE {
		return 0, fmt.Errorf("checksum interval is too small %d", checksumInterval)
	}
	return checksumInterval, nil
}

type valueTOCEntry struct {
	KeyA uint64
	KeyB uint64

	TimestampBits uint64
	BlockID       uint32
	Offset        uint32
	Length        uint32
}

func valueReadTOCEntriesBatched(fpr io.ReadSeeker, blockID uint32, freeBatchChans []chan []valueTOCEntry, pendingBatchChans []chan []valueTOCEntry) []error {
	// There is an assumption that the checksum interval is greater than the
	// _VALUE_FILE_HEADER_SIZE and that the _VALUE_FILE_ENTRY_SIZE is
	// greater than the _VALUE_FILE_TRAILER_SIZE.
	var errs []error
	var checksumInterval int
	if ci, err := readValueHeaderTOC(fpr); err != nil {
		return append(errs, err)
	} else {
		checksumInterval = int(ci)
	}
	fpr.Seek(0, 0)
	buf := make([]byte, checksumInterval+4+_VALUE_FILE_ENTRY_SIZE)
	first := true
	rpos := 0
	checksumErrors := 0
	workers := uint64(len(freeBatchChans))
	batches := make([][]valueTOCEntry, workers)
	batches[0] = <-freeBatchChans[0]
	batchSize := len(batches[0])
	batchesPos := make([]int, len(batches))
	more := true
	for more {
		rbuf := buf[rpos : rpos+checksumInterval+4]
		if n, err := io.ReadFull(fpr, rbuf); err == io.ErrUnexpectedEOF || err == io.EOF {
			rbuf = rbuf[:n]
			more = false
		} else if err != nil {
			errs = append(errs, err)
			break
		} else {
			cbuf := rbuf[len(rbuf)-4:]
			rbuf = rbuf[:len(rbuf)-4]
			if binary.BigEndian.Uint32(cbuf) != murmur3.Sum32(rbuf) {
				checksumErrors++
				// TODO: Have to realign here
			}
		}
		if first {
			rbuf = rbuf[_VALUE_FILE_HEADER_SIZE:]
			first = false
		} else {
			rbuf = buf[:rpos+len(rbuf)]
		}
		if !more {
			if bytes.Equal(rbuf[len(rbuf)-_VALUE_FILE_TRAILER_SIZE:], []byte("TERM v0 ")) {
				rbuf = rbuf[:len(rbuf)-_VALUE_FILE_TRAILER_SIZE]
			} else {
				errs = append(errs, errors.New("no terminator found"))
			}
		}
		for len(rbuf) >= _VALUE_FILE_ENTRY_SIZE {
			keyB := binary.BigEndian.Uint64(rbuf[8:])
			k := keyB % workers
			if batches[k] == nil {
				batches[k] = <-freeBatchChans[k]
				batchesPos[k] = 0
			}
			wr := &batches[k][batchesPos[k]]

			wr.KeyA = binary.BigEndian.Uint64(rbuf)
			wr.KeyB = keyB
			wr.TimestampBits = binary.BigEndian.Uint64(rbuf[16:])
			wr.BlockID = blockID
			wr.Offset = binary.BigEndian.Uint32(rbuf[24:])
			wr.Length = binary.BigEndian.Uint32(rbuf[28:])

			batchesPos[k]++
			if batchesPos[k] >= batchSize {
				pendingBatchChans[k] <- batches[k]
				batches[k] = nil
			}
			rbuf = rbuf[_VALUE_FILE_ENTRY_SIZE:]
		}
		rpos = copy(buf, rbuf)
	}
	for i := 0; i < len(batches); i++ {
		if batches[i] != nil {
			pendingBatchChans[i] <- batches[i][:batchesPos[i]]
		}
	}
	if checksumErrors > 0 {
		errs = append(errs, fmt.Errorf("there were %d checksum errors", checksumErrors))
	}
	return errs
}
