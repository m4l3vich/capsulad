package sandbox

import (
	"debug/elf"
	"fmt"
	"strings"
)

// LibcClass names the two libc families the supported targets use.
type LibcClass string

const (
	LibcGNU  LibcClass = "gnu"
	LibcMusl LibcClass = "musl"
)

// CheckLibc reports whether the shared object at path links a libc of the
// requested class. It reads the ELF dynamic section (DT_NEEDED): a shared object
// carries no program interpreter, so a string inside the file is not evidence.
// A self-contained object with no libc dependency is accepted (it is loadable);
// "must be dynamically linked" is enforced separately by CheckLibcRuntime for
// the installed native runtime.
func CheckLibc(path string, want LibcClass) error {
	if want != LibcGNU && want != LibcMusl {
		return fmt.Errorf("unknown libc class %q", want)
	}
	f, err := elf.Open(path)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", path, err)
	}
	defer f.Close()
	libs, err := f.ImportedLibraries()
	if err != nil {
		return fmt.Errorf("inspect %s dynamic section: %w", path, err)
	}
	var libc []string
	for _, lib := range libs {
		if strings.Contains(lib, "libc.musl") || strings.Contains(lib, "ld-musl") ||
			lib == "libc.so.6" || strings.HasPrefix(lib, "libc.so.") {
			libc = append(libc, lib)
		}
	}
	if len(libc) == 0 {
		// A self-contained shared object is loadable; only the native runtime is
		// required to be dynamically linked (CheckLibcRuntime).
		return nil
	}
	for _, lib := range libc {
		musl := strings.Contains(lib, "musl")
		if want == LibcMusl && musl || want == LibcGNU && !musl {
			return nil
		}
	}
	return fmt.Errorf("%s is linked against %s, but the target needs %s", path, strings.Join(libc, ", "), want)
}

// LibcForABI maps a target ABI string (the profile's Architecture.ABIContains)
// to the libc a runtime must be linked against.
func LibcForABI(abi string) LibcClass {
	if strings.Contains(strings.ToLower(abi), "musl") {
		return LibcMusl
	}
	return LibcGNU
}

// CheckLibcRuntime is the strict form used for the installed native runtime: a
// runtime with no libc dependency is a static link and cannot serve as a shared
// library the launcher dlopens.
func CheckLibcRuntime(path string, want LibcClass) error {
	if err := CheckLibc(path, want); err != nil {
		return err
	}
	f, err := elf.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	libs, err := f.ImportedLibraries()
	if err != nil {
		return err
	}
	for _, lib := range libs {
		if strings.Contains(lib, "libc") {
			return nil
		}
	}
	return fmt.Errorf("%s declares no libc dependency (static link); it cannot be loaded as the shared runtime", path)
}
