package store

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/spaolacci/murmur3"
	"gopkg.in/gholt/brimutil.v1"
)

//    "VALUESTORETOC v0            ":28, checksumInterval:4
// or "VALUESTORE v0               ":28, checksumInterval:4
const _VALUE_FILE_HEADER_SIZE = 32

// keyA:8, keyB:8, timestamp:8, offset:4, length:4
const _VALUE_FILE_ENTRY_SIZE = 32

// 0:4 (reserved for versioning maybe), offsetWhereTrailerOccurs:8, "TERM":4
const _VALUE_FILE_TRAILER_SIZE = 16

type ValueDirectFile struct {
	path                string
	pathTOC             string
	openReadSeeker      func(name string) (io.ReadSeeker, error)
	openWriteSeeker     func(name string) (io.WriteSeeker, error)
	reader              brimutil.ChecksummedReader
	writer              brimutil.ChecksummedWriter
	checksumInterval    int32
	size                int64
	readerTOC           brimutil.ChecksummedReader
	writerTOC           brimutil.ChecksummedWriter
	checksumIntervalTOC int32
	sizeTOC             int64
}

func NewValueDirectFile(path string, pathTOC string, openReadSeeker func(name string) (io.ReadSeeker, error), openWriteSeeker func(name string) (io.WriteSeeker, error)) *ValueDirectFile {
	return &ValueDirectFile{
		path:            path,
		pathTOC:         pathTOC,
		openReadSeeker:  openReadSeeker,
		openWriteSeeker: openWriteSeeker,
	}
}

func (df *ValueDirectFile) Path() string {
	return df.path
}

func (df *ValueDirectFile) PathTOC() string {
	return df.pathTOC
}

func (df *ValueDirectFile) DataSize() (int64, error) {
	if df.reader == nil {
		ok, errs := df.VerifyHeaderAndTrailer()
		if !ok {
			return 0, errs[0]
		}
	}
	return df.size - _VALUE_FILE_HEADER_SIZE - _VALUE_FILE_TRAILER_SIZE, nil
}

func (df *ValueDirectFile) EntryCount() (int64, error) {
	if df.readerTOC == nil {
		ok, errs := df.VerifyHeaderAndTrailerTOC()
		if !ok {
			return 0, errs[0]
		}
	}
	return (df.sizeTOC - _VALUE_FILE_HEADER_SIZE - _VALUE_FILE_TRAILER_SIZE) / _VALUE_FILE_ENTRY_SIZE, nil
}

// VerifyHeaderAndTrailer returns true if the ValueDirectFile can continue to
// be used and a list of errors found (if any) in the header and trailer of the
// data file. Some errors result in false being returned, but some errors (such
// as those in the trailers) will allow for possible recovery of some of the
// data.
func (df *ValueDirectFile) VerifyHeaderAndTrailer() (bool, []error) {
	var errs []error
	if df.reader != nil {
		df.reader.Close()
	}
	if df.writer != nil {
		df.writer.Close()
	}
	fpr, err := df.openReadSeeker(df.path)
	if err != nil {
		return false, append(errs, err)
	}
	buf := make([]byte, _VALUE_FILE_HEADER_SIZE)
	_, err = io.ReadFull(fpr, buf)
	if err != nil {
		closeIfCloser(fpr)
		return false, append(errs, err)
	}
	if !bytes.Equal(buf[:28], []byte("VALUESTORE v0               ")) {
		closeIfCloser(fpr)
		return false, append(errs, errors.New("unknown file type in header"))
	}
	df.checksumInterval = int32(binary.BigEndian.Uint32(buf[28:]))
	if df.checksumInterval < _VALUE_FILE_HEADER_SIZE {
		closeIfCloser(fpr)
		return false, append(errs, fmt.Errorf("checksum interval is too small %d", df.checksumInterval))
	}
	df.reader = brimutil.NewChecksummedReader(fpr, int(df.checksumInterval), murmur3.New32)
	df.size, err = df.reader.Seek(-_VALUE_FILE_TRAILER_SIZE, 2)
	df.size += _VALUE_FILE_TRAILER_SIZE
	if err != nil {
		errs = append(errs, err)
		// Keep going, might be good data available
		df.size, _ = df.reader.Seek(0, 2) // Guess on df.size
	} else {
		buf = buf[:_VALUE_FILE_TRAILER_SIZE]
		_, err = io.ReadFull(df.reader, buf)
		if err != nil {
			errs = append(errs, err)
			// Keep going, might be good data available
			df.size, _ = df.reader.Seek(0, 2) // Guess on df.size
		} else {
			if !bytes.Equal(buf[:4], []byte{0, 0, 0, 0}) {
				errs = append(errs, errors.New("first four bytes of trailer are not 0s"))
				// Keep going, might be good data available
			}
			if int64(binary.BigEndian.Uint64(buf[4:])) > df.size-_VALUE_FILE_TRAILER_SIZE {
				errs = append(errs, fmt.Errorf("data ending offset recorded %d is past actual term offset %d", binary.BigEndian.Uint64(buf[4:]), df.size-_VALUE_FILE_TRAILER_SIZE))
				// Keep going, might be good data available
			}
			if !bytes.Equal(buf[12:], []byte("TERM")) {
				errs = append(errs, errors.New("last four bytes of trailer are not TERM"))
				// Keep going, might be good data available
			}
		}
	}
	fpw, err := df.openWriteSeeker(df.path)
	if err != nil {
		closeIfCloser(df.reader)
		closeIfCloser(fpr)
		return false, append(errs, err)
	}
	df.writer = brimutil.NewChecksummedWriter(fpw, int(df.checksumInterval), murmur3.New32)
	return true, errs
}

// VerifyHeaderAndTrailerTOC returns true if the ValueDirectFile can continue
// to be used and a list of errors (if any) found in the header and trailer of
// the TOC file. Some errors result in false being returned, but some errors
// (such as those in the trailer) will allow for possible recovery of some of
// the data.
func (df *ValueDirectFile) VerifyHeaderAndTrailerTOC() (bool, []error) {
	var errs []error
	if df.readerTOC != nil {
		df.readerTOC.Close()
	}
	if df.writerTOC != nil {
		df.writerTOC.Close()
	}
	fpr, err := df.openReadSeeker(df.pathTOC)
	if err != nil {
		return false, append(errs, err)
	}
	buf := make([]byte, _VALUE_FILE_HEADER_SIZE)
	_, err = io.ReadFull(fpr, buf)
	if err != nil {
		closeIfCloser(fpr)
		return false, append(errs, err)
	}
	if !bytes.Equal(buf[:28], []byte("VALUESTORETOC v0            ")) {
		closeIfCloser(fpr)
		return false, append(errs, errors.New("unknown TOC file type in header"))
	}
	df.checksumIntervalTOC = int32(binary.BigEndian.Uint32(buf[28:]))
	if df.checksumIntervalTOC < _VALUE_FILE_HEADER_SIZE || df.checksumIntervalTOC < _VALUE_FILE_TRAILER_SIZE {
		closeIfCloser(fpr)
		return false, append(errs, fmt.Errorf("TOC checksum interval is too small %d", df.checksumIntervalTOC))
	}
	df.readerTOC = brimutil.NewChecksummedReader(fpr, int(df.checksumIntervalTOC), murmur3.New32)
	df.sizeTOC, err = df.readerTOC.Seek(-_VALUE_FILE_TRAILER_SIZE, 2)
	df.sizeTOC += _VALUE_FILE_TRAILER_SIZE
	if err != nil {
		errs = append(errs, err)
		// Keep going, might be good data available
		df.sizeTOC, _ = df.readerTOC.Seek(0, 2) // Guess on df.sizeTOC
	} else {
		buf = buf[:_VALUE_FILE_TRAILER_SIZE]
		_, err = io.ReadFull(df.readerTOC, buf)
		if err != nil {
			errs = append(errs, err)
			// Keep going, might be good data available
			df.sizeTOC, _ = df.readerTOC.Seek(0, 2) // Guess on df.sizeTOC
		} else {
			if !bytes.Equal(buf[:4], []byte{0, 0, 0, 0}) {
				errs = append(errs, errors.New("first four bytes of TOC trailer are not 0s"))
				// Keep going, might be good data available
			}
			if int64(binary.BigEndian.Uint64(buf[4:])) > df.sizeTOC-_VALUE_FILE_TRAILER_SIZE {
				errs = append(errs, fmt.Errorf("TOC data ending offset recorded %d is past actual term offset %d", binary.BigEndian.Uint64(buf[4:]), df.sizeTOC-_VALUE_FILE_TRAILER_SIZE))
				// Keep going, might be good data available
			}
			if !bytes.Equal(buf[12:], []byte("TERM")) {
				errs = append(errs, errors.New("last four bytes of TOC trailer are not TERM"))
				// Keep going, might be good data available
			}
			if (df.sizeTOC-_VALUE_FILE_HEADER_SIZE-_VALUE_FILE_TRAILER_SIZE)%_VALUE_FILE_ENTRY_SIZE != 0 {
				errs = append(errs, fmt.Errorf("TOC doesn't have the right number of bytes to align to entries; off by %d bytes", (df.sizeTOC-_VALUE_FILE_HEADER_SIZE-_VALUE_FILE_TRAILER_SIZE)%_VALUE_FILE_ENTRY_SIZE))
				// Keep going, might be good data available
			}
		}
	}
	fpw, err := df.openWriteSeeker(df.path)
	if err != nil {
		closeIfCloser(df.readerTOC)
		closeIfCloser(fpr)
		return false, append(errs, err)
	}
	df.writerTOC = brimutil.NewChecksummedWriter(fpw, int(df.checksumIntervalTOC), murmur3.New32)
	return true, errs
}

func (df *ValueDirectFile) FirstEntry() (uint64, uint64, uint64, uint32, uint32, error) {
	if df.readerTOC == nil {
		ok, errs := df.VerifyHeaderAndTrailerTOC()
		if !ok {
			return 0, 0, 0, 0, 0, errs[0]
		}
	}
	if _, err := df.readerTOC.Seek(_VALUE_FILE_HEADER_SIZE, 0); err != nil {
		return 0, 0, 0, 0, 0, err
	}
	buf := make([]byte, _VALUE_FILE_ENTRY_SIZE)
	if _, err := io.ReadFull(df.readerTOC, buf); err != nil {
		return 0, 0, 0, 0, 0, err
	}

	keyA := binary.BigEndian.Uint64(buf)
	keyB := binary.BigEndian.Uint64(buf[8:])
	timestamp := binary.BigEndian.Uint64(buf[16:])
	offset := binary.BigEndian.Uint32(buf[24:])
	length := binary.BigEndian.Uint32(buf[28:])
	return keyA, keyB, timestamp, offset, length, nil

}

func (df *ValueDirectFile) NextEntry() (uint64, uint64, uint64, uint32, uint32, error) {
	if df.readerTOC == nil {
		ok, errs := df.VerifyHeaderAndTrailerTOC()
		if !ok {
			return 0, 0, 0, 0, 0, errs[0]
		}
		if _, err := df.readerTOC.Seek(_VALUE_FILE_HEADER_SIZE, 0); err != nil {
			return 0, 0, 0, 0, 0, err
		}
	}
	buf := make([]byte, _VALUE_FILE_ENTRY_SIZE)
	if _, err := io.ReadFull(df.readerTOC, buf); err != nil {
		return 0, 0, 0, 0, 0, err
	}

	keyA := binary.BigEndian.Uint64(buf)
	keyB := binary.BigEndian.Uint64(buf[8:])
	timestamp := binary.BigEndian.Uint64(buf[16:])
	offset := binary.BigEndian.Uint32(buf[24:])
	length := binary.BigEndian.Uint32(buf[28:])
	return keyA, keyB, timestamp, offset, length, nil

}