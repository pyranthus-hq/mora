package mora

import (
	"bytes"
	"fmt"
	"io"
	"strings"
)

// printVerbHelp prints the usage lines that name the verb, narrowed to the
// subcommand when the usage text has lines for it, or the whole usage text when
// it has none.
func printVerbHelp(w io.Writer, args []string) {
	var all bytes.Buffer
	printUsage(&all)
	var words []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			break
		}
		words = append(words, a)
	}
	// Longest prefix of the typed words that some usage line names.
	for n := len(words); n >= 1; n-- {
		prefix := "mora " + strings.Join(words[:n], " ")
		var lines []string
		for _, l := range strings.Split(all.String(), "\n") {
			t := strings.TrimSpace(l)
			if t == prefix || strings.HasPrefix(t, prefix+" ") || strings.HasPrefix(t, prefix+"|") {
				lines = append(lines, "  "+t)
			}
		}
		if len(lines) > 0 {
			fmt.Fprintf(w, "usage for %s:\n%s\n", prefix, strings.Join(lines, "\n"))
			return
		}
	}
	if len(words) > 0 {
		fmt.Fprintf(w, "usage: mora %s [flags]\nThe usage text has no example for this command; run `mora help` for every command, or see docs/guide.md.\n", words[0])
		return
	}
	printUsage(w)
}
