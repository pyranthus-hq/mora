package companion

// server.go is the narrow companion listener (graph node N12).
//
// It is a SEPARATE loopback HTTP server from internal/loopbackhttp, and the
// separation is the point. The generic loopback API exists to hand a sandboxed
// AI browser the whole tool surface behind one shared bearer token: it has a
// /call escape hatch, it can write memories, and its token lives in a file any
// process running as the user can read. None of that is safe to put on a phone's
// network path. This listener answers three read-only questions and nothing
// else, and the only credential it accepts is a per-device token minted by
// `mora companion pair` and revocable by `mora companion revoke`.
//
// The two token families are disjoint by construction. This server never reads
// http.json, and the generic server never consults the device registry, so a
// leaked loopback token buys nothing here and a stolen device token buys nothing
// there. Both directions are proved in internal/mora/companion_http_test.go.
//
// # What the routes are, and why there are only three
//
//	GET  /v1/companion/today     the three things worth surfacing, with evidence
//	POST /v1/companion/context   a grounded bundle for one query
//	GET  /v1/companion/health    freshness, index state and write policy
//	POST /v1/companion/captures  governed capture, answered with a receipt
//	POST /v1/companion/pairing/confirm  spend a one-time code, receive the token
//
// The allowlist is the security boundary, so it is data (routeDefs) rather than
// a series of mux registrations scattered through the file, and a test walks it.
// There is deliberately no /call, no delete, no sync, no connector command, no
// configuration write and no read-a-memory-by-id route: a phone that can name a
// memory id can enumerate the vault, and a phone that can name a tool has the
// generic API again under a different name.
//
// N12b widened it by one more, and that one is the only route on this listener
// that is reachable WITHOUT a device token — it is the request that asks for the
// token, so it cannot present one. Everything that stands in for the missing
// credential (one slot, one opaque refusal, a timing floor, an attempt budget
// that ends the pairing) is in pairing_route.go, and authGuard below reads the
// exemption out of the same table rather than from a second list.
//
// N21 widened the table by ONE route, deliberately and once. Capture is the only
// write a phone may ask for, it produces a receipt rather than a tool call, and
// what it is permitted to do to the vault is decided by the vault's own write
// policy — see capture.go. Widening the table is a code change in a reviewed
// file, which is the property the table exists to keep.
//
// # 200 does not mean fresh
//
// Every projection carries the kernel's own freshness rows and health summary.
// A degraded index or a dead connector still answers 200 — the honesty lives in
// the body, because a phone that only reads status codes would show a confident
// empty screen during an outage. The kernel supplies those fields; this file
// never invents one.
//
// # What this file will not do
//
// It logs nothing per request. Not the token, not the query, not the body, not
// the answer. The only writer it holds is for the startup banner, and
// TestServerLogsNothingPerRequest asserts the writer stays empty across a full
// authenticated exchange.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Published route patterns. A device pins these strings.
const (
	RouteToday   = "/v1/companion/today"
	RouteContext = "/v1/companion/context"
	RouteHealth  = "/v1/companion/health"
	RouteCapture = "/v1/companion/captures"
	// RouteConfirm is the ONE unauthenticated route: the request that asks for
	// a device token cannot carry one. See pairing_route.go for everything that
	// stands in for the missing credential.
	RouteConfirm = "/v1/companion/pairing/confirm"
)

// LoopbackHost is the ONLY address this server will bind. Not "localhost",
// which resolves through the resolver and can be pointed elsewhere, and not
// "::1" — one literal address, checked as a string, so there is no name
// resolution anywhere in the bind path.
const LoopbackHost = "127.0.0.1"

const (
	// MaxHeaderBytes caps the summed size of the request headers. It is
	// enforced twice: once by http.Server for a real listener, and once in
	// the guard so a handler reached directly (a test, or a future embedding)
	// is bounded too.
	MaxHeaderBytes = 8 << 10
	// ServerReadHeaderTimeout bounds a client that opens a connection and
	// then sends nothing.
	ServerReadHeaderTimeout = 10 * time.Second
	// ServerReadTimeout and ServerWriteTimeout bound a slow body and a slow
	// reader. A companion request is a local read; none of them is long.
	ServerReadTimeout  = 30 * time.Second
	ServerWriteTimeout = 60 * time.Second
	// ServerIdleTimeout bounds a kept-alive connection that has gone quiet.
	// net/http falls back to ReadTimeout when this is unset, which happens to
	// be the value we want — but "happens to be" is not a setting, and the
	// fallback moves if ReadTimeout ever does.
	ServerIdleTimeout = 60 * time.Second
	// markSeenInterval throttles the last-seen stamp. See markSeen.
	markSeenInterval = 5 * time.Minute
	// KernelTimeout bounds ONE kernel call. Every route is a local read over a
	// vault the user owns; a read that has not finished in this long is not
	// going to finish usefully, and a phone waiting on a socket forever is
	// worse than a phone told to come back.
	KernelTimeout = 20 * time.Second
	// RetryAfterSeconds is what a refused caller is told to wait. It is short
	// because the refusal means "someone else is reading", not "come back
	// tomorrow".
	RetryAfterSeconds = 2
	// maxInFlightKernelCalls is the work budget.
	//
	// Today walks the vault. Context runs retrieval. Both are bounded per call
	// but neither is cheap, and a device that pipelines requests would multiply
	// that by however many sockets it opens. ONE at a time is the honest budget
	// for a single-user Mac: the phone is one reader, and the second concurrent
	// request is either a retry or a bug. Excess is refused immediately with a
	// Retry-After rather than queued, because a queue is just a slower way to
	// run out of memory.
	maxInFlightKernelCalls = 1
)

// ErrNotLoopback is returned by NewServer for any address that is not the
// literal loopback host.
var ErrNotLoopback = errors.New("companion: the listener binds " + LoopbackHost + " only")

// ErrBadAllowHost is returned by NewServer for an AllowHost that could never
// match a real Host header.
var ErrBadAllowHost = errors.New("companion: --allow-host must be one exact host or host:port")

// Reader is the kernel seam.
//
// It exists so this package stays a leaf (TestPackageIsALeaf): the contract, the
// registry and the listener compile with only the standard library, and the
// vault, the index and the connectors live behind three methods implemented in
// internal/mora. A device can reach exactly what this interface exposes, which
// is why the interface is three read methods and has no capture, delete or
// tool-dispatch member.
type Reader interface {
	Today(ctx context.Context) (TodayProjection, error)
	Context(ctx context.Context, req ContextRequest) (ContextBundle, error)
	Health(ctx context.Context) (HealthProjection, error)
}

// Writer, the governed-write seam, is declared in capture.go beside the route
// that uses it.

// Authenticator is the credential seam. *Registry implements it.
type Authenticator interface {
	Authenticate(token string) (Device, error)
	MarkSeen(deviceID string) error
}

// ServerOptions configures the listener. Every field except Log is required.
type ServerOptions struct {
	// Addr is host:port. The host must be LoopbackHost.
	Addr string
	// Devices resolves a bearer token to a device.
	Devices Authenticator
	// Reader answers the three read routes.
	Reader Reader
	// Writer answers the capture route through the kernel's governed write
	// path. It is required: a listener assembled without one would declare a
	// route in the allowlist that cannot be served.
	Writer Writer
	// Captures is the durable idempotency store. It is required for the same
	// reason Writer is, and it is injected rather than derived from a path so a
	// caller cannot end up with two stores over one directory.
	Captures *ReservationStore
	// Pairings spends a one-time code and mints a device token. It is required
	// for the same reason Writer is: a listener assembled without one would
	// declare a route in the allowlist that cannot be served.
	//
	// It is a SEPARATE seam from Devices even though *Registry satisfies both.
	// Authenticator is what every authenticated request touches and it is two
	// read-ish methods; Confirmer is what the one unauthenticated route touches
	// and it mints and revokes credentials. Keeping them apart is what lets a
	// reader see, from the type of a field, which surface a stranger can reach.
	Pairings Confirmer
	// AllowHost is the ONE extra Host value the listener accepts, and it is
	// empty by default. Empty means the loopback-only behavior below is
	// unchanged, byte for byte.
	//
	// It exists because a reverse proxy in front of a loopback backend
	// forwards the CLIENT's Host verbatim, so the DNS-rebinding guard — which
	// requires the literal loopback address — refuses every proxied request.
	// Measured against `tailscale serve` (1.102.3): the backend sees
	// Host: <node-fqdn>[:port], X-Forwarded-Host identical, and RemoteAddr
	// 127.0.0.1. Without this field a paired phone behind Tailscale Serve gets
	// 403 forbidden_host on every route.
	//
	// The value is ONE exact host[:port] string, compared case-insensitively
	// against r.Host with no wildcard, no suffix rule and no list. It is
	// supplied by the operator at startup and never read from a file this
	// process does not own.
	AllowHost string
	// Now is the clock, injected so a test can pin it.
	Now func() time.Time
	// PairingFloor is the minimum time one pairing confirmation takes. Zero
	// means PairingFloor.
	//
	// It is configurable for the same reason KernelTimeout is: the property is
	// about real elapsed time, so a test that wants to prove the floor fires has
	// to be able to shorten it, and every other test has to be able to switch it
	// off rather than pay a quarter second per request.
	PairingFloor time.Duration
	// KernelTimeout bounds one kernel call. Zero means KernelTimeout.
	//
	// It is configurable because the deadline PATH has to be reachable in a
	// test with a real, live parent context — the previous deadline test handed
	// the handler an already-expired context, which proved the error mapping
	// and nothing about the timeout actually firing.
	KernelTimeout time.Duration
	// Log receives the startup banner and nothing else.
	Log io.Writer
}

// Server is the narrow companion listener.
type Server struct {
	addr          string
	devices       Authenticator
	reader        Reader
	writer        Writer
	captures      *ReservationStore
	confirms      Confirmer
	allowHost     string
	now           func() time.Time
	log           io.Writer
	kernelTimeout time.Duration

	// kernel is the work budget, held for the duration of one kernel call.
	kernel chan struct{}
	// pairing is the confirmation budget: one at a time, listener-wide. It is
	// separate from kernel on purpose — see pairingSlot.
	pairing chan struct{}
	// pairingMinimum is the confirmation route's timing bucket width. The
	// attempt budget it sits beside is NOT here: it is durable, and lives in
	// the pending device's own record. See pairing_route.go.
	pairingMinimum time.Duration
	// unrecorded latches wrong codes whose durable counter write failed, so a
	// budget that cannot be written still fails closed. It only ever makes the
	// route more closed than the record does; see unrecordedFailures.
	unrecorded *unrecordedFailures

	mu   sync.Mutex
	seen map[string]time.Time
}

// Route is one allowlisted method-and-pattern pair.
//
// Public is part of the published table rather than a separate list, so "which
// routes need no credential" is answered by the same data a test walks. Exactly
// one route sets it, and TestExactlyOneRouteIsUnauthenticated is what keeps that
// true.
type Route struct {
	Method, Pattern string
	Public          bool
}

type route struct {
	Method, Pattern string
	Public          bool
	Handler         http.HandlerFunc
}

// NewServer validates the address and returns the listener.
//
// The address is checked HERE rather than at Serve so a misconfiguration is a
// startup error with a name on it, and so the check cannot be skipped by a
// caller that builds a Handler and mounts it somewhere else.
func NewServer(o ServerOptions) (*Server, error) {
	if err := checkLoopbackAddr(o.Addr); err != nil {
		return nil, err
	}
	if o.Devices == nil {
		return nil, errors.New("companion: the listener needs a device registry")
	}
	if o.Reader == nil {
		return nil, errors.New("companion: the listener needs a kernel reader")
	}
	if o.Writer == nil {
		return nil, errors.New("companion: the listener needs a governed writer")
	}
	if o.Captures == nil {
		return nil, errors.New("companion: the listener needs a capture reservation store")
	}
	if o.Pairings == nil {
		return nil, errors.New("companion: the listener needs a pairing confirmer")
	}
	allowHost, err := CheckAllowHost(o.AllowHost)
	if err != nil {
		return nil, err
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	log := o.Log
	if log == nil {
		log = io.Discard
	}
	timeout := o.KernelTimeout
	if timeout <= 0 {
		timeout = KernelTimeout
	}
	// A NEGATIVE floor means "no floor" and is how a test switches it off; only
	// zero falls back to the default, so an explicit choice is never silently
	// replaced by one this file made.
	floor := o.PairingFloor
	if floor == 0 {
		floor = PairingFloor
	}
	return &Server{
		addr:          o.Addr,
		devices:       o.Devices,
		reader:        o.Reader,
		writer:        o.Writer,
		captures:      o.Captures,
		confirms:      o.Pairings,
		allowHost:     allowHost,
		now:           now,
		log:           log,
		kernelTimeout: timeout,
		kernel:        make(chan struct{}, maxInFlightKernelCalls),

		pairing:        make(chan struct{}, maxInFlightConfirmations),
		pairingMinimum: floor,
		unrecorded:     newUnrecordedFailures(),

		seen: map[string]time.Time{},
	}, nil
}

// checkLoopbackAddr refuses anything but the literal loopback host.
//
// "localhost" is refused as well as 0.0.0.0 and ::1. A name is refused because
// resolving it is someone else's decision — /etc/hosts, a DNS server, a
// container's resolver — and the whole point of this check is that the decision
// is made here, in one place, against one literal string.
func checkLoopbackAddr(addr string) error {
	if addr == "" {
		return fmt.Errorf("%w (got an empty address)", ErrNotLoopback)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%w (%q is not host:port)", ErrNotLoopback, addr)
	}
	if host != LoopbackHost {
		return fmt.Errorf("%w (refusing to bind %q)", ErrNotLoopback, host)
	}
	if port == "" {
		return fmt.Errorf("%w (got no port)", ErrNotLoopback)
	}
	return nil
}

// routeDefs is the allowlist. It is the ONLY place a route is declared, it is
// what Routes() reports, and it is what router() mounts, so the table a test
// walks and the table the server serves cannot drift apart.
func (s *Server) routeDefs() []route {
	return []route{
		{http.MethodGet, RouteToday, false, s.handleToday},
		{http.MethodPost, RouteContext, false, s.handleContext},
		{http.MethodGet, RouteHealth, false, s.handleHealth},
		{http.MethodPost, RouteCapture, false, s.handleCapture},
		{http.MethodPost, RouteConfirm, true, s.handleConfirm},
	}
}

// Routes reports the allowlist.
func (s *Server) Routes() []Route {
	defs := s.routeDefs()
	out := make([]Route, 0, len(defs))
	for _, r := range defs {
		out = append(out, Route{Method: r.Method, Pattern: r.Pattern, Public: r.Public})
	}
	return out
}

// router mounts the allowlist.
//
// The patterns are registered WITHOUT a method, and the method is enforced
// inside each handler instead. That is not the obvious way round, and it is
// deliberate: Go's ServeMux answers a "GET /x" pattern for HEAD as well, so a
// method-in-pattern registration silently serves a method the route table does
// not list. HEAD ran the kernel and returned its headers while Routes() said
// GET only — the allowlist was a claim the mux did not honor.
//
// One method check, in the handler, is a thing a test can drive directly and a
// reader can find. The cost is that an unlisted method on a listed path reaches
// the handler; it gets a 405 with an Allow header there, before any other work.
func (s *Server) router() http.Handler {
	mux := http.NewServeMux()
	for _, rt := range s.routeDefs() {
		mux.HandleFunc(rt.Pattern, rt.Handler)
	}
	return mux
}

// Handler returns the guarded mux: host guard, size guard, authentication, then
// the allowlisted routes. Each handler authenticates AGAIN — see authorize.
func (s *Server) Handler() http.Handler {
	return s.hostGuard(s.sizeGuard(s.authGuard(s.router())))
}

// hostGuard is the DNS-rebinding defense. A browser or an app on the phone's
// network can be pointed at a name that resolves to 127.0.0.1; what it cannot do
// is put the literal loopback address in the Host header and still have the
// browser treat the origin as its own. Requiring the literal is the check.
func (s *Server) hostGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.hostAllowed(r) {
			writeOpaque(w, http.StatusForbidden, "forbidden_host")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hostAllowed decides the guard.
//
// The loopback arm is unchanged and comes first, so a listener with no
// AllowHost behaves exactly as it did before this field existed.
//
// The second arm is the reverse-proxy arm, and it carries a SECOND condition
// that the first does not need: the peer must be loopback. Tailscale Serve
// terminates TLS in tailscaled and dials the backend from 127.0.0.1, so a
// request that arrives with the published name in Host and a peer that is NOT
// loopback did not come through the proxy.
//
// Be precise about what that buys, because it is easy to overclaim: a loopback
// peer does NOT prove the request came through Serve. Any process on this Mac
// can open the port and type the published name in, exactly as it could already
// type 127.0.0.1 in. The check is worth having for one narrower reason — if the
// bind is ever widened, by this code or by a port forwarder someone runs, the
// published name stops being sufficient on its own. Proving WHO is asking is the
// device bearer token's job, and it is the only thing here that does it.
//
// The peer address is NEVER used for anything else. It is not an identity, it is
// not a rate-limit key and it is not logged. Behind Serve the real client is
// carried only in X-Forwarded-For, which is attacker-settable input on any path
// that is not the proxy, so this file reads neither it nor the
// Tailscale-User-* identity headers. The device bearer token stays the only
// credential.
func (s *Server) hostAllowed(r *http.Request) bool {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == LoopbackHost {
		return true
	}
	if s.allowHost == "" {
		return false
	}
	// Exact, whole-string, ASCII-case-insensitive. Host names are
	// case-insensitive on the wire, so folding a/A is not a widening; anything
	// else — a suffix rule, a wildcard, a list — would be, and is deliberately
	// absent.
	//
	// The folding is ASCII-only rather than strings.EqualFold on purpose.
	// EqualFold applies Unicode simple folding, under which U+212A KELVIN SIGN
	// folds to "k" and U+017F LATIN SMALL LETTER LONG S folds to "s" — so an
	// attacker-chosen Host that is not this name byte-wise could still compare
	// equal to it. A Host header for a MagicDNS name is ASCII (an
	// internationalized name reaches the wire as punycode, which is also
	// ASCII), so nothing legitimate is lost, and CheckAllowHost refuses a
	// non-ASCII AllowHost for the same reason.
	if !equalFoldASCII(r.Host, s.allowHost) {
		return false
	}
	return peerIsLoopback(r.RemoteAddr)
}

// equalFoldASCII compares two strings for equality, folding only A-Z with a-z.
//
// It is not strings.EqualFold: see hostAllowed for why Unicode folding is a
// widening here.
func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// peerIsLoopback reports whether the connection's peer is a loopback address.
//
// It fails CLOSED: an empty or unparseable RemoteAddr is not loopback. A handler
// invoked directly by a test has no peer, which is why the tests that drive the
// AllowHost arm set RemoteAddr explicitly.
func peerIsLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// CheckAllowHost validates the opt-in host at startup, so a value that could
// never match is a named startup error rather than a silent 403 an operator
// debugs against a phone.
//
// It is exported so `mora companion expose`, which prints the `--allow-host`
// argument an operator will paste, validates it against the SAME rule the
// listener will apply. A command that prints a value its own listener would
// refuse is worse than one that refuses to print.
//
// The grammar is an ALLOWLIST, not a list of forbidden characters. A published
// host is a DNS name and nothing else, so this accepts exactly:
//
//	<RFC 1123 hostname>[:port]
//	[<IPv6 literal>][:port]
//
// A hostname is at most 253 characters of dot-separated labels; a label is 1 to
// 63 characters of ASCII letters, digits and hyphens and may not begin or end
// with a hyphen. A port is 1 to 65535. Everything else — a scheme, a path, a
// space, userinfo, a wildcard, a comma, a trailing dot, a shell metacharacter —
// is refused here, before any value reaches a command line or a Host
// comparison. A blocklist was the previous shape and was wrong: it admitted
// `node.example;id`, which `mora companion expose` would then print inside a
// command an operator pastes into a shell.
func CheckAllowHost(allowHost string) (string, error) {
	if allowHost == "" {
		return "", nil
	}
	host, err := splitAllowHost(allowHost)
	if err != nil {
		return "", err
	}
	if equalFoldASCII(host, LoopbackHost) {
		return "", fmt.Errorf("%w (%q is already accepted; leave it empty)", ErrBadAllowHost, allowHost)
	}
	return allowHost, nil
}

// splitAllowHost validates the whole value and returns the host part.
func splitAllowHost(allowHost string) (string, error) {
	host := allowHost
	// An IPv6 literal is the one host shape that legitimately contains colons,
	// so it is matched first and by its brackets, which is the only unambiguous
	// way to tell "[::1]:80" from a hostname that happens to hold a colon.
	if strings.HasPrefix(allowHost, "[") {
		end := strings.Index(allowHost, "]")
		if end < 0 {
			return "", fmt.Errorf("%w (%q opens an IPv6 literal it never closes)", ErrBadAllowHost, allowHost)
		}
		literal := allowHost[1:end]
		ip := net.ParseIP(literal)
		if ip == nil || ip.To4() != nil {
			return "", fmt.Errorf("%w (%q is not an IPv6 literal)", ErrBadAllowHost, allowHost)
		}
		if err := checkAllowPortSuffix(allowHost, allowHost[end+1:]); err != nil {
			return "", err
		}
		return literal, nil
	}
	if i := strings.LastIndex(allowHost, ":"); i >= 0 {
		if err := checkAllowPortSuffix(allowHost, allowHost[i:]); err != nil {
			return "", err
		}
		host = allowHost[:i]
	}
	if err := checkDNSName(allowHost, host); err != nil {
		return "", err
	}
	return host, nil
}

// checkAllowPortSuffix validates the ":port" tail, which may be empty.
func checkAllowPortSuffix(allowHost, suffix string) error {
	if suffix == "" {
		return nil
	}
	if suffix[0] != ':' {
		return fmt.Errorf("%w (%q has trailing text after the host)", ErrBadAllowHost, allowHost)
	}
	digits := suffix[1:]
	if digits == "" {
		return fmt.Errorf("%w (%q ends in a colon with no port)", ErrBadAllowHost, allowHost)
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return fmt.Errorf("%w (%q has a non-numeric port)", ErrBadAllowHost, allowHost)
		}
	}
	port, err := strconv.Atoi(digits)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("%w (%q has a port outside 1-65535)", ErrBadAllowHost, allowHost)
	}
	return nil
}

// checkDNSName enforces RFC 1123 host syntax on the host part.
//
// A trailing dot is refused rather than trimmed. The comparison in hostAllowed
// is whole-string, so "name." and "name" are different allowed hosts; accepting
// both spellings here would silently pick one of them for the operator.
func checkDNSName(allowHost, host string) error {
	if host == "" {
		return fmt.Errorf("%w (%q has no host part)", ErrBadAllowHost, allowHost)
	}
	if len(host) > 253 {
		return fmt.Errorf("%w (a hostname is at most 253 characters, %q is %d)", ErrBadAllowHost, allowHost, len(host))
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" {
			return fmt.Errorf("%w (%q has an empty label; a leading or trailing dot is not a hostname)", ErrBadAllowHost, allowHost)
		}
		if len(label) > 63 {
			return fmt.Errorf("%w (%q has a label longer than 63 characters)", ErrBadAllowHost, allowHost)
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("%w (%q has a label that begins or ends with a hyphen)", ErrBadAllowHost, allowHost)
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			switch {
			case c >= 'a' && c <= 'z':
			case c >= 'A' && c <= 'Z':
			case c >= '0' && c <= '9':
			case c == '-':
			default:
				return fmt.Errorf("%w (%q is not a hostname: a label holds only ASCII letters, digits and hyphens)", ErrBadAllowHost, allowHost)
			}
		}
	}
	return nil
}

// sizeGuard caps the headers and the body. Both bounds exist because both are
// attacker-chosen: a phone that can send an unbounded header set or an unbounded
// body can exhaust this process without ever presenting a credential.
func (s *Server) sizeGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if headerBytes(r.Header)+len("Host: ")+len(r.Host)+2 > MaxHeaderBytes {
			writeOpaque(w, http.StatusRequestHeaderFieldsTooLarge, "headers_too_large")
			return
		}
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, MaxRequestBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// headerBytes sums the wire cost of the parsed header set: one "Key: value"
// line, with its separator and CRLF, per value.
//
// The caller adds the Host line separately. net/http promotes Host out of
// r.Header onto r.Host, so a request whose whole payload is an enormous Host
// value is invisible to this function — on a real listener MaxHeaderBytes
// catches it, but a handler reached directly would not, and the two bounds are
// supposed to agree.
func headerBytes(h http.Header) int {
	n := 0
	for key, values := range h {
		for _, v := range values {
			n += len(key) + len(v) + 4 // ": " and CRLF
		}
	}
	return n
}

// authGuard is the first of the two authorization checks.
//
// It runs before routing so an unauthenticated caller cannot learn which paths
// exist: every request without a live device token gets the same 401, whether
// the path is real or not.
//
// The one exemption is read out of routeDefs, not out of a second list here, so
// a route cannot become public by being added in the wrong place. It is keyed on
// the PATH rather than on method-and-path, so an unlisted method against the
// pairing route reaches its handler and gets the same 405-with-Allow every other
// route gives — a 401 there would claim the refusal was about a credential when
// the route has none.
//
// The exemption costs one thing and it is worth naming: the pairing path is
// discoverable without a token, because a 405 and a 404 are distinguishable. It
// is a published route printed by `mora companion pair`, so there was never a
// secret to keep. Nothing else about it is free — see pairing_route.go.
func (s *Server) authGuard(next http.Handler) http.Handler {
	public := map[string]bool{}
	for _, rt := range s.routeDefs() {
		if rt.Public {
			public[rt.Pattern] = true
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// r.URL.Path is the UNESCAPED path, and that is the right key because it
		// is the same form ServeMux matches on. Measured against this Go
		// (TestTheAuthExemptionAgreesWithTheMuxOnEncodedPaths):
		//
		//	/v1/companion/pairing/confirm     Path matches, the mux serves it
		//	/v1/companion/pairing/%63onfirm   Path matches, the mux ALSO serves
		//	                                  it — one route, two spellings, and
		//	                                  Go decides that, not this file
		//	/v1/companion/pairing%2Fconfirm   Path matches, the mux 404s: %2F is
		//	                                  one segment, so no pattern matches
		//	/v1/companion/%74oday             Path does NOT match, so the
		//	                                  credential check runs — an encoded
		//	                                  spelling of a PROTECTED route can
		//	                                  never be exempted
		//
		// The direction that would matter is a request the mux routes to a
		// protected handler while this map calls it public, and it cannot
		// happen: a mux match means the unescaped segments equal the pattern,
		// which means r.URL.Path IS that pattern. The %2F case is the only
		// asymmetry and it costs nothing — the exemption is granted and then
		// nothing is mounted there, so no handler runs.
		if public[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}
		if _, ok := s.authorize(r); !ok {
			writeUnauthorized(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authorize resolves the request's bearer token to a live device.
//
// It is deliberately branch-free on the way in: a missing header, an empty
// token and a malformed one all reach Registry.Authenticate, which hashes
// unconditionally and compares every stored fingerprint at full width. Refusing
// early on "there is no header" would answer a question about the credential
// before any comparison ran, which is the shape the N11 witness exists to
// forbid, and it would make the cheap failure distinguishable from the
// expensive one by a stopwatch.
func (s *Server) authorize(r *http.Request) (Device, bool) {
	dev, err := s.devices.Authenticate(bearerToken(r.Header.Get("Authorization")))
	if err != nil {
		return Device{}, false
	}
	return dev, true
}

// bearerToken strips the scheme, and REQUIRES it.
//
// A header without "Bearer " yields the empty string rather than the raw value,
// so a device token pasted bare into Authorization does not authenticate. That
// matters beyond tidiness: a client that works without the scheme will be
// written without it, and the next proxy, log or auth layer in the path is
// entitled to assume RFC 7235 framing. Accepting both shapes means the
// credential's wire form is whatever the first client happened to send.
//
// It never reports WHICH way it failed. An absent scheme returns "", the same
// as an empty token, and the caller hands that to Authenticate, which costs
// exactly what a real token costs — so the refusal is one path, not two.
func bearerToken(header string) string {
	const scheme = "bearer "
	if len(header) >= len(scheme) && strings.EqualFold(header[:len(scheme)], scheme) {
		return header[len(scheme):]
	}
	return ""
}

// requireDevice is the SECOND authorization check, at the handler boundary.
//
// A middleware chain is a claim about how a handler is mounted, and a mounting
// is one refactor away from being wrong: a route registered on the bare mux, a
// handler reused by a future transport, a test harness that skips the wrapper.
// Re-authenticating inside the handler makes the guarantee a property of the
// handler itself rather than of the assembly around it. The cost is one extra
// SHA-256 per request over a bounded device list.
func (s *Server) requireDevice(w http.ResponseWriter, r *http.Request) (Device, bool) {
	dev, ok := s.authorize(r)
	if !ok {
		writeUnauthorized(w)
		return Device{}, false
	}
	return dev, true
}

// admit is the handler boundary: method, then credential.
//
// The order is the point. The method check runs FIRST, before any credential
// work, because a method this route does not serve is not a question about who
// is asking — answering it with a 401 would be a lie, and doing the
// authentication first would mean an unlisted method still costs a hash.
//
// It deliberately does NOT take the work budget or start the clock. A route that
// does no kernel work should not be able to 503, and a route that reads a body
// should read it before it holds a slot. ok is false when a response has already
// been written.
func (s *Server) admit(w http.ResponseWriter, r *http.Request, method string) (Device, bool) {
	if r.Method != method {
		w.Header().Set("Allow", method)
		writeOpaque(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return Device{}, false
	}
	return s.requireDevice(w, r)
}

// acquire takes the work budget, or refuses immediately.
//
// Immediately, rather than blocking: a queue in front of an expensive read is a
// way to hold sockets and memory while the caller has already given up. A 503
// with a Retry-After is a smaller lie than a connection that eventually answers
// something stale.
//
// The returned release is idempotent. Handlers release the slot BEFORE writing
// the response body and again through a defer, because a slow client reading a
// 4 MiB projection down a phone's network must not be holding the Mac's only
// kernel slot while it does.
func (s *Server) acquire(w http.ResponseWriter) (func(), bool) {
	select {
	case s.kernel <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-s.kernel }) }, true
	default:
		w.Header().Set("Retry-After", strconv.Itoa(RetryAfterSeconds))
		writeOpaque(w, http.StatusServiceUnavailable, "busy")
		return nil, false
	}
}

// budgeted runs one kernel call under the work budget and the deadline.
//
// It exists so the slot's lifetime is exactly the kernel call: taken after the
// request is fully read and validated, released before a byte of the response is
// written. Everything outside that window is the listener's own cheap work or
// the client's network, and neither should be able to shut the other phone out.
func (s *Server) budgeted(w http.ResponseWriter, r *http.Request, call func(context.Context) error) bool {
	release, ok := s.acquire(w)
	if !ok {
		return false
	}
	// The slot is taken here, the kernel call happens here, and the slot goes
	// back BEFORE any response is written — the error response included. The
	// deferred release is the backstop for a panic; the explicit one is the
	// contract, because a deferred release alone would still hold the slot
	// across writeKernelFailure below. Holding it across any write means a
	// phone on a slow network keeps every other request out, and an error body
	// travels the same network a projection does.
	defer release()
	ctx, cancel := context.WithTimeout(r.Context(), s.kernelTimeout)
	defer cancel()
	err := call(ctx)
	release()
	if err != nil {
		writeKernelFailure(w, err)
		return false
	}
	return true
}

// writeKernelFailure turns a kernel error into an opaque answer.
//
// A deadline is reported as its own code so a client can tell "this Mac is
// busy, ask again" from "this read cannot be served", and both carry a
// Retry-After. Neither carries the kernel's error text: it can name a vault
// path, and a device has no use for it.
func writeKernelFailure(w http.ResponseWriter, err error) {
	w.Header().Set("Retry-After", strconv.Itoa(RetryAfterSeconds))
	if errors.Is(err, context.DeadlineExceeded) {
		writeOpaque(w, http.StatusServiceUnavailable, "timeout")
		return
	}
	// A capture whose key is already being processed is not a failure and is
	// not the kernel being unwell: the retry that follows finds the receipt the
	// holder settled. It gets its own code so a phone can say "still saving"
	// rather than "Mora is down", and the same Retry-After as the rest.
	if errors.Is(err, ErrCaptureInFlight) {
		writeOpaque(w, http.StatusServiceUnavailable, "in_flight")
		return
	}
	// The reservation store's HARD bound. It is its own code because the remedy
	// is different from every other 503 here: the Mac is not unwell and the
	// request is not malformed — too many captures are already claimed and
	// unfinished, and a client that keeps minting fresh keys is the pressure
	// rather than the victim. No reservation was created for this one.
	if errors.Is(err, ErrTooManyPending) {
		writeOpaque(w, http.StatusServiceUnavailable, "too_many_pending")
		return
	}
	writeOpaque(w, http.StatusServiceUnavailable, "unavailable")
}

// markSeen stamps last_seen_at, debounced in memory and best-effort.
//
// It is called from exactly one place: after a route has answered a device
// successfully. Not from the guard chain, which is where it used to live — a
// 405, a 503 and a rejected body all ran through that chain, so a client
// hammering an unsupported method could drive a durable write per request. A
// last-seen stamp records that a device was SERVED, and nothing else is a
// serving.
//
// Debounced because the stamp is a durable write behind the registry lock, and a
// write per request would serialize a phone's traffic behind its own audit
// trail. The window is per device and lives in memory: a restart loses the
// debounce, not the stamp. Best-effort because a read must not fail on account
// of a stamp — the device authenticated, the answer is owed, and a lock held by
// a concurrent `mora companion revoke` is a normal thing to lose a race to.
func (s *Server) markSeen(deviceID string) {
	now := s.now()
	s.mu.Lock()
	last, ok := s.seen[deviceID]
	if ok && now.Sub(last) < markSeenInterval {
		s.mu.Unlock()
		return
	}
	s.seen[deviceID] = now
	s.mu.Unlock()
	_ = s.devices.MarkSeen(deviceID)
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (s *Server) handleToday(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.admit(w, r, http.MethodGet)
	if !ok {
		return
	}
	var projection TodayProjection
	if !s.budgeted(w, r, func(ctx context.Context) error {
		var err error
		projection, err = s.reader.Today(ctx)
		return err
	}) {
		return
	}
	// The stamp records that a device was SERVED. A projection the contract
	// refuses is a 500, and a 500 served nothing — so the write is gated on the
	// response actually being a 2xx rather than on having got this far.
	if s.writePayload(w, &projection) {
		s.markSeen(dev.DeviceID)
	}
}

// handleHealth is deliberately OUTSIDE the work budget.
//
// Health is the route a phone reads when something looks wrong, and a health
// check that answers "too busy" is the one answer it must never give: a client
// cannot tell that from the Mac being down. It earns the exemption by being
// cheap — a health snapshot and one COUNT over the read-only index, no vault
// walk and no retrieval — so it cannot be the thing that makes the Mac busy.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.admit(w, r, http.MethodGet)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.kernelTimeout)
	defer cancel()
	projection, err := s.reader.Health(ctx)
	if err != nil {
		// A health route that answers 503 has told the phone nothing it could
		// not already infer from the socket. Health's whole job is to report
		// state, and "the kernel could not tell me" IS a state — so the failure
		// becomes a projection that says unhealthy, rather than an absence of
		// one. The alternative asks a client to distinguish "Mora is unwell"
		// from "Mora is not there", which is exactly what this route exists to
		// let it do.
		projection = degradedHealth(err)
	}
	if s.writePayload(w, &projection) {
		s.markSeen(dev.DeviceID)
	}
}

// kernelSourceKey names the kernel itself in a freshness row.
//
// It is not a connector, and it is deliberately distinguishable from one: the
// thing that failed is Mora's own read path, and attributing that to gmail or
// imessage would be a lie about which part of the system is unwell.
const kernelSourceKey = "mora.kernel"

// degradedHealth is the projection a health request gets when the kernel could
// not produce one.
//
// Everything it claims is something this function actually knows. The state is
// unhealthy because a kernel that cannot answer is not healthy. The policy is
// readonly because a kernel in that condition must not be told a write would be
// accepted — the safe direction is the one that refuses. The index is unhealthy
// with no memory count and no built-at, because nothing here read the index. The
// only row is the kernel's own, carrying the typed reason.
func degradedHealth(err error) HealthProjection {
	out := NewHealthProjection()
	out.GeneratedAt = time.Now().UTC().Truncate(time.Second).Format(time.RFC3339)
	out.State = HealthUnhealthy
	out.Policy = PolicyReadonly
	out.Index = IndexHealth{State: HealthUnhealthy}
	out.Sources = []SourceFreshness{{
		Key:        kernelSourceKey,
		State:      FreshnessFailed,
		AgeSeconds: -1,
		ErrorCode:  kernelErrorCode(err),
	}}
	return out
}

// kernelErrorCode maps a kernel failure onto the frozen vocabulary. A deadline
// is the kernel being unreachable in time, which is what source_unavailable
// says; anything else is internal, because a code this package cannot explain
// must not be guessed at.
func kernelErrorCode(err error) SourceErrorCode {
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrSourceUnavailable
	}
	return ErrInternal
}

func (s *Server) handleContext(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.admit(w, r, http.MethodPost)
	if !ok {
		return
	}
	// The body is read and validated BEFORE the budget is taken. A slow client
	// dribbling a request body must not hold the Mac's only kernel slot while it
	// does, and a malformed body should cost a decode rather than a slot.
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxRequestBytes+1))
	if err != nil {
		writeOpaque(w, http.StatusRequestEntityTooLarge, CodeTooLarge)
		return
	}
	if len(body) > MaxRequestBytes {
		writeOpaque(w, http.StatusRequestEntityTooLarge, CodeTooLarge)
		return
	}
	// Strict inbound: unknown fields, unknown enum values, oversize text and
	// malformed timestamps are all rejected here rather than normalized. The
	// error carries the schema CODE and never the value that failed, so a
	// rejection cannot echo a device's query back through an error body.
	//
	// The zero value is load-bearing. Decoding into NewContextRequest() would
	// pre-fill schema and schema_version, so a body that omitted them inherited
	// the right answer from the constructor and passed a check it never faced:
	// {"mode":"think","query":"x"} was accepted as a v1 context request. A zero
	// Header fails Header.validate with schema_mismatch, which is what "strict
	// inbound" was supposed to mean.
	var req ContextRequest
	if err := Unmarshal(body, &req); err != nil {
		var schemaErr *Error
		if errors.As(err, &schemaErr) {
			writeRejection(w, http.StatusBadRequest, schemaErr)
			return
		}
		writeOpaque(w, http.StatusBadRequest, CodeMalformed)
		return
	}

	var bundle ContextBundle
	if !s.budgeted(w, r, func(ctx context.Context) error {
		var err error
		bundle, err = s.reader.Context(ctx, req)
		return err
	}) {
		return
	}
	if s.writePayload(w, &bundle) {
		s.markSeen(dev.DeviceID)
	}
}

// writePayload marshals through Marshal, which validates.
//
// Validating on the way out is what stops a kernel bug from becoming a wire
// contract violation: a projection with a freshness row whose age disagrees with
// its own timestamps, or an item with no evidence, is a lie a phone would
// render, so it becomes a 500 with nothing in it instead. Tolerant outbound
// applies to the DEVICE's decoder, not to this producer.
func (s *Server) writePayload(w http.ResponseWriter, v Payload) bool {
	body, err := Marshal(v)
	if err != nil {
		writeOpaque(w, http.StatusInternalServerError, "internal")
		return false
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
	return true
}

// ---------------------------------------------------------------------------
// Responses
// ---------------------------------------------------------------------------

// unauthorizedBody is the ONE answer to every credential failure: no token, a
// malformed token, a token for a device that was never paired, and a token for
// a device that was revoked. An operator learns which from `mora companion
// list`; a caller holding a bad token learns nothing at all, so a stolen token
// cannot be classified by probing.
const unauthorizedBody = "{\n  \"error\": \"unauthorized\"\n}\n"

func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// No WWW-Authenticate challenge: the header's realm and error parameters
	// are exactly the discrimination this response exists not to give.
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = io.WriteString(w, unauthorizedBody)
}

func writeOpaque(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "{\n  \"error\": %q\n}\n", code)
}

// writeRejection reports a schema refusal. It carries the code and the field
// path — both of which are this package's own vocabulary — and never the
// offending value.
func writeRejection(w http.ResponseWriter, status int, err *Error) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err.Field == "" {
		_, _ = fmt.Fprintf(w, "{\n  \"error\": %q\n}\n", err.Code)
		return
	}
	_, _ = fmt.Fprintf(w, "{\n  \"error\": %q,\n  \"field\": %q\n}\n", err.Code, err.Field)
}

// ---------------------------------------------------------------------------
// Serving
// ---------------------------------------------------------------------------

// Serve listens on the validated loopback address until ctx is done.
//
// The address is re-checked here. NewServer already refused a non-loopback
// address, and checking again costs nothing and closes the one path that would
// otherwise matter: a Server value assembled by some future constructor that
// forgot to.
func (s *Server) Serve(ctx context.Context) error {
	if err := checkLoopbackAddr(s.addr); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("companion listen on %s: %w (is another `mora companion serve` already running?)", s.addr, err)
	}
	hs := &http.Server{
		Handler:           s.Handler(),
		MaxHeaderBytes:    MaxHeaderBytes,
		ReadHeaderTimeout: ServerReadHeaderTimeout,
		ReadTimeout:       ServerReadTimeout,
		WriteTimeout:      ServerWriteTimeout,
		IdleTimeout:       ServerIdleTimeout,
		// ErrorLog is left nil on purpose: net/http's default logger writes
		// request-derived text to standard error, and this listener logs
		// nothing a device sent.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hs.Shutdown(shutdownCtx)
	}()
	fmt.Fprintf(s.log, "mora companion serve listening on http://%s/  (loopback only)\n", ln.Addr().String())
	fmt.Fprintf(s.log, "  routes: GET %s, POST %s, GET %s, POST %s\n", RouteToday, RouteContext, RouteHealth, RouteCapture)
	fmt.Fprintf(s.log, "  pairing: POST %s (the one route that takes no token; it is where a one-time code is spent)\n", RouteConfirm)
	fmt.Fprintln(s.log, "  credential: a device token from `mora companion pair` — the loopback API token is not accepted here")
	if s.allowHost != "" {
		// The name itself is deliberately absent. The operator typed it on the
		// command line one line above, `tailscale serve status` repeats it, and
		// a banner that does not carry it is a log an operator can paste into a
		// bug report without redacting a machine name.
		fmt.Fprintln(s.log, "  published: one extra Host value is accepted, and only from a loopback peer (a reverse proxy on this Mac)")
	}
	if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
