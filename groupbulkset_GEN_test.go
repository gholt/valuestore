package store

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"

	ring "github.com/gholt/devicering"
	"golang.org/x/net/context"
)

func TestGroupBulkSetReadObviouslyTooShort(t *testing.T) {
	cfg := newTestGroupStoreConfig()
	cfg.MsgRing = &msgRingPlaceholder{}
	store, _ := newTestGroupStore(cfg)
	if err := store.Startup(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer store.Shutdown(context.Background())
	store.bulkSetShutdown()
	n, err := store.newInBulkSetMsg(bytes.NewBuffer(make([]byte, 1)), 1)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal(n)
	}
	select {
	case bsm := <-store.bulkSetState.inMsgChan:
		t.Fatal(bsm)
	default:
	}
	// Once again, way too short but with an error too.
	_, err = store.newInBulkSetMsg(bytes.NewBuffer(make([]byte, 1)), 2)
	if err != io.EOF {
		t.Fatal(err)
	}
	select {
	case bsm := <-store.bulkSetState.inMsgChan:
		t.Fatal(bsm)
	default:
	}
}

func TestGroupBulkSetRead(t *testing.T) {
	cfg := newTestGroupStoreConfig()
	cfg.MsgRing = &msgRingPlaceholder{}
	store, _ := newTestGroupStore(cfg)
	if err := store.Startup(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer store.Shutdown(context.Background())
	imc := store.bulkSetState.inMsgChan
	ifmc := store.bulkSetState.inFreeMsgChan
	store.bulkSetShutdown()
	store.bulkSetState.inMsgChan = imc
	store.bulkSetState.inFreeMsgChan = ifmc
	n, err := store.newInBulkSetMsg(bytes.NewBuffer(make([]byte, 100)), 100)
	if err != nil {
		t.Fatal(err)
	}
	if n != 100 {
		t.Fatal(n)
	}
	<-store.bulkSetState.inMsgChan
	// Again, but with an error in the header.
	n, err = store.newInBulkSetMsg(bytes.NewBuffer(make([]byte, _GROUP_BULK_SET_MSG_HEADER_LENGTH-1)), 100)
	if err != io.EOF {
		t.Fatal(err)
	}
	if n != _GROUP_BULK_SET_MSG_HEADER_LENGTH-1 {
		t.Fatal(n)
	}
	select {
	case bsm := <-store.bulkSetState.inMsgChan:
		t.Fatal(bsm)
	default:
	}
	// Once again, but with an error in the body.
	n, err = store.newInBulkSetMsg(bytes.NewBuffer(make([]byte, 10)), 100)
	if err != io.EOF {
		t.Fatal(err)
	}
	if n != 10 {
		t.Fatal(n)
	}
	select {
	case bsm := <-store.bulkSetState.inMsgChan:
		t.Fatal(bsm)
	default:
	}
}

func TestGroupBulkSetReadLowSendCap(t *testing.T) {
	cfg := newTestGroupStoreConfig()
	cfg.MsgRing = &msgRingPlaceholder{}
	cfg.BulkSetMsgCap = _GROUP_BULK_SET_MSG_HEADER_LENGTH + 1
	store, _ := newTestGroupStore(cfg)
	if err := store.Startup(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer store.Shutdown(context.Background())
	imc := store.bulkSetState.inMsgChan
	ifmc := store.bulkSetState.inFreeMsgChan
	store.bulkSetShutdown()
	store.bulkSetState.inMsgChan = imc
	store.bulkSetState.inFreeMsgChan = ifmc
	n, err := store.newInBulkSetMsg(bytes.NewBuffer(make([]byte, 100)), 100)
	if err != nil {
		t.Fatal(err)
	}
	if n != 100 {
		t.Fatal(n)
	}
}

func TestGroupBulkSetMsgWithoutAck(t *testing.T) {
	b := ring.NewBuilder(64)
	n, err := b.AddNode(true, 1, nil, nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	r := b.Ring()
	r.SetLocalNode(n.ID())
	m := &msgRingPlaceholder{ring: r}
	cfg := newTestGroupStoreConfig()
	cfg.MsgRing = m
	cfg.InBulkSetWorkers = 1
	cfg.InBulkSetMsgs = 1
	store, _ := newTestGroupStore(cfg)
	if err := store.Startup(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer store.Shutdown(context.Background())
	bsm := <-store.bulkSetState.inFreeMsgChan
	bsm.body = bsm.body[:0]
	if !bsm.add(1, 2, 3, 4, 0x500, []byte("testing")) {
		t.Fatal("")
	}
	store.bulkSetState.inMsgChan <- bsm
	// only one of these, so if we get it back we know the previous data was
	// processed
	<-store.bulkSetState.inFreeMsgChan
	ts, v, err := store.Read(context.Background(), 1, 2, 3, 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ts != 5 { // the bottom 8 bits are discarded for the public Read
		t.Fatal(ts)
	}
	if string(v) != "testing" {
		t.Fatal(string(v))
	}
	m.lock.Lock()
	v2 := len(m.msgToNodeIDs)
	m.lock.Unlock()
	if v2 != 0 {
		t.Fatal(v2)
	}
}

func TestGroupBulkSetMsgWithAck(t *testing.T) {
	b := ring.NewBuilder(64)
	n, err := b.AddNode(true, 1, nil, nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	r := b.Ring()
	r.SetLocalNode(n.ID())
	m := &msgRingPlaceholder{ring: r}
	cfg := newTestGroupStoreConfig()
	cfg.MsgRing = m
	cfg.InBulkSetWorkers = 1
	cfg.InBulkSetMsgs = 1
	store, _ := newTestGroupStore(cfg)
	if err := store.Startup(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer store.Shutdown(context.Background())
	bsm := <-store.bulkSetState.inFreeMsgChan
	binary.BigEndian.PutUint64(bsm.header, 123)
	bsm.body = bsm.body[:0]
	if !bsm.add(1, 2, 3, 4, 0x500, []byte("testing")) {
		t.Fatal("")
	}
	store.bulkSetState.inMsgChan <- bsm
	// only one of these, so if we get it back we know the previous data was
	// processed
	<-store.bulkSetState.inFreeMsgChan
	ts, v, err := store.Read(context.Background(), 1, 2, 3, 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ts != 5 { // the bottom 8 bits are discarded for the public Read
		t.Fatal(ts)
	}
	if string(v) != "testing" {
		t.Fatal(string(v))
	}
	m.lock.Lock()
	v2 := len(m.msgToNodeIDs)
	m.lock.Unlock()
	if v2 != 1 {
		t.Fatal(v2)
	}
	m.lock.Lock()
	v3 := m.msgToNodeIDs[0]
	m.lock.Unlock()
	if v3 != 123 {
		t.Fatal(v3)
	}
}

func TestGroupBulkSetMsgWithoutRing(t *testing.T) {
	m := &msgRingPlaceholder{}
	cfg := newTestGroupStoreConfig()
	cfg.MsgRing = m
	cfg.InBulkSetWorkers = 1
	cfg.InBulkSetMsgs = 1
	store, _ := newTestGroupStore(cfg)
	if err := store.Startup(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer store.Shutdown(context.Background())
	bsm := <-store.bulkSetState.inFreeMsgChan
	binary.BigEndian.PutUint64(bsm.header, 123)
	bsm.body = bsm.body[:0]
	if !bsm.add(1, 2, 3, 4, 0x500, []byte("testing")) {
		t.Fatal("")
	}
	store.bulkSetState.inMsgChan <- bsm
	// only one of these, so if we get it back we know the previous data was
	// processed
	<-store.bulkSetState.inFreeMsgChan
	ts, v, err := store.Read(context.Background(), 1, 2, 3, 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ts != 5 { // the bottom 8 bits are discarded for the public Read
		t.Fatal(ts)
	}
	if string(v) != "testing" {
		t.Fatal(string(v))
	}
	m.lock.Lock()
	v2 := len(m.msgToNodeIDs)
	m.lock.Unlock()
	if v2 != 0 {
		t.Fatal(v2)
	}
}

func TestGroupBulkSetMsgOut(t *testing.T) {
	cfg := newTestGroupStoreConfig()
	cfg.MsgRing = &msgRingPlaceholder{}
	store, _ := newTestGroupStore(cfg)
	if err := store.Startup(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer store.Shutdown(context.Background())
	bsm := store.newOutBulkSetMsg()
	if bsm.MsgType() != _GROUP_BULK_SET_MSG_TYPE {
		t.Fatal(bsm.MsgType())
	}
	if bsm.MsgLength() != _GROUP_BULK_SET_MSG_HEADER_LENGTH {
		t.Fatal(bsm.MsgLength())
	}
	buf := bytes.NewBuffer(nil)
	n, err := bsm.WriteContent(buf)
	if err != nil {
		t.Fatal(err)
	}
	if n != _GROUP_BULK_SET_MSG_HEADER_LENGTH {
		t.Fatal(n)
	}
	if !bytes.Equal(buf.Bytes(), []byte{0, 0, 0, 0, 0, 0, 0, 0}) {
		t.Fatal(buf.Bytes())
	}
	bsm.Free(0, 0)
	bsm = store.newOutBulkSetMsg()
	binary.BigEndian.PutUint64(bsm.header, 12345)
	bsm.add(1, 2, 3, 4, 0x500, nil)
	bsm.add(6, 7, 8, 9, 0xa00, []byte("testing"))
	if bsm.MsgType() != _GROUP_BULK_SET_MSG_TYPE {
		t.Fatal(bsm.MsgType())
	}
	if bsm.MsgLength() != _GROUP_BULK_SET_MSG_HEADER_LENGTH+_GROUP_BULK_SET_MSG_ENTRY_HEADER_LENGTH+0+_GROUP_BULK_SET_MSG_ENTRY_HEADER_LENGTH+7 {
		t.Fatal(bsm.MsgLength())
	}
	buf = bytes.NewBuffer(nil)
	n, err = bsm.WriteContent(buf)
	if err != nil {
		t.Fatal(err)
	}
	if n != _GROUP_BULK_SET_MSG_HEADER_LENGTH+_GROUP_BULK_SET_MSG_ENTRY_HEADER_LENGTH+0+_GROUP_BULK_SET_MSG_ENTRY_HEADER_LENGTH+7 {
		t.Fatal(n)
	}
	if !bytes.Equal(buf.Bytes(), []byte{
		0, 0, 0, 0, 0, 0, 48, 57, // header
		0, 0, 0, 0, 0, 0, 0, 1, // keyA
		0, 0, 0, 0, 0, 0, 0, 2, // keyB

		0, 0, 0, 0, 0, 0, 0, 3, // childKeyA
		0, 0, 0, 0, 0, 0, 0, 4, // childKeyB

		0, 0, 0, 0, 0, 0, 5, 0, // timestamp
		0, 0, 0, 0, // length
		0, 0, 0, 0, 0, 0, 0, 6, // keyA
		0, 0, 0, 0, 0, 0, 0, 7, // keyB

		0, 0, 0, 0, 0, 0, 0, 8, // childKeyA
		0, 0, 0, 0, 0, 0, 0, 9, // childKeyB

		0, 0, 0, 0, 0, 0, 10, 0, // timestamp
		0, 0, 0, 7, // length
		116, 101, 115, 116, 105, 110, 103, // "testing"
	}) {
		t.Fatal(buf.Bytes())
	}
	bsm.Free(0, 0)
}

func TestGroupBulkSetMsgOutdefaultsToFromLocalNode(t *testing.T) {
	b := ring.NewBuilder(64)
	n, err := b.AddNode(true, 1, nil, nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	r := b.Ring()
	r.SetLocalNode(n.ID())
	cfg := newTestGroupStoreConfig()
	cfg.MsgRing = &msgRingPlaceholder{ring: r}
	store, _ := newTestGroupStore(cfg)
	if err := store.Startup(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer store.Shutdown(context.Background())
	bsm := store.newOutBulkSetMsg()
	if binary.BigEndian.Uint64(bsm.header) != n.ID() {
		t.Fatal(bsm)
	}
}

func TestGroupBulkSetMsgOutWriteError(t *testing.T) {
	cfg := newTestGroupStoreConfig()
	cfg.MsgRing = &msgRingPlaceholder{}
	store, _ := newTestGroupStore(cfg)
	if err := store.Startup(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer store.Shutdown(context.Background())
	bsm := store.newOutBulkSetMsg()
	_, err := bsm.WriteContent(&testErrorWriter{})
	if err == nil {
		t.Fatal(err)
	}
	bsm.Free(0, 0)
}

func TestGroupBulkSetMsgOutHitCap(t *testing.T) {
	cfg := newTestGroupStoreConfig()
	cfg.MsgRing = &msgRingPlaceholder{}
	cfg.BulkSetMsgCap = _GROUP_BULK_SET_MSG_HEADER_LENGTH + _GROUP_BULK_SET_MSG_ENTRY_HEADER_LENGTH + 3
	store, _ := newTestGroupStore(cfg)
	if err := store.Startup(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer store.Shutdown(context.Background())
	bsm := store.newOutBulkSetMsg()
	if !bsm.add(1, 2, 3, 4, 0x500, []byte("1")) {
		t.Fatal("")
	}
	if bsm.add(6, 7, 8, 9, 0xa00, []byte("12345678901234567890")) {
		t.Fatal("")
	}
}
