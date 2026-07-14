package exam

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	emailToken = regexp.MustCompile(`(?i)[a-z0-9.!#$%&'*+/=?^_` + "`" + `{|}~-]+@([a-z0-9-]+(?:\.[a-z0-9-]+)+)`)
	plusPhone  = regexp.MustCompile(`\+[0-9]{11}`)
	parenPhone = regexp.MustCompile(`\([0-9]{3}\)[ ]*[0-9]{3}-[0-9]{4}`)
	dashPhone  = regexp.MustCompile(`\b[0-9]{3}-[0-9]{3}-[0-9]{4}\b`)
)

func Lint(l Ledger) error {
	b, err := json.Marshal(l)
	if err != nil {
		return fmt.Errorf("%s: encode ledger: %w", LintRealIdentity, err)
	}
	return lintBytes(LintRealIdentity, "ledger", b)
}

func LintCorpus(files map[string][]byte) error {
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		b := files[path]
		if err := lintBytes(LintCorpusBytes, path, b); err != nil {
			return err
		}
	}
	return nil
}

func lintBytes(rule, path string, b []byte) error {
	s := strings.ToLower(string(b))
	for _, forbidden := range []string{"karode", "neil patel", "pyranthus", "halcyon", "northwind"} {
		if strings.Contains(s, forbidden) {
			return fmt.Errorf("ERR_%s [%s]: %s contains forbidden identity literal %q", strings.ToUpper(rule), rule, path, forbidden)
		}
	}
	for _, match := range emailToken.FindAllStringSubmatch(s, -1) {
		if !reservedDomain(match[1]) {
			return fmt.Errorf("ERR_%s [%s]: %s contains non-reserved email %q", strings.ToUpper(rule), rule, path, match[0])
		}
	}
	for _, token := range plusPhone.FindAllString(s, -1) {
		if !fictionalHandle(token) {
			return fmt.Errorf("ERR_%s [%s]: %s contains non-fictional handle %q", strings.ToUpper(rule), rule, path, token)
		}
	}
	for _, re := range []*regexp.Regexp{parenPhone, dashPhone} {
		for _, token := range re.FindAllString(s, -1) {
			digits := regexp.MustCompile(`[0-9]`).FindAllString(token, -1)
			if len(digits) != 10 {
				return fmt.Errorf("ERR_%s [%s]: %s contains malformed handle %q", strings.ToUpper(rule), rule, path, token)
			}
			joined := strings.Join(digits, "")
			line, _ := strconv.Atoi(joined[6:])
			if joined[:6] != "555010" || line < 100 || line > 199 {
				return fmt.Errorf("ERR_%s [%s]: %s contains non-fictional handle %q", strings.ToUpper(rule), rule, path, token)
			}
		}
	}
	return nil
}
