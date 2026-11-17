package sandbox

import (
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// CheckGLIBC reports whether every GLIBC_* symbol version an ELF file imports is
// at or below max (for example "2.28") for the named target. A non-ELF file is
// an error: callers filter with IsELF, and treating a host binary as a pass is
// how a Darwin build could slip into a Linux bundle. A file with no imported
// versions (static) passes.
func CheckGLIBC(path, max string) error {
	limit, ok := parseGlibc(max)
	if !ok {
		return fmt.Errorf("invalid glibc ceiling %q", max)
	}
	if ok, err := IsELF(path); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("%s is not an ELF file; it cannot be carried as a native artifact for this target", path)
	}
	f, err := elf.Open(path)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", path, err)
	}
	defer f.Close()
	syms, err := f.ImportedSymbols()
	if errors.Is(err, elf.ErrNoSymbols) {
		return nil // static binary: no imported symbol versions
	}
	if err != nil {
		return fmt.Errorf("inspect %s versions: %w", path, err)
	}
	var worst string
	var worstV [2]int
	for _, sym := range syms {
		if !strings.HasPrefix(sym.Version, "GLIBC_") {
			continue
		}
		v, ok := parseGlibc(strings.TrimPrefix(sym.Version, "GLIBC_"))
		if !ok {
			continue
		}
		if v[0] > limit[0] || (v[0] == limit[0] && v[1] > limit[1]) {
			if worst == "" || v[0] > worstV[0] || (v[0] == worstV[0] && v[1] > worstV[1]) {
				worst, worstV = sym.Version, v
			}
		}
	}
	if worst != "" {
		return fmt.Errorf("%s requires %s, above the %s ceiling for this target", path, worst, max)
	}
	return nil
}

func parseGlibc(s string) ([2]int, bool) {
	parts := strings.SplitN(strings.TrimSpace(s), ".", 3)
	if len(parts) < 2 {
		return [2]int{}, false
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return [2]int{}, false
	}
	return [2]int{major, minor}, true
}

// IsELF reports whether path starts with the ELF magic. A missing/unreadable file
// is an error, so a caller cannot mistake absence for a pass.
func IsELF(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	var magic [4]byte
	n, err := io.ReadFull(f, magic[:])
	if err != nil && n == 0 {
		return false, nil
	}
	return n == 4 && string(magic[:]) == "\x7fELF", nil
}

// CheckMachine verifies that an ELF file targets the profile's architecture.
// A "host"/empty target is permissive. A non-ELF file is an error.
func CheckMachine(path, target string) error {
	switch target {
	case "", "host", "simulator":
		return nil
	}
	f, err := elf.Open(path)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", path, err)
	}
	defer f.Close()
	if f.Class != elf.ELFCLASS64 {
		return fmt.Errorf("%s is not a 64-bit ELF for %s", path, target)
	}
	want, ok := map[string]elf.Machine{
		"aarch64": elf.EM_AARCH64,
		"arm64":   elf.EM_AARCH64,
		"amd64":   elf.EM_X86_64,
		"x86_64":  elf.EM_X86_64,
	}[target]
	if !ok {
		return fmt.Errorf("unknown target architecture %q", target)
	}
	if f.Machine != want {
		return fmt.Errorf("%s is a %s binary, not %s", path, f.Machine, target)
	}
	return nil
}

// CheckInterpRequired is CheckInterp for an entrypoint that must be able to
// dlopen the native SDK runtime. A fully static musl binary cannot: musl reports
// "Dynamic loading not supported", so a package that declares [native_sdk] must
// ship a dynamically linked executable with the target interpreter.
func CheckInterpRequired(path string, want LibcClass) error {
	if err := CheckInterp(path, want); err != nil {
		return err
	}
	f, err := elf.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, prog := range f.Progs {
		if prog.Type == elf.PT_INTERP {
			return nil
		}
	}
	return fmt.Errorf("%s is statically linked; a package that declares [native_sdk] must load the shared runtime, and a static %s binary cannot dlopen it", path, want)
}

// CheckInterp verifies the program interpreter of a dynamic executable. A static
// executable (no PT_INTERP) is accepted, matching tools/check-elf-interp.sh.
func CheckInterp(path string, want LibcClass) error {
	f, err := elf.Open(path)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", path, err)
	}
	defer f.Close()
	for _, prog := range f.Progs {
		if prog.Type != elf.PT_INTERP {
			continue
		}
		data := make([]byte, prog.Filesz)
		if _, err := prog.ReadAt(data, 0); err != nil {
			return fmt.Errorf("read %s interpreter: %w", path, err)
		}
		interp := strings.TrimRight(string(data), "\x00")
		if want == LibcMusl && !strings.Contains(interp, "ld-musl-") {
			return fmt.Errorf("%s names %s, not a musl interpreter", path, interp)
		}
		if want == LibcGNU && !strings.Contains(interp, "ld-linux-") {
			return fmt.Errorf("%s names %s, not a glibc interpreter", path, interp)
		}
		return nil
	}
	return nil
}
