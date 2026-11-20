package ptprecv

// A buffered anchor names its sender by PTP SourceClockID. The receiver must
// keep those estimates separate: a second source on the multicast group cannot
// shift the first source's offset, and an unknown ID must not fall back to any
// aggregate or anonymous clock.

import (
	"encoding/binary"
	"testing"
	"time"

	"capsulad/apps/airplay/internal/ptp"
)

func sourceClockID(id uint64) [8]byte {
	var out [8]byte
	binary.BigEndian.PutUint64(out[:], id)
	return out
}

func syncForSource(source uint64, sequence uint16, ingress time.Time, offset time.Duration) *ptp.Message {
	master := ingress.Add(offset).UnixNano()
	return &ptp.Message{
		Header: ptp.Header{
			Type:          ptp.MsgSync,
			SourceClockID: sourceClockID(source),
			SequenceID:    sequence,
		},
		OriginTimestamp: ptp.Timestamp{
			Seconds: uint64(master / int64(time.Second)),
			Nanos:   uint32(master % int64(time.Second)),
		},
		HasOrigin: true,
	}
}

func TestSourceClockOffsetsStaySeparate(t *testing.T) {
	receiver := newReceiver(nil)
	base := time.Now().Add(-time.Second)
	const sourceA, sourceB uint64 = 0x0102030405060708, 0x1112131415161718
	const offsetA, offsetB = time.Second, 10 * time.Second

	for i := range 8 {
		ingress := base.Add(time.Duration(i) * 125 * time.Millisecond)
		// Interleave the sources, as two masters sharing the multicast group
		// would. Sequence numbers deliberately overlap.
		receiver.add(syncForSource(sourceA, uint16(i), ingress, offsetA), ingress)
		receiver.add(syncForSource(sourceB, uint16(i), ingress, offsetB), ingress)
	}

	gotA, ok := receiver.OffsetForClockID(sourceA)
	if !ok {
		t.Fatal("source A has no disciplined offset")
	}
	gotB, ok := receiver.OffsetForClockID(sourceB)
	if !ok {
		t.Fatal("source B has no disciplined offset")
	}
	if within := time.Duration(gotA - int64(offsetA)); within > time.Millisecond || within < -time.Millisecond {
		t.Fatalf("source A offset = %v, want %v", time.Duration(gotA), offsetA)
	}
	if within := time.Duration(gotB - int64(offsetB)); within > time.Millisecond || within < -time.Millisecond {
		t.Fatalf("source B offset = %v, want %v", time.Duration(gotB), offsetB)
	}
	if _, ok := receiver.OffsetForClockID(0x9999); ok {
		t.Fatal("an unknown source returned an offset")
	}
	if _, ok := receiver.OffsetForClockID(0); ok {
		t.Fatal("clock ID zero returned an anonymous offset")
	}
}

func TestSourceClockTableIsBounded(t *testing.T) {
	receiver := newReceiver(nil)
	ingress := time.Now()
	for id := uint64(1); id <= maxSourceClocks+2; id++ {
		receiver.add(syncForSource(id, 1, ingress, time.Second), ingress)
	}
	receiver.mu.Lock()
	count := len(receiver.sources)
	receiver.mu.Unlock()
	if count > maxSourceClocks {
		t.Fatalf("source table holds %d entries, cap is %d", count, maxSourceClocks)
	}
}