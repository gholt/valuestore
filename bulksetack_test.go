package valuestore

import (
	"bytes"
	"io"
	"testing"
	"time"

	"github.com/gholt/ring"
)

func TestBulkSetAckInTimeout(t *testing.T) {
	vs := New(&Config{
		MsgRing:                &msgRingPlaceholder{},
		InBulkSetAckMsgTimeout: 1,
	})
	// This means that the subsystem can never get a free bulkSetAckMsg since
	// we never feed this replacement channel.
	vs.bulkSetAckState.inFreeMsgChan = make(chan *bulkSetAckMsg, 1)
	n, err := vs.newInBulkSetAckMsg(bytes.NewBuffer(make([]byte, 100)), 100)
	// Validates we got no error and read all the bytes; meaning the message
	// was read and tossed after the timeout in getting a free bulkSetAckMsg.
	if err != nil {
		t.Fatal(err)
	}
	if n != 100 {
		t.Fatal(n)
	}
	// Try again to make sure it can handle Reader errors.
	n, err = vs.newInBulkSetAckMsg(bytes.NewBuffer(make([]byte, 10)), 100)
	if err != io.EOF {
		t.Fatal(err)
	}
	if n != 10 {
		t.Fatal(n)
	}
}

func TestBulkSetAckRead(t *testing.T) {
	vs := New(&Config{MsgRing: &msgRingPlaceholder{}})
	vs.bulkSetAckState.inMsgChan <- nil
	for len(vs.bulkSetAckState.inMsgChan) > 0 {
		time.Sleep(time.Millisecond)
	}
	n, err := vs.newInBulkSetAckMsg(bytes.NewBuffer(make([]byte, 100)), 100)
	if err != nil {
		t.Fatal(err)
	}
	if n != 100 {
		t.Fatal(n)
	}
	select {
	case <-vs.bulkSetAckState.inMsgChan:
	default:
		t.Fatal("")
	}
	// Once again, but with an error in the body.
	n, err = vs.newInBulkSetAckMsg(bytes.NewBuffer(make([]byte, 10)), 100)
	if err != io.EOF {
		t.Fatal(err)
	}
	if n != 10 {
		t.Fatal(n)
	}
	select {
	case <-vs.bulkSetAckState.inMsgChan:
		t.Fatal("")
	default:
	}
}

func TestBulkSetAckReadLowSendCap(t *testing.T) {
	vs := New(&Config{MsgRing: &msgRingPlaceholder{}, BulkSetAckMsgCap: 1})
	vs.bulkSetAckState.inMsgChan <- nil
	for len(vs.bulkSetAckState.inMsgChan) > 0 {
		time.Sleep(time.Millisecond)
	}
	n, err := vs.newInBulkSetAckMsg(bytes.NewBuffer(make([]byte, 100)), 100)
	if err != nil {
		t.Fatal(err)
	}
	if n != 100 {
		t.Fatal(n)
	}
	select {
	case <-vs.bulkSetAckState.inMsgChan:
	default:
		t.Fatal("")
	}
}

func TestBulkSetAckMsgIncoming(t *testing.T) {
	b := ring.NewBuilder()
	n := b.AddNode(true, 1, nil, nil, "", nil)
	r := b.Ring()
	r.SetLocalNode(n.ID() + 1) // so we're not responsible for anything
	m := &msgRingPlaceholder{ring: r}
	vs := New(&Config{
		MsgRing:             m,
		InBulkSetAckWorkers: 1,
		InBulkSetAckMsgs:    1,
	})
	vs.EnableAll()
	ts, err := vs.write(1, 2, 0x300, []byte("testing"))
	if err != nil {
		t.Fatal(err)
	}
	if ts != 0 {
		t.Fatal(ts)
	}
	// just double check the item is there
	ts2, v, err := vs.read(1, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ts2 != 0x300 {
		t.Fatal(ts2)
	}
	if string(v) != "testing" {
		t.Fatal(string(v))
	}
	bsam := <-vs.bulkSetAckState.inFreeMsgChan
	bsam.body = bsam.body[:0]
	if !bsam.add(1, 2, 0x300) {
		t.Fatal("")
	}
	vs.bulkSetAckState.inMsgChan <- bsam
	// only one of these, so if we get it back we know the previous data was
	// processed
	<-vs.bulkSetAckState.inFreeMsgChan
	// Make sure the item is gone
	ts2, v, err = vs.read(1, 2, nil)
	if err != ErrNotFound {
		t.Fatal(err)
	}
	if ts2 != 0x300|_TSB_LOCAL_REMOVAL {
		t.Fatal(ts2)
	}
	if string(v) != "" {
		t.Fatal(string(v))
	}
}

func TestBulkSetAckMsgIncomingNoRing(t *testing.T) {
	m := &msgRingPlaceholder{}
	vs := New(&Config{
		MsgRing:             m,
		InBulkSetAckWorkers: 1,
		InBulkSetAckMsgs:    1,
	})
	vs.EnableAll()
	ts, err := vs.write(1, 2, 0x300, []byte("testing"))
	if err != nil {
		t.Fatal(err)
	}
	if ts != 0 {
		t.Fatal(ts)
	}
	// just double check the item is there
	ts2, v, err := vs.read(1, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ts2 != 0x300 {
		t.Fatal(ts2)
	}
	if string(v) != "testing" {
		t.Fatal(string(v))
	}
	bsam := <-vs.bulkSetAckState.inFreeMsgChan
	bsam.body = bsam.body[:0]
	if !bsam.add(1, 2, 0x300) {
		t.Fatal("")
	}
	vs.bulkSetAckState.inMsgChan <- bsam
	// only one of these, so if we get it back we know the previous data was
	// processed
	<-vs.bulkSetAckState.inFreeMsgChan
	// Make sure the item is not gone since we don't know if we're responsible
	// or not since we don't have a ring
	ts2, v, err = vs.read(1, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ts2 != 0x300 {
		t.Fatal(ts2)
	}
	if string(v) != "testing" {
		t.Fatal(string(v))
	}
}

func TestBulkSetAckMsgOut(t *testing.T) {
	vs := New(&Config{MsgRing: &msgRingPlaceholder{}})
	bsam := vs.newOutBulkSetAckMsg()
	if bsam.MsgType() != _BULK_SET_ACK_MSG_TYPE {
		t.Fatal(bsam.MsgType())
	}
	if bsam.MsgLength() != 0 {
		t.Fatal(bsam.MsgLength())
	}
	buf := bytes.NewBuffer(nil)
	n, err := bsam.WriteContent(buf)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal(n)
	}
	if !bytes.Equal(buf.Bytes(), []byte{}) {
		t.Fatal(buf.Bytes())
	}
	bsam.Done()
	bsam = vs.newOutBulkSetAckMsg()
	bsam.add(1, 2, 0x300)
	bsam.add(4, 5, 0x600)
	if bsam.MsgType() != _BULK_SET_ACK_MSG_TYPE {
		t.Fatal(bsam.MsgType())
	}
	if bsam.MsgLength() != _BULK_SET_ACK_MSG_ENTRY_LENGTH+_BULK_SET_ACK_MSG_ENTRY_LENGTH {
		t.Fatal(bsam.MsgLength())
	}
	buf = bytes.NewBuffer(nil)
	n, err = bsam.WriteContent(buf)
	if err != nil {
		t.Fatal(err)
	}
	if n != _BULK_SET_ACK_MSG_ENTRY_LENGTH+_BULK_SET_ACK_MSG_ENTRY_LENGTH {
		t.Fatal(n)
	}
	if !bytes.Equal(buf.Bytes(), []byte{
		0, 0, 0, 0, 0, 0, 0, 1, // keyA
		0, 0, 0, 0, 0, 0, 0, 2, // keyB
		0, 0, 0, 0, 0, 0, 3, 0, // timestamp
		0, 0, 0, 0, 0, 0, 0, 4, // keyA
		0, 0, 0, 0, 0, 0, 0, 5, // keyB
		0, 0, 0, 0, 0, 0, 6, 0, // timestamp
	}) {
		t.Fatal(buf.Bytes())
	}
	bsam.Done()
}

func TestBulkSetAckMsgOutWriteError(t *testing.T) {
	vs := New(&Config{MsgRing: &msgRingPlaceholder{}})
	bsam := vs.newOutBulkSetAckMsg()
	bsam.add(1, 2, 0x300)
	_, err := bsam.WriteContent(&testErrorWriter{})
	if err == nil {
		t.Fatal(err)
	}
	bsam.Done()
}

func TestBulkSetAckMsgOutHitCap(t *testing.T) {
	vs := New(&Config{MsgRing: &msgRingPlaceholder{}, BulkSetAckMsgCap: _BULK_SET_ACK_MSG_ENTRY_LENGTH + 3})
	bsam := vs.newOutBulkSetAckMsg()
	if !bsam.add(1, 2, 0x300) {
		t.Fatal("")
	}
	if bsam.add(4, 5, 0x600) {
		t.Fatal("")
	}
}
