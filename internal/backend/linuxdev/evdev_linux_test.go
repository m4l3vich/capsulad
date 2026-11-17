//go:build linux

package linuxdev

import (
	"encoding/binary"
	"testing"
	"time"
)

// The backend must carry the kernel event's real age, not the read time. This
// is the piece the input manager anchors to its own clock so a late dispatch
// preserves inter-contact gaps and hold deadlines.
func TestDecodeCarriesKernelEventAge(t *testing.T) {
	event := time.Now().Add(-95 * time.Millisecond)
	nanos := event.UnixNano()

	buf := make([]byte, 24)
	binary.LittleEndian.PutUint64(buf[0:8], uint64(nanos/1e9))
	binary.LittleEndian.PutUint64(buf[8:16], uint64((nanos%1e9)/1e3))
	binary.LittleEndian.PutUint16(buf[16:18], evKey)
	binary.LittleEndian.PutUint16(buf[18:20], KeyVolumeUp)
	binary.LittleEndian.PutUint32(buf[20:24], 1)

	d := &evdevDevice{} // CLOCK_REALTIME: the driver default without EVIOCSCLOCKID
	frame := d.decode(buf, 24)
	if frame.typ != evKey || frame.code != KeyVolumeUp || frame.value != 1 {
		t.Fatalf("frame = %+v", frame)
	}
	if frame.ageNS < uint64(90*time.Millisecond) || frame.ageNS > uint64(150*time.Millisecond) {
		t.Fatalf("kernel age = %v, want about 95 ms", time.Duration(frame.ageNS))
	}
}

// A 32-bit input_event has the same tail at a different offset. A zeroed or
// future timestamp is unusable and must fall back to read time.
func TestDecodeLayoutAndUnusableTimestamps(t *testing.T) {
	buf := make([]byte, 16)
	binary.LittleEndian.PutUint16(buf[8:10], evKey)
	binary.LittleEndian.PutUint16(buf[10:12], KeyVolumeDown)
	binary.LittleEndian.PutUint32(buf[12:16], 0)
	d := &evdevDevice{}
	frame := d.decode(buf, 16)
	if frame.typ != evKey || frame.code != KeyVolumeDown || frame.value != 0 {
		t.Fatalf("32-bit frame = %+v", frame)
	}
	if frame.ageNS != 0 {
		t.Fatalf("zeroed 32-bit timestamp produced age %v", time.Duration(frame.ageNS))
	}

	future := time.Now().Add(time.Minute).UnixNano()
	big := make([]byte, 24)
	binary.LittleEndian.PutUint64(big[0:8], uint64(future/1e9))
	binary.LittleEndian.PutUint64(big[8:16], uint64((future%1e9)/1e3))
	binary.LittleEndian.PutUint16(big[16:18], evKey)
	binary.LittleEndian.PutUint16(big[18:20], KeyVolumeUp)
	binary.LittleEndian.PutUint32(big[20:24], 1)
	if frame := d.decode(big, 24); frame.ageNS != 0 {
		t.Fatalf("future timestamp produced age %v", time.Duration(frame.ageNS))
	}

	old := time.Now().Add(-time.Hour).UnixNano()
	stale := make([]byte, 24)
	binary.LittleEndian.PutUint64(stale[0:8], uint64(old/1e9))
	binary.LittleEndian.PutUint64(stale[8:16], uint64((old%1e9)/1e3))
	binary.LittleEndian.PutUint16(stale[16:18], evKey)
	binary.LittleEndian.PutUint16(stale[18:20], KeyVolumeUp)
	binary.LittleEndian.PutUint32(stale[20:24], 1)
	if frame := d.decode(stale, 24); frame.ageNS != 0 {
		t.Fatalf("implausibly old timestamp produced age %v", time.Duration(frame.ageNS))
	}
}
