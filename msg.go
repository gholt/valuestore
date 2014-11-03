package brimstore

import (
	"io"
	"log"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

type FlushWriter interface {
	io.Writer
	Flush() error
}

type msgType uint64

const (
	_MSG_PULL_REPLICATION msgType = iota
	_MSG_BULK_SET
)

type msgUnmarshaller func(io.Reader, uint64) (uint64, error)

type msgMap struct {
	lock    sync.RWMutex
	mapping map[msgType]msgUnmarshaller
}

func newMsgMap() *msgMap {
	return &msgMap{mapping: make(map[msgType]msgUnmarshaller)}
}

func (mm *msgMap) set(t msgType, f msgUnmarshaller) msgUnmarshaller {
	mm.lock.Lock()
	p := mm.mapping[t]
	mm.mapping[t] = f
	mm.lock.Unlock()
	return p
}

func (mm *msgMap) get(t msgType) msgUnmarshaller {
	mm.lock.RLock()
	f := mm.mapping[t]
	mm.lock.RUnlock()
	return f
}

type msg interface {
	msgType() msgType
	msgLength() uint64
	writeContent(io.Writer) (uint64, error)
}

type MsgConn struct {
	closing         uint32
	conn            net.Conn
	lock            sync.RWMutex
	msgMap          *msgMap
	logError        *log.Logger
	logWarning      *log.Logger
	typeBytes       int
	lengthBytes     int
	writeChan       chan msg
	writingDoneChan chan struct{}
}

func NewMsgConn(c net.Conn) *MsgConn {
	mc := &MsgConn{
		conn:            c,
		msgMap:          newMsgMap(),
		logError:        log.New(os.Stderr, "", log.LstdFlags),
		logWarning:      log.New(os.Stderr, "", log.LstdFlags),
		typeBytes:       1,
		lengthBytes:     3,
		writeChan:       make(chan msg, 40),
		writingDoneChan: make(chan struct{}, 1),
	}
	return mc
}

func (mc *MsgConn) start() {
	go mc.reading()
	go mc.writing()
}

func (mc *MsgConn) send(m msg) {
	if atomic.LoadUint32(&mc.closing) == 0 {
		select {
		case mc.writeChan <- m:
		default:
		}
	}
}

func (mc *MsgConn) close() {
	if atomic.LoadUint32(&mc.closing) == 0 {
		atomic.StoreUint32(&mc.closing, 1)
		mc.writeChan <- nil
		<-mc.writingDoneChan
	}
}

func (mc *MsgConn) reading() {
	b := make([]byte, mc.typeBytes+mc.lengthBytes)
	d := make([]byte, 65536)
	for {
		var n int
		var sn int
		var err error
		for n != len(b) {
			if err != nil {
				if n != 0 || err != io.EOF {
					mc.logError.Print("error reading msg", err)
				}
				return
			}
			mc.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			sn, err = mc.conn.Read(b[n:])
			n += sn
		}
		if err != nil {
			mc.logError.Print("error reading msg content", err)
			return
		}
		var t msgType
		for i := 0; i < mc.typeBytes; i++ {
			t = (t << 8) | msgType(b[i])
		}
		var l uint64
		for i := 0; i < mc.lengthBytes; i++ {
			l = (l << 8) | uint64(b[mc.typeBytes+i])
		}
		f := mc.msgMap.get(t)
		if f != nil && atomic.LoadUint32(&mc.closing) == 0 {
			_, err = f(mc.conn, l)
			if err != nil {
				mc.logError.Print("error reading msg content", err)
				return
			}
		} else {
			if f == nil {
				mc.logWarning.Printf("unknown msg type %d", t)
			}
			for l > 0 {
				if err != nil {
					mc.logError.Print("err reading msg content", err)
					return
				}
				mc.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				if l >= uint64(len(d)) {
					sn, err = mc.conn.Read(d)
				} else {
					sn, err = mc.conn.Read(d[:l])
				}
				l -= uint64(sn)
			}
		}
	}
}

func (mc *MsgConn) writing() {
	b := make([]byte, mc.typeBytes+mc.lengthBytes)
	for {
		m := <-mc.writeChan
		if m == nil {
			break
		}
		if atomic.LoadUint32(&mc.closing) != 0 {
			continue
		}
		t := m.msgType()
		for i := mc.typeBytes - 1; i >= 0; i-- {
			b[i] = byte(t)
			t >>= 8
		}
		l := m.msgLength()
		for i := mc.lengthBytes - 1; i >= 0; i-- {
			b[mc.typeBytes+i] = byte(l)
			l >>= 8
		}
		mc.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_, err := mc.conn.Write(b)
		if err != nil {
			mc.logError.Print("err writing msg", err)
			break
		}
		_, err = m.writeContent(mc.conn)
		if err != nil {
			mc.logError.Print("err writing msg content", err)
			break
		}
	}
	mc.writingDoneChan <- struct{}{}
}