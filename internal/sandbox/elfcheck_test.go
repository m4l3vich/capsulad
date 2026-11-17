package sandbox

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// elfFixture builds a minimal ELF64 little-endian file with an optional
// PT_INTERP entry, so the machine and interpreter rules can be tested on real
// bytes without a cross compiler.
func elfFixture(t *testing.T, machine uint16, interp string) string {
	t.Helper()
	const ehsize, phentsize = 64, 56
	phnum := uint16(0)
	payloadOff := uint64(ehsize)
	if interp != "" {
		phnum = 1
		payloadOff = uint64(ehsize + phentsize)
	}
	payload := append([]byte(interp), 0)
	buf := make([]byte, ehsize+int(phnum)*phentsize+len(payload))
	copy(buf[0:4], "\x7fELF")
	buf[4] = 2                                 // ELFCLASS64
	buf[5] = 1                                 // little-endian
	buf[6] = 1                                 // version
	binary.LittleEndian.PutUint16(buf[16:], 3) // ET_DYN
	binary.LittleEndian.PutUint16(buf[18:], machine)
	binary.LittleEndian.PutUint32(buf[20:], 1)
	binary.LittleEndian.PutUint64(buf[32:], ehsize)
	binary.LittleEndian.PutUint64(buf[40:], 0)
	binary.LittleEndian.PutUint16(buf[52:], ehsize)
	binary.LittleEndian.PutUint16(buf[54:], phentsize)
	binary.LittleEndian.PutUint16(buf[56:], phnum)
	if interp != "" {
		ph := buf[ehsize:]
		binary.LittleEndian.PutUint32(ph[0:], 3) // PT_INTERP
		binary.LittleEndian.PutUint64(ph[8:], payloadOff)
		binary.LittleEndian.PutUint64(ph[32:], uint64(len(payload)))
		binary.LittleEndian.PutUint64(ph[40:], uint64(len(payload)))
		binary.LittleEndian.PutUint64(ph[48:], 1)
		copy(buf[payloadOff:], payload)
	}
	path := filepath.Join(t.TempDir(), "fixture.elf")
	if err := os.WriteFile(path, buf, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

const (
	emAArch64 = 0xB7
	emX8664   = 0x3E
)

func TestCheckMachineRejectsWrongArchitecture(t *testing.T) {
	aarch64 := elfFixture(t, emAArch64, "")
	if err := CheckMachine(aarch64, "aarch64"); err != nil {
		t.Fatalf("aarch64 fixture rejected: %v", err)
	}
	x86 := elfFixture(t, emX8664, "")
	if err := CheckMachine(x86, "aarch64"); err == nil {
		t.Fatal("x86_64 fixture accepted for an aarch64 target")
	}
	if err := CheckMachine(aarch64, "host"); err != nil {
		t.Fatalf("host target must be permissive: %v", err)
	}
	if err := CheckMachine("/nonexistent", "aarch64"); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestCheckInterpMatchesTargetLibc(t *testing.T) {
	musl := elfFixture(t, emAArch64, "/lib/ld-musl-aarch64.so.1")
	if err := CheckInterp(musl, LibcMusl); err != nil {
		t.Fatalf("musl interpreter rejected: %v", err)
	}
	if err := CheckInterp(musl, LibcGNU); err == nil {
		t.Fatal("musl interpreter accepted for a gnu target")
	}
	static := elfFixture(t, emAArch64, "")
	if err := CheckInterp(static, LibcGNU); err != nil {
		t.Fatalf("static executable rejected: %v", err)
	}
	gnu := elfFixture(t, emAArch64, "/lib/ld-linux-aarch64.so.1")
	if err := CheckInterp(gnu, LibcGNU); err != nil {
		t.Fatalf("gnu interpreter rejected: %v", err)
	}
}

func TestCheckGLIBCRejectsNonELF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-elf")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := CheckGLIBC(path, "2.28")
	if err == nil || !errors.Is(err, nil) && err.Error() == "" {
		t.Fatalf("non-ELF accepted: %v", err)
	}
	if err == nil {
		t.Fatal("non-ELF accepted by the glibc gate")
	}
}
