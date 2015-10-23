package valuestore

import (
	"encoding/binary"
	"io"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/gholt/brimtime.v1"
)

const _GROUP_PULL_REPLICATION_MSG_TYPE = 0x34bf87953e59e8d1

const _GROUP_PULL_REPLICATION_MSG_HEADER_BYTES = 44

type groupPullReplicationState struct {
	inWorkers            int
	inMsgChan            chan *groupPullReplicationMsg
	inFreeMsgChan        chan *groupPullReplicationMsg
	inResponseMsgTimeout time.Duration
	outWorkers           uint64
	outInterval          time.Duration
	outNotifyChan        chan *backgroundNotification
	outIteration         uint16
	outAbort             uint32
	outMsgChan           chan *groupPullReplicationMsg
	outKTBFs             []*groupKTBloomFilter
	outMsgTimeout        time.Duration
	bloomN               uint64
	bloomP               float64
}

type groupPullReplicationMsg struct {
	vs     *DefaultGroupStore
	header []byte
	body   []byte
}

func (vs *DefaultGroupStore) pullReplicationConfig(cfg *GroupStoreConfig) {
	vs.pullReplicationState.outInterval = time.Duration(cfg.OutPullReplicationInterval) * time.Second
	vs.pullReplicationState.outNotifyChan = make(chan *backgroundNotification, 1)
	vs.pullReplicationState.outWorkers = uint64(cfg.OutPullReplicationWorkers)
	vs.pullReplicationState.outIteration = uint16(cfg.Rand.Uint32())
	if vs.msgRing != nil {
		vs.msgRing.SetMsgHandler(_GROUP_PULL_REPLICATION_MSG_TYPE, vs.newInPullReplicationMsg)
		vs.pullReplicationState.inMsgChan = make(chan *groupPullReplicationMsg, cfg.InPullReplicationMsgs)
		vs.pullReplicationState.inFreeMsgChan = make(chan *groupPullReplicationMsg, cfg.InPullReplicationMsgs)
		for i := 0; i < cap(vs.pullReplicationState.inFreeMsgChan); i++ {
			vs.pullReplicationState.inFreeMsgChan <- &groupPullReplicationMsg{
				vs:     vs,
				header: make([]byte, _GROUP_KT_BLOOM_FILTER_HEADER_BYTES+_GROUP_PULL_REPLICATION_MSG_HEADER_BYTES),
			}
		}
		vs.pullReplicationState.inWorkers = cfg.InPullReplicationWorkers
		vs.pullReplicationState.outMsgChan = make(chan *groupPullReplicationMsg, cfg.OutPullReplicationMsgs)
		vs.pullReplicationState.bloomN = uint64(cfg.OutPullReplicationBloomN)
		vs.pullReplicationState.bloomP = cfg.OutPullReplicationBloomP
		vs.pullReplicationState.outKTBFs = []*groupKTBloomFilter{newGroupKTBloomFilter(vs.pullReplicationState.bloomN, vs.pullReplicationState.bloomP, 0)}
		for i := 0; i < cap(vs.pullReplicationState.outMsgChan); i++ {
			vs.pullReplicationState.outMsgChan <- &groupPullReplicationMsg{
				vs:     vs,
				header: make([]byte, _GROUP_KT_BLOOM_FILTER_HEADER_BYTES+_GROUP_PULL_REPLICATION_MSG_HEADER_BYTES),
				body:   make([]byte, len(vs.pullReplicationState.outKTBFs[0].bits)),
			}
		}
		vs.pullReplicationState.inResponseMsgTimeout = time.Duration(cfg.InPullReplicationResponseMsgTimeout) * time.Millisecond
		vs.pullReplicationState.outMsgTimeout = time.Duration(cfg.OutPullReplicationMsgTimeout) * time.Millisecond
	}
	vs.pullReplicationState.outNotifyChan = make(chan *backgroundNotification, 1)
}

func (vs *DefaultGroupStore) pullReplicationLaunch() {
	for i := 0; i < vs.pullReplicationState.inWorkers; i++ {
		go vs.inPullReplication()
	}
	go vs.outPullReplicationLauncher()
}

// DisableOutPullReplication will stop any outgoing pull replication requests
// until EnableOutPullReplication is called.
func (vs *DefaultGroupStore) DisableOutPullReplication() {
	c := make(chan struct{}, 1)
	vs.pullReplicationState.outNotifyChan <- &backgroundNotification{
		disable:  true,
		doneChan: c,
	}
	<-c
}

// EnableOutPullReplication will resume outgoing pull replication requests.
func (vs *DefaultGroupStore) EnableOutPullReplication() {
	c := make(chan struct{}, 1)
	vs.pullReplicationState.outNotifyChan <- &backgroundNotification{
		enable:   true,
		doneChan: c,
	}
	<-c
}

// newInPullReplicationMsg reads pull-replication messages from the MsgRing and
// puts them on the inMsgChan for the inPullReplication workers to work on.
func (vs *DefaultGroupStore) newInPullReplicationMsg(r io.Reader, l uint64) (uint64, error) {
	var prm *groupPullReplicationMsg
	select {
	case prm = <-vs.pullReplicationState.inFreeMsgChan:
	default:
		// If there isn't a free groupPullReplicationMsg, just read and discard the
		// incoming pull-replication message.
		left := l
		var sn int
		var err error
		for left > 0 {
			t := toss
			if left < uint64(len(t)) {
				t = t[:left]
			}
			sn, err = r.Read(t)
			left -= uint64(sn)
			if err != nil {
				atomic.AddInt32(&vs.inPullReplicationInvalids, 1)
				return l - left, err
			}
		}
		atomic.AddInt32(&vs.inPullReplicationDrops, 1)
		return l, nil
	}
	// TODO: We need to cap this so memory isn't abused in case someone
	// accidentally sets a crazy sized bloom filter on another node. Since a
	// partial pull-replication message is pretty much useless as it would drop
	// a chunk of the bloom filter bitspace, we should drop oversized messages
	// but report the issue.
	bl := l - _GROUP_PULL_REPLICATION_MSG_HEADER_BYTES - uint64(_GROUP_KT_BLOOM_FILTER_HEADER_BYTES)
	if uint64(cap(prm.body)) < bl {
		prm.body = make([]byte, bl)
	}
	prm.body = prm.body[:bl]
	var n int
	var sn int
	var err error
	for n != len(prm.header) {
		if err != nil {
			vs.pullReplicationState.inFreeMsgChan <- prm
			atomic.AddInt32(&vs.inPullReplicationInvalids, 1)
			return uint64(n), err
		}
		sn, err = r.Read(prm.header[n:])
		n += sn
	}
	n = 0
	for n != len(prm.body) {
		if err != nil {
			vs.pullReplicationState.inFreeMsgChan <- prm
			atomic.AddInt32(&vs.inPullReplicationInvalids, 1)
			return uint64(len(prm.header)) + uint64(n), err
		}
		sn, err = r.Read(prm.body[n:])
		n += sn
	}
	vs.pullReplicationState.inMsgChan <- prm
	atomic.AddInt32(&vs.inPullReplications, 1)
	return l, nil
}

// inPullReplication actually processes incoming pull-replication messages;
// there may be more than one of these workers.
func (vs *DefaultGroupStore) inPullReplication() {
	k := make([]uint64, vs.bulkSetState.msgCap/_GROUP_BULK_SET_MSG_MIN_ENTRY_LENGTH*4)
	v := make([]byte, vs.valueCap)
	for {
		prm := <-vs.pullReplicationState.inMsgChan
		if prm == nil {
			break
		}
		ring := vs.msgRing.Ring()
		if ring == nil {
			vs.pullReplicationState.inFreeMsgChan <- prm
			continue
		}
		k = k[:0]
		// This is what the remote system used when making its bloom filter,
		// computed via its config.ReplicationIgnoreRecent setting. We want to
		// use the exact same cutoff in our checks and possible response.
		cutoff := prm.cutoff()
		tombstoneCutoff := (uint64(brimtime.TimeToUnixMicro(time.Now())) << _TSB_UTIL_BITS) - vs.tombstoneDiscardState.age
		ktbf := prm.ktBloomFilter()
		l := int64(vs.bulkSetState.msgCap)
		callback := func(keyA uint64, keyB uint64, nameKeyA uint64, nameKeyB uint64, timestampbits uint64, length uint32) bool {
			if timestampbits&_TSB_DELETION == 0 || timestampbits >= tombstoneCutoff {
				if !ktbf.mayHave(keyA, keyB, nameKeyA, nameKeyB, timestampbits) {
					k = append(k, keyA, keyB, nameKeyA, nameKeyB)
					l -= _GROUP_BULK_SET_MSG_ENTRY_HEADER_LENGTH + int64(length)
					if l <= 0 {
						return false
					}
				}
			}
			return true
		}
		// Based on the replica index for the local node, start the scan at
		// different points. For example, in a three replica system the first
		// replica would start scanning at the start, the second a third
		// through, the last would start two thirds through. This is so that
		// pull-replication messages, which are sent concurrently to all other
		// replicas, will get different responses back instead of duplicate
		// items if there is a lot of data to be sent.
		scanStart := prm.rangeStart() + (prm.rangeStop()-prm.rangeStart())/uint64(ring.ReplicaCount())*uint64(ring.ResponsibleReplica(uint32(prm.rangeStart()>>(64-ring.PartitionBitCount()))))
		scanStop := prm.rangeStop()
		vs.vlm.ScanCallback(scanStart, scanStop, 0, _TSB_LOCAL_REMOVAL, cutoff, math.MaxUint64, callback)
		if l > 0 {
			scanStop = scanStart - 1
			scanStart = prm.rangeStart()
			vs.vlm.ScanCallback(scanStart, scanStop, 0, _TSB_LOCAL_REMOVAL, cutoff, math.MaxUint64, callback)
		}
		nodeID := prm.nodeID()
		vs.pullReplicationState.inFreeMsgChan <- prm
		if len(k) > 0 {
			bsm := vs.newOutBulkSetMsg()
			// Indicate that a response to this bulk-set message is not
			// necessary. If the message fails to reach its destination, that
			// destination will simply resend another pull replication message
			// on its next pass.
			binary.BigEndian.PutUint64(bsm.header, 0)
			var t uint64
			var err error
			for i := 0; i < len(k); i += 4 {
				t, v, err = vs.read(k[i], k[i+1], k[i+2], k[i+3], v[:0])
				if err == ErrNotFound {
					if t == 0 {
						continue
					}
				} else if err != nil {
					continue
				}
				if t&_TSB_LOCAL_REMOVAL == 0 {
					if !bsm.add(k[i], k[i+1], k[i+2], k[i+3], t, v) {
						break
					}
					atomic.AddInt32(&vs.outBulkSetValues, 1)
				}
			}
			if len(bsm.body) > 0 {
				atomic.AddInt32(&vs.outBulkSets, 1)
				vs.msgRing.MsgToNode(bsm, nodeID, vs.pullReplicationState.inResponseMsgTimeout)
			}
		}
	}
}

// OutPullReplicationPass will immediately execute an outgoing pull replication
// pass rather than waiting for the next interval. If a pass is currently
// executing, it will be stopped and restarted so that a call to this function
// ensures one complete pass occurs. Note that this pass will send the outgoing
// pull replication requests, but all the responses will almost certainly not
// have been received when this function returns. These requests are stateless,
// and so synchronization at that level is not possible.
func (vs *DefaultGroupStore) OutPullReplicationPass() {
	atomic.StoreUint32(&vs.pullReplicationState.outAbort, 1)
	c := make(chan struct{}, 1)
	vs.pullReplicationState.outNotifyChan <- &backgroundNotification{doneChan: c}
	<-c
}

func (vs *DefaultGroupStore) outPullReplicationLauncher() {
	var enabled bool
	interval := float64(vs.pullReplicationState.outInterval)
	vs.randMutex.Lock()
	nextRun := time.Now().Add(time.Duration(interval + interval*vs.rand.NormFloat64()*0.1))
	vs.randMutex.Unlock()
	for {
		var notification *backgroundNotification
		sleep := nextRun.Sub(time.Now())
		if sleep > 0 {
			select {
			case notification = <-vs.pullReplicationState.outNotifyChan:
			case <-time.After(sleep):
			}
		} else {
			select {
			case notification = <-vs.pullReplicationState.outNotifyChan:
			default:
			}
		}
		vs.randMutex.Lock()
		nextRun = time.Now().Add(time.Duration(interval + interval*vs.rand.NormFloat64()*0.1))
		vs.randMutex.Unlock()
		if notification != nil {
			if notification.enable {
				enabled = true
				notification.doneChan <- struct{}{}
				continue
			}
			if notification.disable {
				atomic.StoreUint32(&vs.pullReplicationState.outAbort, 1)
				enabled = false
				notification.doneChan <- struct{}{}
				continue
			}
			atomic.StoreUint32(&vs.pullReplicationState.outAbort, 0)
			vs.outPullReplicationPass()
			notification.doneChan <- struct{}{}
		} else if enabled {
			atomic.StoreUint32(&vs.pullReplicationState.outAbort, 0)
			vs.outPullReplicationPass()
		}
	}
}

func (vs *DefaultGroupStore) outPullReplicationPass() {
	if vs.msgRing == nil {
		return
	}
	if vs.logDebug != nil {
		begin := time.Now()
		defer func() {
			vs.logDebug("out pull replication pass took %s\n", time.Now().Sub(begin))
		}()
	}
	ring := vs.msgRing.Ring()
	if ring == nil {
		return
	}
	rightwardPartitionShift := 64 - ring.PartitionBitCount()
	partitionCount := uint64(1) << ring.PartitionBitCount()
	if vs.pullReplicationState.outIteration == math.MaxUint16 {
		vs.pullReplicationState.outIteration = 0
	} else {
		vs.pullReplicationState.outIteration++
	}
	ringVersion := ring.Version()
	ws := vs.pullReplicationState.outWorkers
	for uint64(len(vs.pullReplicationState.outKTBFs)) < ws {
		vs.pullReplicationState.outKTBFs = append(vs.pullReplicationState.outKTBFs, newGroupKTBloomFilter(vs.pullReplicationState.bloomN, vs.pullReplicationState.bloomP, 0))
	}
	f := func(p uint64, w uint64, ktbf *groupKTBloomFilter) {
		pb := p << rightwardPartitionShift
		rb := pb + ((uint64(1) << rightwardPartitionShift) / ws * w)
		var re uint64
		if w+1 == ws {
			if p+1 == partitionCount {
				re = math.MaxUint64
			} else {
				re = ((p + 1) << rightwardPartitionShift) - 1
			}
		} else {
			re = pb + ((uint64(1) << rightwardPartitionShift) / ws * (w + 1)) - 1
		}
		timestampbitsnow := uint64(brimtime.TimeToUnixMicro(time.Now())) << _TSB_UTIL_BITS
		cutoff := timestampbitsnow - vs.replicationIgnoreRecent
		var more bool
		for {
			rbThis := rb
			ktbf.reset(vs.pullReplicationState.outIteration)
			rb, more = vs.vlm.ScanCallback(rb, re, 0, _TSB_LOCAL_REMOVAL, cutoff, vs.pullReplicationState.bloomN, func(keyA uint64, keyB uint64, nameKeyA uint64, nameKeyB uint64, timestampbits uint64, length uint32) bool {
				ktbf.add(keyA, keyB, nameKeyA, nameKeyB, timestampbits)
				return true
			})
			if atomic.LoadUint32(&vs.pullReplicationState.outAbort) != 0 {
				break
			}
			ring2 := vs.msgRing.Ring()
			if ring2 == nil || ring2.Version() != ringVersion {
				break
			}
			reThis := re
			if more {
				reThis = rb - 1
			}
			prm := vs.newOutPullReplicationMsg(ringVersion, uint32(p), cutoff, rbThis, reThis, ktbf)
			atomic.AddInt32(&vs.outPullReplications, 1)
			vs.msgRing.MsgToOtherReplicas(prm, uint32(p), vs.pullReplicationState.outMsgTimeout)
			if !more {
				break
			}
		}
	}
	wg := &sync.WaitGroup{}
	wg.Add(int(ws))
	for w := uint64(0); w < ws; w++ {
		go func(w uint64) {
			ktbf := vs.pullReplicationState.outKTBFs[w]
			pb := partitionCount / ws * w
			for p := pb; p < partitionCount; p++ {
				if atomic.LoadUint32(&vs.pullReplicationState.outAbort) != 0 {
					break
				}
				ring2 := vs.msgRing.Ring()
				if ring2 == nil || ring2.Version() != ringVersion {
					break
				}
				if ring.Responsible(uint32(p)) {
					f(p, w, ktbf)
				}
			}
			for p := uint64(0); p < pb; p++ {
				if atomic.LoadUint32(&vs.pullReplicationState.outAbort) != 0 {
					break
				}
				ring2 := vs.msgRing.Ring()
				if ring2 == nil || ring2.Version() != ringVersion {
					break
				}
				if ring.Responsible(uint32(p)) {
					f(p, w, ktbf)
				}
			}
			wg.Done()
		}(w)
	}
	wg.Wait()
}

// newOutPullReplicationMsg gives an initialized groupPullReplicationMsg for filling
// out and eventually sending using the MsgRing. The MsgRing (or someone else
// if the message doesn't end up with the MsgRing) will call
// groupPullReplicationMsg.Free() eventually and the pullReplicationMsg will be
// requeued for reuse later. There is a fixed number of outgoing
// groupPullReplicationMsg instances that can exist at any given time, capping
// memory usage. Once the limit is reached, this method will block until a
// groupPullReplicationMsg is available to return.
func (vs *DefaultGroupStore) newOutPullReplicationMsg(ringVersion int64, partition uint32, cutoff uint64, rangeStart uint64, rangeStop uint64, ktbf *groupKTBloomFilter) *groupPullReplicationMsg {
	prm := <-vs.pullReplicationState.outMsgChan
	if vs.msgRing != nil {
		if r := vs.msgRing.Ring(); r != nil {
			if n := r.LocalNode(); n != nil {
				binary.BigEndian.PutUint64(prm.header, n.ID())
			}
		}
	}
	binary.BigEndian.PutUint64(prm.header[8:], uint64(ringVersion))
	binary.BigEndian.PutUint32(prm.header[16:], partition)
	binary.BigEndian.PutUint64(prm.header[20:], cutoff)
	binary.BigEndian.PutUint64(prm.header[28:], rangeStart)
	binary.BigEndian.PutUint64(prm.header[36:], rangeStop)
	ktbf.toMsg(prm, _GROUP_PULL_REPLICATION_MSG_HEADER_BYTES)
	return prm
}

func (prm *groupPullReplicationMsg) MsgType() uint64 {
	return _GROUP_PULL_REPLICATION_MSG_TYPE
}

func (prm *groupPullReplicationMsg) MsgLength() uint64 {
	return uint64(len(prm.header)) + uint64(len(prm.body))
}

func (prm *groupPullReplicationMsg) nodeID() uint64 {
	return binary.BigEndian.Uint64(prm.header)
}

func (prm *groupPullReplicationMsg) ringVersion() int64 {
	return int64(binary.BigEndian.Uint64(prm.header[8:]))
}

func (prm *groupPullReplicationMsg) partition() uint32 {
	return binary.BigEndian.Uint32(prm.header[16:])
}

func (prm *groupPullReplicationMsg) cutoff() uint64 {
	return binary.BigEndian.Uint64(prm.header[20:])
}

func (prm *groupPullReplicationMsg) rangeStart() uint64 {
	return binary.BigEndian.Uint64(prm.header[28:])
}

func (prm *groupPullReplicationMsg) rangeStop() uint64 {
	return binary.BigEndian.Uint64(prm.header[36:])
}

func (prm *groupPullReplicationMsg) ktBloomFilter() *groupKTBloomFilter {
	return newGroupKTBloomFilterFromMsg(prm, _GROUP_PULL_REPLICATION_MSG_HEADER_BYTES)
}

func (prm *groupPullReplicationMsg) WriteContent(w io.Writer) (uint64, error) {
	var n int
	var sn int
	var err error
	sn, err = w.Write(prm.header)
	n += sn
	if err != nil {
		return uint64(n), err
	}
	sn, err = w.Write(prm.body)
	n += sn
	return uint64(n), err
}

func (prm *groupPullReplicationMsg) Free() {
	prm.vs.pullReplicationState.outMsgChan <- prm
}
