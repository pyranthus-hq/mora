// Package eywa implements Mora's connector for a governed company memory
// service with an agent MCP API ("Eywa"). It pulls admitted facts into the vault
// as read-only evidence and, only on an explicit owner action, pushes authored
// memories to the service as governed agent updates.
//
// The package imports only internal/memory (AGENTS.md rule 1). It never stores
// the client secret: the caller passes it in from the environment at run time.
package eywa

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
)

// KindFact is the item kind of one pulled fact version. Its StableID is
// eywa_fact/<version_id>: provider identity only, never content.
const KindFact memory.ItemKind = "eywa_fact"

// Provider is the provider string stamped on every pulled record.
const Provider = "eywa"

// SecretEnv names the environment variable that carries the OAuth2 client
// secret. Mora reads it at run time and never writes it anywhere.
const SecretEnv = "MORA_EYWA_CLIENT_SECRET"

func init() { memory.RegisterKind(KindFact, "fact", Provider) }

// Config is everything the connector needs to reach the service. Endpoint,
// TokenURL and ClientID come from the persisted source row; ClientSecret comes
// from SecretEnv. There are no defaults: every value is the user's.
type Config struct {
	Endpoint     string // MCP endpoint (streamable HTTP)
	TokenURL     string // OAuth2 token endpoint (client_credentials grant)
	ClientID     string
	ClientSecret string
	OAuthScope   string // optional OAuth2 scope the token endpoint requires
	AgentID      string // push only: the agent id the service registered for this client
	Topic        string // push only: the agent's scope topic
}

// ValidateEndpoints checks the two URLs without contacting anything. HTTPS is
// required; plain HTTP is accepted only for a loopback host (local testing).
func ValidateEndpoints(endpoint, tokenURL, clientID string) error {
	if err := validateURL("endpoint", endpoint); err != nil {
		return err
	}
	if err := validateURL("token URL", tokenURL); err != nil {
		return err
	}
	if strings.TrimSpace(clientID) == "" {
		return errors.New("eywa: a client id is required")
	}
	return nil
}

func validateURL(what, raw string) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("eywa: the %s is required", what)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("eywa: the %s %q is not an absolute URL", what, raw)
	}
	if u.User != nil {
		return fmt.Errorf("eywa: the %s must not carry credentials", what)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if isLoopback(u.Hostname()) {
			return nil
		}
		return fmt.Errorf("eywa: the %s must use https (plain http is allowed only for a loopback host)", what)
	default:
		return fmt.Errorf("eywa: the %s must use https", what)
	}
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// AuthError is a 401/403-class failure: the token endpoint refused the client,
// the MCP endpoint refused the token, or the service answered Forbidden for the
// agent itself. It is never retried automatically.
type AuthError struct {
	Status int    // HTTP status, or 403 for a JSON-RPC Forbidden
	Stage  string // "token" or "mcp"
	Detail string
}

func (e *AuthError) Error() string {
	switch {
	case e.Stage == "token":
		return fmt.Sprintf("eywa: the token endpoint refused the client credentials (HTTP %d) — check the client id and %s", e.Status, SecretEnv)
	case e.Status == 401:
		return "eywa: the MCP endpoint refused the access token (HTTP 401) — check the client id, the OAuth scope and " + SecretEnv
	default:
		detail := ""
		if e.Detail != "" {
			detail = ": " + e.Detail
		}
		return fmt.Sprintf("eywa: permission denied (HTTP %d%s) — the agent may be revoked, unknown, or outside its scope topic", e.Status, detail)
	}
}

// ConnectorUnauthorized lets Mora's structural error taxonomy type this
// failure as connector.unauthorized without matching on prose.
func (e *AuthError) ConnectorUnauthorized() bool { return true }

// RateLimitError is a 429 (HTTP) or Throttled (JSON-RPC -32002) answer that
// outlasted the bounded in-process wait. RetryAfter is what the service asked for.
type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("eywa: rate limited (retry after %s)", e.RetryAfter.Round(time.Second))
	}
	return "eywa: rate limited (no Retry-After given)"
}

// ConnectorUnavailable types the failure as connector.unavailable (retryable later).
func (e *RateLimitError) ConnectorUnavailable() bool { return true }

// UnavailableError is a 5xx or a service-declared temporary failure (-32004).
type UnavailableError struct {
	Status     int
	RetryAfter time.Duration
	Detail     string
}

func (e *UnavailableError) Error() string {
	s := "eywa: the service is temporarily unavailable"
	if e.Status > 0 {
		s += fmt.Sprintf(" (HTTP %d)", e.Status)
	}
	if e.Detail != "" {
		s += ": " + e.Detail
	}
	if e.RetryAfter > 0 {
		s += fmt.Sprintf(" (retry after %s)", e.RetryAfter.Round(time.Second))
	}
	return s
}

// ConnectorUnavailable types the failure as connector.unavailable.
func (e *UnavailableError) ConnectorUnavailable() bool { return true }

// RPCError is any other JSON-RPC error the endpoint returned.
type RPCError struct {
	Code    int
	Message string
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("eywa: MCP error %d: %s", e.Code, e.Message)
}

// ToolError is a tool RESULT with isError: true (for example a guardrail block
// or an invalid lookup). Nothing was stored or answered.
type ToolError struct {
	Code    string
	Message string
}

func (e *ToolError) Error() string {
	if e.Code == "" {
		return "eywa: tool error: " + e.Message
	}
	return fmt.Sprintf("eywa: tool error %s: %s", e.Code, e.Message)
}

// ForbiddenError is a JSON-RPC Forbidden (-32001) answer to one lookup. The
// service uses one uniform answer for out of scope, not visible, unknown and no
// current version, so per record it is reported, not guessed at.
type ForbiddenError struct{ Message string }

func (e *ForbiddenError) Error() string {
	if e.Message == "" {
		return "eywa: forbidden"
	}
	return "eywa: forbidden: " + e.Message
}
