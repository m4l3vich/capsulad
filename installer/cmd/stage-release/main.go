// Command stage-release assembles one platform's entry in a Capsula Mini
// release tree: it copies the deployment bundle and app archives under -out,
// hashes them and merges the platform into release.json without touching other
// platforms.
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
	out := flag.String("out", "", "release tree root (release.json and <platform>/ live here)")
	version := flag.String("version", "", "release version written to release.json")
	platform := flag.String("platform", "", "rockchip-stock, amlogic-stock or amlogic-alpine")
	bundle := flag.String("bundle", "", "deployment bundle .tar.gz for this platform")
	apps := flag.String("apps", "", "comma-separated app .tar.gz files for this platform (optional)")
	flat := flag.Bool("flat", false, "write GitHub-release assets with a platform__ filename prefix")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: stage-release -out DIR -version VERSION -platform PLATFORM -bundle FILE [-apps FILE[,FILE...]] [-flat]\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	var list []string
	for _, a := range strings.Split(*apps, ",") {
		if a = strings.TrimSpace(a); a != "" {
			list = append(list, a)
		}
	}
	m, err := release.Stage(release.StageOptions{
		Out:      *out,
		Version:  *version,
		Platform: *platform,
		Bundle:   *bundle,
		Apps:     list,
		Flat:     *flat,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "stage-release: %v\n", err)
		os.Exit(1)
	}
	p := m.Firmwares[*platform]
	fmt.Printf("staged %s %s: bundle %d bytes, %d apps -> %s\n", *platform, m.Version, p.Bundle.Size, len(p.Apps), *out)
}
