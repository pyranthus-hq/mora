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

func cmdServe(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 || args[0] != "http" {
		return errors.New("usage: mora serve http [install|uninstall|status] [--port 7777] [--print-token]")
	}
	rest := args[1:]
	if len(rest) > 0 {
		switch rest[0] {
		case "install", "uninstall", "status":
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			return serveHTTPService(cfg, rest[0], stdout)
		}
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
	cfg, err := loadConfig()
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
	return newHTTPServer(token, *port).lower().Serve(ctx, stdout)
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
func (s *httpServer) lower() *loopbackhttp.Server {
	return loopbackhttp.New(loopbackhttp.Options{Token: s.token, Port: s.port, Version: BuildVersion, AllowCall: func(name string) bool { return httpCallAllowed[name] }, Dispatch: func(ctx context.Context, name string, args map[string]any) (any, error) {
		return callMCPTool(ctx, name, args)
	}, Health: func() loopbackhttp.Health {
		cfg, err := loadConfig()
		if err != nil {
			return loopbackhttp.Health{OK: false, State: string(healthUnhealthy), Err: err}
		}
		h := healthOf(cfg, time.Now())
		return loopbackhttp.Health{OK: h.State == healthHealthy, State: string(h.State)}
	}})
}
func (s *httpServer) handler() http.Handler   { return s.lower().Handler() }
func (s *httpServer) httpRoutes() []httpRoute { return s.lower().Routes() }
