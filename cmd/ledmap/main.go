// Command ledmap walks every display channel of a Capsula Mini and records
// which digit and segment it lights, so the framework's channel permutation can
// be verified instead of inferred. It drives the device over adb and asks the
// operator one question per channel.
//
// Segment letters follow the conventional 7-segment naming:
//
//	  a
//	f   b
//	  g
//	e   c
//	  d
//
// Usage:
//
//	go run ./cmd/ledmap -serial 192.168.1.50:5555 -out ledmap.json
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

const segmentLetters = "abcdefg"

type entry struct {
	Channel int    `json:"channel"`
	Digit   int    `json:"digit,omitempty"`   // 1..4, 0 for colon
	Segment string `json:"segment,omitempty"` // a..g, or "colon-top"/"colon-bottom"
}

type result struct {
	Device   string  `json:"device"`
	Channels []entry `json:"channels"`
}

func main() {
	serial := flag.String("serial", "", "adb serial (-s); empty uses the only device")
	remote := flag.String("remote", "/data/capsdemo/capsula", "path to capsula on the device")
	device := flag.String("device", "/dev/led_multi_ctrl", "vendor display device")
	value := flag.Int("value", 200, "brightness of the channel under test")
	bg := flag.Int("bg", 12, "dim baseline brightness for the rest of the display")
	out := flag.String("out", "ledmap.json", "where to write the mapping")
	from := flag.Int("from", 0, "first channel")
	to := flag.Int("to", 29, "last channel")
	flag.Parse()

	fmt.Println("Segment letters (conventional 7-segment):")
	fmt.Println("       a")
	fmt.Println("     f   b")
	fmt.Println("       g")
	fmt.Println("     e   c")
	fmt.Println("       d")
	fmt.Println("Answer with digit+letter, e.g. 3c = third digit, lower-right; ct/cb = upper/lower colon dot.")
	fmt.Println()

	r := result{Device: *device}
	if data, err := os.ReadFile(*out); err == nil {
		if err := json.Unmarshal(data, &r); err == nil {
			fmt.Printf("resuming from %s (%d channels already mapped)\n", *out, len(r.Channels))
		}
	}
	done := map[int]bool{}
	for _, e := range r.Channels {
		done[e.Channel] = true
	}

	adb := func(args ...string) error {
		full := []string{}
		if *serial != "" {
			full = append(full, "-s", *serial)
		}
		cmd := exec.Command("adb", append(full, args...)...)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("adb %s: %w", strings.Join(full, " "), err)
		}
		return nil
	}
	light := func(ch int, brightness int) error {
		return adb("shell", fmt.Sprintf("%s platform ledtest -path %s -channels %d -value %d -bg %d -hold 0",
			*remote, *device, ch, brightness, *bg))
	}

	// Start from a known state and leave a marker so the operator can tell the
	// script is driving the display.
	if err := adb("shell", fmt.Sprintf("%s platform ledtest -path %s -hold 0 -channels '' -value 0 -bg %d",
		*remote, *device, *bg)); err != nil {
		fail(err)
	}

	in := bufio.NewScanner(os.Stdin)
	for ch := *from; ch <= *to; ch++ {
		if done[ch] {
			continue
		}
		if err := light(ch, *value); err != nil {
			fail(err)
		}
		e, quit := ask(in, ch)
		if quit {
			save(*out, r)
			return
		}
		r.Channels = append(r.Channels, e)
	}
	if err := adb("shell", fmt.Sprintf("%s platform ledtest -path %s -hold 0 -value 0", *remote, *device)); err != nil {
		fmt.Fprintf(os.Stderr, "clear display: %v\n", err)
	}
	save(*out, r)
	printSummary(r)
}

// ask prompts for one channel until the answer parses. quit reports a request
// to stop and keep what has been recorded.
func ask(in *bufio.Scanner, ch int) (entry, bool) {
	for {
		fmt.Printf("channel %2d: digit 1-4 + segment a-g (e.g. 3c), ct/cb for the colon dots, s to skip, q to save and quit > ", ch)
		if !in.Scan() {
			return entry{}, true
		}
		text := strings.ToLower(strings.TrimSpace(in.Text()))
		switch text {
		case "q":
			return entry{}, true
		case "s":
			return entry{Channel: ch}, false
		case "ct", "colon-top":
			return entry{Channel: ch, Segment: "colon-top"}, false
		case "cb", "colon-bottom":
			return entry{Channel: ch, Segment: "colon-bottom"}, false
		}
		if len(text) >= 2 && text[0] >= '1' && text[0] <= '4' && strings.ContainsRune(segmentLetters, rune(text[1])) {
			digit, _ := strconv.Atoi(text[:1])
			return entry{Channel: ch, Digit: digit, Segment: string(text[1])}, false
		}
		fmt.Println("  expected 1a..4g, ct, cb, s, or q")
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "ledmap:", err)
	os.Exit(1)
}

func save(path string, r result) {
	sort.Slice(r.Channels, func(i, j int) bool { return r.Channels[i].Channel < r.Channels[j].Channel })
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		fail(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		fail(err)
	}
	fmt.Printf("\nwrote %s (%d channels)\n", path, len(r.Channels))
}

// printSummary shows the calibration as the table the frame encoder needs:
// digit and segment role to channel.
func printSummary(r result) {
	type key struct {
		digit int
		seg   string
	}
	table := map[key]int{}
	var missing []string
	for _, e := range r.Channels {
		if e.Segment == "" {
			continue
		}
		table[key{e.Digit, e.Segment}] = e.Channel
	}
	for digit := 1; digit <= 4; digit++ {
		for _, seg := range segmentLetters {
			if _, ok := table[key{digit, string(seg)}]; !ok {
				missing = append(missing, fmt.Sprintf("%d%c", digit, seg))
			}
		}
	}
	for _, seg := range []string{"colon-top", "colon-bottom"} {
		if _, ok := table[key{0, seg}]; !ok {
			missing = append(missing, seg)
		}
	}
	fmt.Println("\nchannel table (digit/segment -> channel):")
	for digit := 1; digit <= 4; digit++ {
		fmt.Printf("  digit %d:", digit)
		for _, seg := range segmentLetters {
			if ch, ok := table[key{digit, string(seg)}]; ok {
				fmt.Printf(" %c=%d", seg, ch)
			}
		}
		fmt.Println()
	}
	for _, seg := range []string{"colon-top", "colon-bottom"} {
		if ch, ok := table[key{0, seg}]; ok {
			fmt.Printf("  %-13s %d\n", seg, ch)
		}
	}
	if len(missing) > 0 {
		fmt.Printf("  unassigned: %s\n", strings.Join(missing, " "))
	}
	if len(missing) == 0 && !hasDuplicateChannels(r) {
		fmt.Println("\ncomplete mapping — hand ledmap.json back to rewrite the encoder")
	}
}

func hasDuplicateChannels(r result) bool {
	seen := map[int]bool{}
	for _, e := range r.Channels {
		if e.Segment == "" {
			continue
		}
		if seen[e.Channel] {
			return true
		}
		seen[e.Channel] = true
	}
	return false
}
