// Package linuxdev is the board-agnostic Linux device layer shared by the
// board backends (rockchip, amlogic): device-tree and sysfs helpers, evdev
// input, IIO/thermal sensors, LED-class devices and the vendor WS2812B ring
// char device. The generic /dev/watchdog opener lives in
// internal/backend/syswatchdog, which the daemon's supervisor owns.
//
// It owns no board identity. Every device path, geometry and calibration table
// arrives from the caller, which takes it from internal/backend/profile. A board
// backend decides what to probe and how to report the result; this package only
// talks to the kernel.
package linuxdev

import (
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ReadTrim reads a kernel text attribute and strips the trailing NUL, newline
// and space padding device-tree properties and sysfs attributes carry.
func ReadTrim(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimRight(string(data), "\x00\n ")
}

// NodeExists reports whether a rooted device path exists.
func NodeExists(prefix, path string) bool {
	_, err := os.Stat(prefix + path)
	return err == nil
}

// HasCommand reports whether an executable is installed in one of the standard
// binary directories of the (optionally rooted) tree. An empty prefix also
// consults PATH, because the live system may install a client elsewhere.
func HasCommand(prefix, name string) bool {
	for _, dir := range []string{"/usr/bin", "/bin", "/usr/sbin", "/sbin"} {
		if st, err := os.Stat(filepath.Join(prefix+dir, name)); err == nil && !st.IsDir() {
			return true
		}
	}
	if prefix == "" {
		_, err := exec.LookPath(name)
		return err == nil
	}
	return false
}

// Rooted joins a sysfs path under an optional firmware-root prefix. An empty
// prefix must produce an absolute /sys path, not a CWD-relative one.
func Rooted(prefix, path string) string {
	path = strings.TrimPrefix(path, "/")
	if prefix == "" {
		return "/" + path
	}
	return filepath.Join(prefix, path)
}

// GlobSorted expands a glob and returns the matches in lexicographic order so a
// probe result does not depend on directory iteration order.
func GlobSorted(pattern string) []string {
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil
	}
	sort.Strings(matches)
	return matches
}

// ExistingFile returns path when it is a readable regular file, else "".
func ExistingFile(path string) string {
	if st, err := os.Stat(path); err == nil && !st.IsDir() {
		return path
	}
	return ""
}

// DeviceTreeBase returns the flattened device tree view of the live (or
// offline-rooted) system, or "" when the kernel does not expose one.
func DeviceTreeBase(prefix string) string {
	for _, candidate := range []string{"/proc/device-tree", "/sys/firmware/devicetree/base"} {
		if st, err := os.Stat(prefix + candidate); err == nil && st.IsDir() {
			return prefix + candidate
		}
	}
	return ""
}

// FindCompatible walks the flattened device tree (exposed as directories and
// property files) and returns the first node whose compatible property
// contains want.
func FindCompatible(base, want string) string {
	var found string
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if found != "" || depth > 6 {
			return
		}
		if hasCompatible(dir, want) {
			found = dir
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		for _, name := range names {
			walk(filepath.Join(dir, name), depth+1)
		}
	}
	walk(base, 0)
	return found
}

func hasCompatible(dir, want string) bool {
	data, err := os.ReadFile(filepath.Join(dir, "compatible"))
	if err != nil {
		return false
	}
	for _, value := range strings.Split(string(data), "\x00") {
		if value == want {
			return true
		}
	}
	return false
}

// CountLEDChildren counts the ledN subnodes declared under a controller. The
// label property is present on every stock child; a name fallback keeps the
// probe useful for trees that omit labels.
func CountLEDChildren(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	count := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		child := filepath.Join(dir, e.Name())
		if _, err := os.Stat(filepath.Join(child, "label")); err == nil {
			count++
			continue
		}
		if strings.HasPrefix(e.Name(), "led") {
			count++
		}
	}
	return count
}

// Brightness255 quantizes a resolved frame brightness to the 0..255 level the
// LED-class and line-display interfaces take. It clamps first, then rounds, so
// the 5% auto floor stays visibly lit (13, not a truncated 12).
func Brightness255(v float64) int {
	return int(math.Round(ClampBrightness(v) * 255))
}

// NaturalLess orders device names by their numeric suffix so that clock-led2
// precedes clock-led10.
func NaturalLess(a, b string) bool {
	pa, na := splitNumericSuffix(a)
	pb, nb := splitNumericSuffix(b)
	if pa == pb && na != nb {
		return na < nb
	}
	return a < b
}

func splitNumericSuffix(s string) (string, int) {
	i := len(s)
	for i > 0 && s[i-1] >= '0' && s[i-1] <= '9' {
		i--
	}
	if i == len(s) {
		return s, 0
	}
	n, _ := strconv.Atoi(s[i:])
	return s[:i], n
}
