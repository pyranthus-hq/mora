// gencask deterministically renders the dormant signed-Mora.app Homebrew Cask.
// It does not publish or modify a tap.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/pyranthus-hq/mora/internal/appbundle"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gencask", flag.ContinueOnError)
	fs.SetOutput(stderr)
	tag := fs.String("tag", "", "canonical release tag (vMAJOR.MINOR.PATCH)")
	checksums := fs.String("checksums", "", "path to checksums-app.txt")
	out := fs.String("out", "", "output Cask path, or - for stdout")
	autoUpdates := fs.Bool("auto-updates", false, "declare auto_updates true (refused until #291 lands)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *tag == "" || *checksums == "" || *out == "" {
		fmt.Fprintln(stderr, "usage: gencask --tag vMAJOR.MINOR.PATCH --checksums checksums-app.txt --out Casks/mora.rb [--auto-updates]")
		return 2
	}
	manifest, err := os.Open(*checksums)
	if err != nil {
		fmt.Fprintf(stderr, "gencask: %v\n", err)
		return 1
	}
	defer manifest.Close()
	body, err := appbundle.GenerateCask(*tag, manifest, *autoUpdates)
	if err != nil {
		fmt.Fprintf(stderr, "gencask: %v\n", err)
		return 1
	}
	if *out == "-" {
		if _, err := stdout.Write(body); err != nil {
			fmt.Fprintf(stderr, "gencask: %v\n", err)
			return 1
		}
		return 0
	}
	if err := atomicWrite(*out, body); err != nil {
		fmt.Fprintf(stderr, "gencask: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "gencask: wrote %s (%d bytes)\n", *out, len(body))
	return 0
}

func atomicWrite(path string, body []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".mora-cask-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err = tmp.Write(body); err == nil {
		err = tmp.Chmod(0o644)
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
