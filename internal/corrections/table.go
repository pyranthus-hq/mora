package corrections

import "strings"

// The tables below are the only source of Shape text. Add entries only as
// table text.
var shapeSubs = map[string]string{
	"git":  "add am apply bisect blame branch checkout cherry-pick clean clone commit config diff fetch init log merge mv pull push rebase reflog remote reset restore revert rm show stash status submodule switch tag worktree",
	"npm":  "audit ci exec init install publish run test update",
	"pnpm": "add exec install publish run test update", "yarn": "add install publish run test", "bun": "add install run test x",
	"docker":  "build compose exec images kill logs ps pull push rm rmi run stop system volume",
	"kubectl": "apply delete describe edit exec get logs patch rollout scale",
	"gh":      "api auth issue pr release repo run workflow",
	"cargo":   "add build check clippy fmt install publish run test",
	"go":      "build fmt generate get install mod run test tool vet",
	"pip":     "install uninstall", "pip3": "install uninstall", "uv": "add pip run sync", "poetry": "add install run",
	"terraform": "apply destroy import init plan", "brew": "install tap uninstall upgrade",
	"apt": "install remove", "apt-get": "install remove",
	"systemctl": "disable enable restart start stop", "launchctl": "load unload",
	"vercel": "deploy", "wrangler": "deploy", "fly": "deploy",
	"mora": "check corrections doctor hook ingest rules search sync upgrade",
}
var shapePlain = strings.Fields("rm mv cp chmod chown ln mkdir touch dd kill pkill killall curl wget ssh scp rsync tar zip unzip make just cmake python python3 node ruby perl bash sh zsh psql mysql sqlite3 redis-cli pytest jest vitest tsc eslint prettier ruff mypy black gofmt golangci-lint mvn gradle dotnet swift xcodebuild nix claude codex npx bunx docker-compose ffmpeg open orca aws gcloud az")
var shapeLow = strings.Fields("cd pushd popd export set source . echo printf true false sleep wait : ls pwd cat head tail tee wc grep rg find sed awk sort uniq cut tr date which jq test [ stat file du df ps basename dirname realpath diff xargs nl seq mktemp")
var shapeFlags = map[string]string{
	"git push":   "--force --force-with-lease --no-verify --delete --mirror --tags",
	"git commit": "--no-verify --amend", "git reset": "--hard", "git checkout": "--force",
	"git add": "-A --all .", "git branch": "-D", "git rebase": "-i",
	"npm install": "--force -g", "kubectl delete": "--force --all", "terraform apply": "-auto-approve",
	"docker run": "--privileged", "rsync": "--delete",
}
var shapeAlias = map[string]string{"git push -f": "--force", "git checkout -f": "--force", "git commit -n": "--no-verify"}
var shapeClusters = map[string]string{"rm": "rf", "git clean": "f"}

// wrappers are dropped from the front of a simple command.
var wrappers = map[string]bool{
	"sudo": true, "env": true, "nohup": true, "time": true, "command": true, "exec": true, "nice": true, "timeout": true,
}

// valueFlags take the next token as their value, so it is not a subcommand.
var valueFlags = map[string]bool{"-C": true, "-c": true, "--git-dir": true, "--work-tree": true}

// Each lookup below maps a word to the table's own string. Shape is built from
// those values, never from the word that was looked up.
var (
	lowWords   = wordSet(shapeLow)
	plainWords = wordSet(shapePlain)
	subNames   = map[string]string{}
	subWords   = map[string]map[string]string{}
)

func init() {
	for name, subs := range shapeSubs {
		subNames[name] = name
		subWords[name] = wordSet(strings.Fields(subs))
	}
}

func wordSet(words []string) map[string]string {
	set := make(map[string]string, len(words))
	for _, w := range words {
		set[w] = w
	}
	return set
}

// AllowedTokens returns every string the tables can put in a Shape.
func AllowedTokens() map[string]bool {
	allowed := map[string]bool{}
	add := func(words ...string) {
		for _, w := range words {
			allowed[w] = true
		}
	}
	add(shapePlain...)
	add(shapeLow...)
	for name, subs := range shapeSubs {
		add(name)
		add(strings.Fields(subs)...)
	}
	for _, flags := range shapeFlags {
		add(strings.Fields(flags)...)
	}
	for _, flag := range shapeAlias {
		add(flag)
	}
	for _, letters := range shapeClusters {
		add("-" + letters)
		for i := range letters {
			add("-" + letters[i:i+1])
		}
	}
	return allowed
}
