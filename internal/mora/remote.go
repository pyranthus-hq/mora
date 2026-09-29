package mora

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
)

// `mora remote expose` (#539) prints the user-controlled tunnel commands that
// publish `mora mcp serve-http` beyond loopback. It executes none of them:
// Tailscale Funnel and Cloudflare tunnel are the operator's network, not Mora's.
// Printing leaves the decision and the undo with the operator, and keeps this
// subcommand runnable on a machine with no tunnel tooling installed.
//
// Remote MCP is meant to leave the loopback boundary (cloud agents), so unlike
// `mora companion expose` this helper does print Funnel and Cloudflare lines.

const (
	schemaRemoteExpose = "mora.remote.expose"
	remoteUsage        = "usage: mora remote <expose>"
	remoteExposeUsage  = "usage: mora remote expose [--port N] [--hostname NAME] [--json]"
)

// remoteExposePayload is the print-only receipt. Commands are argv slices so a
// caller that wants to run one does not have to unparse a shell line; the human
// branch joins them with shellLine.
type remoteExposePayload struct {
	ListenerPort      int      `json:"listener_port"`
	Backend           string   `json:"backend"`
	Hostname          string   `json:"hostname"`
	HostnameKnown     bool     `json:"hostname_known"`
	AllowHost         string   `json:"allow_host"`
	ActiveProfiles    int      `json:"active_profiles"`
	ListenCommand     []string `json:"listen_command"`
	FunnelCommand     []string `json:"funnel_command"`
	CloudflareCommand []string `json:"cloudflare_command"`
	FunnelOffCommand  []string `json:"funnel_off_command"`
	ExecutesNothing   bool     `json:"executes_nothing"`
	Note              string   `json:"note"`
}

func cmdRemote(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	_ = stderr
	if len(args) == 0 {
		return errors.New(remoteUsage)
	}
	switch args[0] {
	case "expose":
		return cmdRemoteExpose(ctx, args[1:], stdout)
	default:
		return fmt.Errorf("%s (unknown subcommand %q)", remoteUsage, args[0])
	}
}

// buildRemoteExposeCommands is the string-only API: given a loopback port and an
// optional public hostname, it returns the argv slices an operator can paste.
// It never looks at PATH, never shells out, and never starts a listener.
func buildRemoteExposeCommands(port int, hostname string) remoteExposePayload {
	known := hostname != ""
	node := hostname
	if !known {
		node = hostnamePlaceholder
	}
	backend := "http://127.0.0.1:" + strconv.Itoa(port)
	return remoteExposePayload{
		ListenerPort:  port,
		Backend:       backend + "/mcp",
		Hostname:      node,
		HostnameKnown: known,
		AllowHost:     node,
		ListenCommand: []string{
			"mora", "mcp", "serve-http",
			"--port", strconv.Itoa(port),
			"--allow-host", node,
		},
		FunnelCommand: []string{
			"tailscale", "funnel", "--bg", "--https=443", backend,
		},
		CloudflareCommand: []string{
			"cloudflared", "tunnel", "--url", backend,
		},
		FunnelOffCommand: []string{
			"tailscale", "funnel", "off",
		},
		ExecutesNothing: true,
		Note:            "prints user-controlled tunnel commands only; Mora never starts a tunnel",
	}
}

func cmdRemoteExpose(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("remote expose", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	port := fs.Int("port", mcpHTTPDefaultPort, "loopback port `mora mcp serve-http` listens on")
	hostname := fs.String("hostname", "", "public hostname your tunnel forwards (e.g. node.tailnet.ts.net)")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return errors.New(remoteExposeUsage)
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("%s (unexpected argument %q)", remoteExposeUsage, fs.Arg(0))
	}
	if *port <= 0 || *port > 65535 {
		return fmt.Errorf("invalid --port %d: must be between 1 and 65535", *port)
	}

	out := buildRemoteExposeCommands(*port, *hostname)

	cfg, err := loadConfigFor(ctx)
	if err == nil {
		if store, serr := loadAgentsStore(cfg); serr == nil {
			out.ActiveProfiles = store.activeCount()
		}
	}

	if *jsonOut {
		return emitReceipt(stdout, schemaRemoteExpose, 1, out)
	}

	fmt.Fprintf(stdout, "backend\t%s\n", out.Backend)
	fmt.Fprintf(stdout, "allow-host\t%s\n", out.AllowHost)
	fmt.Fprintf(stdout, "active profiles\t%d\n", out.ActiveProfiles)
	fmt.Fprintln(stdout, "executes\tnothing (this command only prints)")
	fmt.Fprintln(stdout)
	if out.ActiveProfiles == 0 {
		fmt.Fprintln(stdout, "No active agent profiles yet. Create one before serving:")
		fmt.Fprintln(stdout, "  mora agents add <name> --read-scopes <scope>")
		fmt.Fprintln(stdout)
	}
	if !out.HostnameKnown {
		fmt.Fprintf(stdout, "Replace %s with the public hostname your tunnel forwards,\n", hostnamePlaceholder)
		fmt.Fprintln(stdout, "or re-run with --hostname NAME and copy the lines below verbatim.")
		fmt.Fprintln(stdout)
	}
	fmt.Fprintln(stdout, "Start the loopback MCP server, then publish it with a tunnel you control:")
	fmt.Fprintf(stdout, "  %s\n", shellLine(out.ListenCommand))
	fmt.Fprintf(stdout, "  %s\n", shellLine(out.FunnelCommand))
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Or with a Cloudflare quick tunnel:")
	fmt.Fprintf(stdout, "  %s\n", shellLine(out.CloudflareCommand))
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Stop Tailscale Funnel on this node:")
	fmt.Fprintf(stdout, "  %s\n", shellLine(out.FunnelOffCommand))
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Mora binds 127.0.0.1 only. Exposure is your tunnel. Pass the same hostname")
	fmt.Fprintln(stdout, "to --allow-host so serve-http accepts the Host header your tunnel forwards.")
	return nil
}
