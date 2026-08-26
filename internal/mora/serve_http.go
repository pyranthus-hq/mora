package mora

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	configstore "github.com/pyranthus-hq/mora/internal/config"
	loopbackhttp "github.com/pyranthus-hq/mora/internal/loopbackhttp"
)

const defaultHTTPPort = 7777

// httpCallAllowed is the root-owned explicit MCP exposure policy for the
// generic /call escape hatch. It deliberately excludes delete_memory, and a
// newly added tool is not exposed until it is consciously allowlisted here.
var httpCallAllowed = map[string]bool{
	"brief":          true,
	"context_memory": true,
	"digest":         true,
	"get_entity":     true,
	"list_entities":  true,
	"list_memory":    true,
	"meeting_prep":   true,
	"read_memory":    true,
	"search_memory":  true,
	"think":          true,
	"write_memory":   true,
}

// cmdServe dispatches `mora serve <subcommand>`. Today only `http` exists; the
// verb is deliberately generic so future transports (e.g. an SSE MCP endpoint)
// can slot in beside it without a new top-level command.
// serveHTTPServiceReceipt is the machine form of an install/uninstall/status
// service action.
type serveHTTPServiceReceipt struct {
	Action string `json:"action"`
	OK     bool   `json:"ok"`
}

func cmdServe(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] != "http" {
		return errors.New("usage: mora serve http [install|uninstall|status] [--port 7777] [--print-token]")
	}
	rest := args[1:]
	jsonOut := false
	filtered := make([]string, 0, len(rest))
	for _, a := range rest {
		if a == "--json" {
			jsonOut = true
			continue
		}
		filtered = append(filtered, a)
	}
	rest = filtered
	if len(rest) > 0 {
		switch rest[0] {
		case "install", "uninstall", "status":
			cfg, err := loadConfigFor(ctx)
			if err != nil {
				return err
			}
			// The service verb's prose moves to stderr under --json so stdout
			// carries exactly one document.
			out := stdout
			if jsonOut {
				out = stderr
			}
			if err := serveHTTPService(cfg, rest[0], out); err != nil {
				return err
			}
			if jsonOut {
				return emitReceipt(stdout, "mora.serve.http."+rest[0], 1, serveHTTPServiceReceipt{
					Action: rest[0], OK: true,
				})
			}
			return nil
		}
	}
	if jsonOut {
		// The long-running server itself has no document to emit; refuse rather
		// than start a blocking server that a machine caller is waiting to parse.
		return newCodedError(errCodeUsageUnknownFlag, nil, "mora serve http does not support --json (it runs a server); use `mora serve http status --json`")
	}
	return serveLoopbackHTTP(ctx, rest, stdout)
}
func serveLoopbackHTTP(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("serve http", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	port := fs.Int("port", envPortOr(defaultHTTPPort), "loopback port to listen on (or set MORA_PORT)")
	printToken := fs.Bool("print-token", false, "print the bearer token and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfigFor(ctx)
	if err != nil {
		return err
	}
	token, err := loopbackhttp.LoadOrCreateToken(cfg.ConfigDir)
	if err != nil {
		return err
	}
	if *printToken {
		fmt.Fprintln(stdout, token)
		return nil
	}
	return newHTTPServer(token, *port).lower(ctx).Serve(ctx, stdout)
}
func envPortOr(def int) int {
	if v := os.Getenv("MORA_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n < 65536 {
			return n
		}
	}
	return def
}

type httpRoute = loopbackhttp.Route
type httpServer struct {
	token string
	port  int
}

func newHTTPServer(token string, port int) *httpServer { return &httpServer{token: token, port: port} }
func (s *httpServer) lower(ctx context.Context) *loopbackhttp.Server {
	return loopbackhttp.New(loopbackhttp.Options{Token: s.token, Port: s.port, Version: BuildVersion, AllowCall: func(name string) bool { return httpCallAllowed[name] }, Dispatch: func(reqCtx context.Context, name string, args map[string]any) (any, error) {
		// Request contexts carry cancellation but not the launch context's
		// injected sandbox; restore it so every dispatched tool resolves the
		// same config the server was started under.
		return callMCPTool(configstore.CarryInjection(reqCtx, ctx), name, args)
	}, Health: func() loopbackhttp.Health {
		cfg, err := loadConfigFor(ctx)
		if err != nil {
			return loopbackhttp.Health{OK: false, State: string(healthUnhealthy), Err: err}
		}
		h := healthOf(cfg, time.Now())
		return loopbackhttp.Health{OK: h.State == healthHealthy, State: string(h.State)}
	}})
}
func (s *httpServer) handler(ctx context.Context) http.Handler   { return s.lower(ctx).Handler() }
func (s *httpServer) httpRoutes(ctx context.Context) []httpRoute { return s.lower(ctx).Routes() }
