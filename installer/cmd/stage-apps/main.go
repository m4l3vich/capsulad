// Command stage-apps assembles the independent app channel: it copies app
// archives under -out, hashes them and merges one platform into apps.json
// without touching the core release.json or other platforms.
//
// It is a build-host tool. It never runs the files it stages.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"capsulad/installer/internal/release"
)

func main() {
	out := flag.String("out", "", "release tree root (apps.json and app archives live here)")
	version := flag.String("version", "", "app-channel version written to apps.json")
	core := flag.String("core", "", "core version these apps were built against (optional)")
	platform := flag.String("platform", "", "rockchip-stock, amlogic-stock or amlogic-alpine")
	libc := flag.String("libc", "", "gnu or musl; selects the flat asset prefix")
	apps := flag.String("apps", "", "comma-separated app .tar.gz files for this platform")
	flat := flag.Bool("flat", false, "write GitHub-release assets with a libc__ filename prefix")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: stage-apps -out DIR -version VERSION -platform PLATFORM -libc gnu|musl -apps FILE[,FILE...] [-core VERSION] [-flat]\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	var list []string
	for _, a := range strings.Split(*apps, ",") {
		if a = strings.TrimSpace(a); a != "" {
			list = append(list, a)
		}
	}
	m, err := release.StageApps(release.StageAppsOptions{
		Out:      *out,
		Version:  *version,
		Core:     *core,
		Platform: *platform,
		Libc:     *libc,
		Apps:     list,
		Flat:     *flat,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "stage-apps: %v\n", err)
		os.Exit(1)
	}
	p := m.Firmwares[*platform]
	fmt.Printf("staged %s %s (%s): %d apps -> %s\n", *platform, m.Version, *libc, len(p.Apps), *out)
}
