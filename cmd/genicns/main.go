// genicns renders Mora.icns deterministically from the committed pixel-art
// SVG. It is a release-pipeline tool, never part of the shipped mora binary:
//
//	go run ./cmd/genicns docs/assets/mora-eye.svg dist/Mora.icns
//
// Standard library only, CGO-free, byte-stable for a given input + toolchain.
package main

import (
	"fmt"
	"os"

	"github.com/pyranthus-hq/mora/internal/appbundle"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: genicns <pixel-art.svg> <output.icns>")
		os.Exit(2)
	}
	svg, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "genicns: %v\n", err)
		os.Exit(1)
	}
	icns, err := appbundle.GenerateICNS(svg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "genicns: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(os.Args[2], icns, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "genicns: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("genicns: wrote %s (%d bytes)\n", os.Args[2], len(icns))
}
