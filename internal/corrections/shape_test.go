package corrections

import (
	"reflect"
	"strings"
	"testing"
)

const canary = "CANARY_7f3a"

func primaryShape(t *testing.T, command string) (string, Class) {
	t.Helper()
	p := Parse(command)
	if p.Primary < 0 {
		t.Fatalf("Parse(%q) has no primary", command)
	}
	return p.Segments[p.Primary].Shape, p.Segments[p.Primary].Class
}

func TestShapeTable(t *testing.T) {
	cases := []struct {
		command, shape string
		class          Class
	}{
		{"git push -f origin main", "git push --force", ClassKnown},
		{"git -C /x push --force", "git push --force", ClassKnown},
		{"git --git-dir /x --work-tree /y push --force", "git push --force", ClassKnown},
		{"git -c a=b --no-pager push", "git push", ClassKnown},
		{"git push --force-with-lease", "git push --force-with-lease", ClassKnown},
		{"git push --force=true origin", "git push", ClassKnown},
		{"git push --tags --no-verify --force", "git push --force --no-verify --tags", ClassKnown},
		{"git commit -n -m x", "git commit --no-verify", ClassKnown},
		{"git checkout -f main", "git checkout --force", ClassKnown},
		{"git add .", "git add .", ClassKnown},
		{"git add -A", "git add -A", ClassKnown},
		{"git clean -fdx", "git clean -f", ClassKnown},
		{"rm -fr x", "rm -rf", ClassKnown},
		{"rm -Rf x", "rm -rf", ClassKnown},
		{"rm -r -f x", "rm -rf", ClassKnown},
		{"rm -r x", "rm -r", ClassKnown},
		{"rm -f x", "rm -f", ClassKnown},
		{"rm --force x", "rm", ClassKnown},
		{"rm x", "rm", ClassKnown},
		{"/usr/bin/git status", "git status", ClassKnown},
		{"git.exe status", "git status", ClassKnown},
		{`C:\\tools\\git.exe status`, "git status", ClassKnown},
		{"FOO=1 sudo git status", "git status", ClassKnown},
		{"sudo env FOO=1 git status", "git status", ClassKnown},
		{"sudo -E nohup git status", "git status", ClassKnown},
		{"timeout 5 git status", "git status", ClassKnown},
		{"timeout -s KILL 5 git status", "", ClassOther},
		{"npm run secret-script", "npm run", ClassKnown},
		{"npm install -g", "npm install -g", ClassKnown},
		{"git foo", "git", ClassKnown},
		{"git", "git", ClassKnown},
		{"mytool x", "", ClassOther},
		{"cat x", "cat", ClassLow},
		{"[ -f x ]", "[", ClassLow},
		{"rsync -a --delete a b", "rsync --delete", ClassKnown},
		{"terraform apply -auto-approve", "terraform apply -auto-approve", ClassKnown},
	}
	for _, c := range cases {
		shape, class := primaryShape(t, c.command)
		if shape != c.shape || class != c.class {
			t.Errorf("%q: got %q class %d, want %q class %d", c.command, shape, class, c.shape, c.class)
		}
	}
}

func TestLexer(t *testing.T) {
	cases := []struct {
		command string
		want    [][]string
	}{
		{"a 2>&1 | b", [][]string{{"a", "2>&1"}, {"b"}}},
		{"a &> f", [][]string{{"a", "&>", "f"}}},
		{"a &>f", [][]string{{"a", "&>f"}}},
		{"a >&2", [][]string{{"a", ">&2"}}},
		{"a <&3", [][]string{{"a", "<&3"}}},
		{`x "a;b"`, [][]string{{"x", "a;b"}}},
		{`x 'a;b'`, [][]string{{"x", "a;b"}}},
		{`x a\;b`, [][]string{{"x", "a;b"}}},
		{`x 'a\'`, [][]string{{"x", `a\`}}},
		{`x "a\"b"`, [][]string{{"x", `a"b`}}},
		{"x \\\n  y", [][]string{{"x", "y"}}},
		{"a; b\nc && d || e & f", [][]string{{"a"}, {"b"}, {"c"}, {"d"}, {"e"}, {"f"}}},
		{"a # b;c", [][]string{{"a"}}},
		{"a # b\nc", [][]string{{"a"}, {"c"}}},
		{"a#b", [][]string{{"a#b"}}},
		{"cat <<EOF\nbody; rm -rf x\nEOF\nnext", [][]string{{"cat"}, {"next"}}},
		{"cat <<'EOF' | grep x\nbody\nEOF\n", [][]string{{"cat"}, {"grep", "x"}}},
		{"cat <<-EOF\n\tbody\n\tEOF\nnext", [][]string{{"cat"}, {"next"}}},
		{"cat <<EOF\nnever ends", [][]string{{"cat"}}},
		{"cat <<A <<B\none\nA\ntwo\nB\nnext", [][]string{{"cat"}, {"next"}}},
		{"cat <<< word", [][]string{{"cat", "<<<", "word"}}},
		{"(cd x && make)", [][]string{{"cd", "x"}, {"make"}}},
		{`x "`, [][]string{{"x", ""}}},
		{`x 'unterminated`, [][]string{{"x", "unterminated"}}},
		{`x \`, [][]string{{"x"}}},
		{`x ""`, [][]string{{"x", ""}}},
		{"", nil},
		{" ; && | ", nil},
	}
	for _, c := range cases {
		if got := lex(c.command); !reflect.DeepEqual(got, c.want) {
			t.Errorf("lex(%q) = %q, want %q", c.command, got, c.want)
		}
	}
}

func TestLexerCaps(t *testing.T) {
	if got := len(lex(strings.Repeat("a;", 100))); got != maxSimple {
		t.Errorf("simple commands = %d, want %d", got, maxSimple)
	}
	long := lex("x " + strings.Repeat("y", 2*maxInput))
	if got := len(long[0][1]); got != maxInput-2 {
		t.Errorf("token length = %d, want %d", got, maxInput-2)
	}
}

func TestPrimary(t *testing.T) {
	cases := []struct {
		name, command, shape string
		index                int
	}{
		{"flag wins", "cd x && npm test && git push --force origin main", "git push --force", 2},
		{"cluster wins", "git status; rm -rf x; npm test", "rm -rf", 1},
		{"first flag", "rm -rf x; git push --force", "rm -rf", 0},
		{"last known", "cd x && npm test && git status", "git status", 2},
		{"known beats low", "git log | grep x", "git log", 0},
		{"known beats other", "git log | mytool", "git log", 0},
		{"last other", "mytool x | othertool | grep y", "", 1},
		{"other beats low", "ls | mytool | wc -l", "", 1},
		{"last low", "ls | grep x | wc -l", "wc", 2},
	}
	for _, c := range cases {
		p := Parse(c.command)
		if p.Primary != c.index || p.Segments[p.Primary].Shape != c.shape {
			t.Errorf("%s: primary %d, want %d (%q)", c.name, p.Primary, c.index, c.shape)
		}
	}
	if p := Parse("   "); p.Primary != -1 || len(p.Segments) != 0 {
		t.Errorf("blank command: %+v", p)
	}
	if p := Parse("FOO=1 sudo"); p.Primary != -1 {
		t.Errorf("nothing left after wrappers: %+v", p)
	}
}

func primaryKey(t *testing.T, command string) Key {
	t.Helper()
	p := Parse(command)
	if p.Primary < 0 {
		t.Fatalf("Parse(%q) has no primary", command)
	}
	return p.Segments[p.Primary].Key
}

func TestKey(t *testing.T) {
	same := [][2]string{
		{"rm -rf /tmp/a/build", "rm -rf build"},
		{"rm -rf /tmp/a/build/", "rm -rf build"},
		{`git commit -m "one two"`, `git commit -m "three four"`},
		{"sleep 5", "sleep 10"},
		{"/usr/bin/git status", "git status"},
		{"git.exe status", "git status"},
		{"FOO=1 sudo git status", "git status"},
		{"git push -f origin main", "git push --force origin main"},
		{"mytool --out=/a/b x", "mytool --out=/c/d x"},
		{"git show abcdef1", "git show 1234567890abcdef"},
		{"npm install pkg@1.2.3", "npm install pkg@4.50.6"},
	}
	for _, c := range same {
		if primaryKey(t, c[0]) != primaryKey(t, c[1]) {
			t.Errorf("%q and %q should share a Key", c[0], c[1])
		}
	}
	different := [][2]string{
		{"rm -rf dist", "rm -rf build"},
		{"rm -rf a", "rm -rf b"},
		{"git push origin main", "git push origin dev"},
		{"git push --force", "git push --force-with-lease"},
		{"mytool -f", "mytool --force"},
		{"a b", "a  c"},
		{"ab c", "a bc"},
	}
	for _, c := range different {
		if primaryKey(t, c[0]) == primaryKey(t, c[1]) {
			t.Errorf("%q and %q should not share a Key", c[0], c[1])
		}
	}
	if Parse("rm -rf a").Segments[0].Name != Parse("rm x").Segments[0].Name {
		t.Error("Name should depend on the command name only")
	}
}

// hostileCommands places the canary everywhere an agent could put text.
func hostileCommands() []string {
	big := strings.Repeat("x", 1<<20)
	return []string{
		canary,
		canary + " status",
		"/usr/bin/" + canary + " --force",
		canary + ".exe push",
		"git " + canary,
		"git " + canary + " --force",
		"git push " + canary,
		"git push --" + canary,
		"git push -" + canary,
		"git push --force=" + canary,
		"git push --force-with-lease=main:" + canary,
		"git -C " + canary + " push",
		"git --git-dir=" + canary + " push",
		canary + "=1 git status",
		"FOO=" + canary + " git status",
		"FOO=" + canary + " " + canary,
		"env " + canary + "=1 git status",
		"sudo -u " + canary + " git status",
		"timeout " + canary + " git status",
		"rm -rf /tmp/" + canary,
		"cat /home/" + canary + "/secrets.txt",
		"cp a /" + canary + "/b",
		"curl https://" + canary + ".example.invalid/x",
		"curl https://example.invalid/x?token=" + canary,
		"wget 'https://example.invalid/?a=b&c=" + canary + "'",
		"ssh " + canary + "@host.invalid",
		`git commit -m "fix ` + canary + ` now"`,
		`git commit -m '` + canary + `'`,
		"cat <<EOF\n" + canary + "\nEOF",
		"cat <<" + canary + "\nbody\n" + canary,
		"git push $(echo " + canary + ")",
		"$(" + canary + ")",
		"`" + canary + "`",
		"git push `" + canary + "`",
		"(" + canary + ")",
		"git status; " + canary,
		"echo é" + canary + "ü",
		canary + "\u00e9 git",
		"git push \u202e" + canary,
		"git\x00" + canary + " push",
		"git push \x01\x1b[31m" + canary + "\x7f",
		"rm \xff\xfe" + canary,
		"git push " + big + canary,
		"git push " + canary + big,
		"git pushCANARY_7f3a",
		"rm -rfCANARY_7f3a",
		"rmCANARY_7f3a -rf",
		"git push --forceCANARY_7f3a",
		"npm run " + canary,
		"docker run --privileged " + canary,
		"kubectl delete pod " + canary + " --force",
		strings.ToLower(canary) + " push",
		"git push " + strings.ToLower(canary),
		strings.Repeat(canary+"; ", 40),
		strings.Repeat("a; ", 31) + canary,
		`\` + canary,
		"'" + canary,
		`"` + canary,
	}
}

func checkShapes(t *testing.T, command string, allowed map[string]bool) {
	t.Helper()
	p := Parse(command)
	for _, seg := range p.Segments {
		if strings.Contains(strings.ToLower(seg.Shape), strings.ToLower(canary)) {
			t.Fatalf("Shape %q holds the canary", seg.Shape)
		}
		if seg.Class == ClassOther && seg.Shape != "" {
			t.Fatalf("class other has Shape %q", seg.Shape)
		}
		if seg.Shape == "" {
			continue
		}
		for _, token := range strings.Split(seg.Shape, " ") {
			if !allowed[token] {
				t.Fatalf("Shape %q holds %q, which is not table text", seg.Shape, token)
			}
		}
	}
	if p.Primary < -1 || p.Primary >= len(p.Segments) || (p.Primary == -1) != (len(p.Segments) == 0) {
		t.Fatalf("Primary %d with %d segments", p.Primary, len(p.Segments))
	}
}

func TestShapeCanary(t *testing.T) {
	commands := hostileCommands()
	if len(commands) < 40 {
		t.Fatalf("only %d hostile commands", len(commands))
	}
	allowed := AllowedTokens()
	for _, command := range commands {
		checkShapes(t, command, allowed)
	}
	// The table words the canary is joined to must still match nothing.
	for _, command := range []string{"git pushCANARY_7f3a", "rmCANARY_7f3a -rf"} {
		if shape, _ := primaryShape(t, command); strings.Contains(shape, "push") || shape == "rm" {
			t.Errorf("%q gave %q", command, shape)
		}
	}
}

func TestShapeTokensAreFromTable(t *testing.T) {
	allowed := AllowedTokens()
	// Every table entry, with every flag, so each emitted form is reached.
	var commands []string
	for name, subs := range shapeSubs {
		commands = append(commands, name)
		for _, sub := range strings.Fields(subs) {
			commands = append(commands, name+" "+sub)
		}
	}
	commands = append(commands, shapePlain...)
	commands = append(commands, shapeLow...)
	for shape, flags := range shapeFlags {
		commands = append(commands, shape+" "+flags)
	}
	for alias := range shapeAlias {
		commands = append(commands, alias)
	}
	for shape, letters := range shapeClusters {
		commands = append(commands, shape+" -"+letters, shape+" -"+strings.ToUpper(letters))
	}
	commands = append(commands, hostileCommands()...)
	for _, command := range commands {
		checkShapes(t, command, allowed)
	}
	for _, want := range []string{"git", "push", "--force", "-rf", "-r", "-f", ".", "["} {
		if !allowed[want] {
			t.Errorf("AllowedTokens misses %q", want)
		}
	}
}

func FuzzParse(f *testing.F) {
	for _, command := range hostileCommands() {
		if len(command) < 1<<12 {
			f.Add(command)
		}
	}
	for _, seed := range []string{
		"", "a 2>&1 | b", "cat <<EOF\nx\nEOF\n", "cat <<", "cat <<-", "'", `"`, `\`, "#", "&", "<<<",
		"((((", "))))", "FOO=1", "sudo", "timeout", "env -i", "rm -", "git -C", "a=b=c d", "\xff",
	} {
		f.Add(seed)
	}
	allowed := AllowedTokens()
	f.Fuzz(func(t *testing.T, command string) {
		checkShapes(t, command, allowed)
		checkShapes(t, command+" "+canary, allowed)
		checkShapes(t, canary+" "+command, allowed)
	})
}
