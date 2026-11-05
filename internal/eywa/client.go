package eywa

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// protocolVersion is the MCP revision Mora asks for; the server's answer is
// echoed back on every later request, as the streamable HTTP transport requires.
const protocolVersion = "2025-06-18"

// DefaultMaxRetryWait bounds how long one call may sleep for a Retry-After
// before it gives up and reports the rate limit instead.
const DefaultMaxRetryWait = 30 * time.Second

const maxResponseBytes = 16 << 20

// JSON-RPC error codes the service documents for its agent tools.
const (
	rpcForbidden     = -32001
	rpcThrottled     = -32002
	rpcUnavailable   = -32004
	rpcInvalidParams = -32602
)

// Client is a minimal MCP client over streamable HTTP with an OAuth2
// client_credentials bearer token. It is safe for sequential use by one sync or
// push; calls are serialized.
type Client struct {
	http         *http.Client
	cfg          Config
	now          func() time.Time
	sleep        func(context.Context, time.Duration) error
	maxRetryWait time.Duration
	version      string

	mu          sync.Mutex
	token       string
	tokenExpiry time.Time
	session     string
	negotiated  string
	initialized bool
	toolNames   map[string]string
	nextID      int64
}

// ClientOptions are the deterministic test seams. Zero values mean production
// behavior.
type ClientOptions struct {
	HTTP         *http.Client
	Now          func() time.Time
	Sleep        func(context.Context, time.Duration) error
	MaxRetryWait time.Duration
	Version      string // client version reported in initialize
}

// NewClient validates the configuration and returns a client. It performs no
// network call.
func NewClient(cfg Config, opts ClientOptions) (*Client, error) {
	if err := ValidateEndpoints(cfg.Endpoint, cfg.TokenURL, cfg.ClientID); err != nil {
		return nil, err
	}
	if cfg.ClientSecret == "" {
		return nil, fmt.Errorf("eywa: %s is not set — export the agent's client secret for this command; Mora never stores it", SecretEnv)
	}
	c := &Client{http: opts.HTTP, cfg: cfg, now: opts.Now, sleep: opts.Sleep, maxRetryWait: opts.MaxRetryWait, version: opts.Version}
	if c.http == nil {
		c.http = &http.Client{Timeout: 60 * time.Second}
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.sleep == nil {
		c.sleep = sleepContext
	}
	if c.maxRetryWait <= 0 {
		c.maxRetryWait = DefaultMaxRetryWait
	}
	if c.version == "" {
		c.version = "dev"
	}
	return c, nil
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// accessToken returns a cached token or fetches a new one with the
// client_credentials grant (HTTP Basic client authentication).
func (c *Client) accessToken(ctx context.Context, force bool) (string, error) {
	if !force && c.token != "" && c.now().Before(c.tokenExpiry) {
		return c.token, nil
	}
	form := url.Values{"grant_type": {"client_credentials"}}
	if c.cfg.OAuthScope != "" {
		form.Set("scope", c.cfg.OAuthScope)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(url.QueryEscape(c.cfg.ClientID), url.QueryEscape(c.cfg.ClientSecret))
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("eywa: token request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return "", &RateLimitError{RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), c.now())}
	case resp.StatusCode >= 500:
		return "", &UnavailableError{Status: resp.StatusCode, Detail: "token endpoint"}
	case resp.StatusCode != http.StatusOK:
		// 400 invalid_client / invalid_scope and 401 are all a refusal of these
		// credentials; the response body is never echoed (it can name the client).
		return "", &AuthError{Status: resp.StatusCode, Stage: "token"}
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return "", fmt.Errorf("eywa: decoding token response: %w", err)
	}
	if tok.AccessToken == "" || (tok.TokenType != "" && !strings.EqualFold(tok.TokenType, "bearer")) {
		return "", errors.New("eywa: the token endpoint returned no bearer access token")
	}
	life := time.Duration(tok.ExpiresIn) * time.Second
	if life <= 0 {
		life = 5 * time.Minute
	}
	// Refresh a minute early so a long sync never sends an expiring token.
	if life > 2*time.Minute {
		life -= time.Minute
	}
	c.token, c.tokenExpiry = tok.AccessToken, c.now().Add(life)
	return c.token, nil
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int64 `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcErrorBody struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcErrorBody   `json:"error"`
}

// post sends one JSON-RPC message. A nil id is a notification. It refreshes the
// token once on a 401 and re-initializes once when the server forgets the session.
func (c *Client) post(ctx context.Context, method string, params any, notify bool) (json.RawMessage, error) {
	var id *int64
	if !notify {
		c.nextID++
		n := c.nextID
		id = &n
	}
	payload, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	refreshed := false
	for {
		token, err := c.accessToken(ctx, false)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.Endpoint, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Authorization", "Bearer "+token)
		if c.negotiated != "" {
			req.Header.Set("MCP-Protocol-Version", c.negotiated)
		}
		if c.session != "" {
			req.Header.Set("Mcp-Session-Id", c.session)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("eywa: request failed: %w", err)
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("eywa: reading response: %w", readErr)
		}
		if s := resp.Header.Get("Mcp-Session-Id"); s != "" {
			c.session = s
		}
		switch {
		case resp.StatusCode == http.StatusUnauthorized && !refreshed:
			refreshed = true
			if _, err := c.accessToken(ctx, true); err != nil {
				return nil, err
			}
			continue
		case resp.StatusCode == http.StatusUnauthorized:
			return nil, &AuthError{Status: 401, Stage: "mcp"}
		case resp.StatusCode == http.StatusForbidden:
			return nil, &AuthError{Status: 403, Stage: "mcp"}
		case resp.StatusCode == http.StatusTooManyRequests:
			return nil, &RateLimitError{RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), c.now())}
		case resp.StatusCode == http.StatusNotFound && c.session != "" && method != "initialize":
			// The server dropped the session: start a new one, once.
			return nil, errSessionExpired
		case resp.StatusCode >= 500:
			return nil, &UnavailableError{Status: resp.StatusCode, RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), c.now())}
		case notify && (resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNoContent):
			return nil, nil
		case resp.StatusCode != http.StatusOK:
			return nil, fmt.Errorf("eywa: MCP endpoint returned HTTP %d", resp.StatusCode)
		}
		msg, err := decodeRPC(resp.Header.Get("Content-Type"), raw, id)
		if err != nil {
			return nil, err
		}
		if msg.Error != nil {
			return nil, rpcFailure(msg.Error, c.now())
		}
		return msg.Result, nil
	}
}

var errSessionExpired = errors.New("eywa: MCP session expired")

// decodeRPC reads a JSON body or a text/event-stream body and returns the
// response whose id matches the request.
func decodeRPC(contentType string, raw []byte, id *int64) (rpcResponse, error) {
	mt, _, _ := mime.ParseMediaType(contentType)
	var candidates [][]byte
	if mt == "text/event-stream" {
		sc := bufio.NewScanner(bytes.NewReader(raw))
		sc.Buffer(make([]byte, 64*1024), maxResponseBytes)
		var data []string
		flush := func() {
			if len(data) > 0 {
				candidates = append(candidates, []byte(strings.Join(data, "\n")))
				data = nil
			}
		}
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				flush()
			case strings.HasPrefix(line, "data:"):
				data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			}
		}
		if err := sc.Err(); err != nil {
			return rpcResponse{}, fmt.Errorf("eywa: reading event stream: %w", err)
		}
		flush()
	} else {
		candidates = [][]byte{raw}
	}
	want := ""
	if id != nil {
		want = strconv.FormatInt(*id, 10)
	}
	for _, cand := range candidates {
		var msg rpcResponse
		if err := json.Unmarshal(cand, &msg); err != nil {
			if mt != "text/event-stream" {
				return rpcResponse{}, fmt.Errorf("eywa: decoding MCP response: %w", err)
			}
			continue
		}
		if msg.Result == nil && msg.Error == nil {
			continue // a server request or notification, not our answer
		}
		if want == "" || strings.Trim(string(msg.ID), `"`) == want {
			return msg, nil
		}
	}
	return rpcResponse{}, errors.New("eywa: the MCP response carried no answer to the request")
}

func rpcFailure(e *rpcErrorBody, now time.Time) error {
	var data struct {
		RetryAfter json.RawMessage `json:"retry_after"`
	}
	_ = json.Unmarshal(e.Data, &data)
	retry := parseRetryAfter(strings.Trim(string(data.RetryAfter), `"`), now)
	switch e.Code {
	case rpcForbidden:
		return &ForbiddenError{Message: e.Message}
	case rpcThrottled:
		return &RateLimitError{RetryAfter: retry}
	case rpcUnavailable:
		return &UnavailableError{RetryAfter: retry, Detail: e.Message}
	default:
		return &RPCError{Code: e.Code, Message: e.Message}
	}
}

// parseRetryAfter accepts delta-seconds or an HTTP date.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" || v == "null" {
		return 0
	}
	if n, err := strconv.ParseFloat(v, 64); err == nil && n >= 0 {
		return time.Duration(n * float64(time.Second))
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

func (c *Client) ensureSession(ctx context.Context) error {
	if c.initialized {
		return nil
	}
	c.session, c.negotiated = "", ""
	params := map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "mora", "version": c.version},
	}
	res, err := c.post(ctx, "initialize", params, false)
	if err != nil {
		return asAgentFailure(err)
	}
	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(res, &init); err != nil {
		return fmt.Errorf("eywa: decoding initialize result: %w", err)
	}
	c.negotiated = init.ProtocolVersion
	if c.negotiated == "" {
		c.negotiated = protocolVersion
	}
	if _, err := c.post(ctx, "notifications/initialized", nil, true); err != nil {
		return asAgentFailure(err)
	}
	res, err = c.post(ctx, "tools/list", map[string]any{}, false)
	if err != nil {
		return asAgentFailure(err)
	}
	var list struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(res, &list); err != nil {
		return fmt.Errorf("eywa: decoding tools/list: %w", err)
	}
	// A gateway may publish a tool as "<target>___<tool>"; resolve by the bare name.
	c.toolNames = map[string]string{}
	for _, t := range list.Tools {
		bare := t.Name
		if i := strings.LastIndex(bare, "___"); i >= 0 {
			bare = bare[i+3:]
		}
		if _, dup := c.toolNames[bare]; !dup {
			c.toolNames[bare] = t.Name
		}
	}
	c.initialized = true
	return nil
}

// asAgentFailure turns a Forbidden on a session-level call into an AuthError:
// the agent itself was refused (revoked, unknown, or no scope).
func asAgentFailure(err error) error {
	var fe *ForbiddenError
	if errors.As(err, &fe) {
		return &AuthError{Status: 403, Stage: "mcp", Detail: "Forbidden"}
	}
	return err
}

// CallTool invokes one tool and decodes its structured result into out. A
// Retry-After within the bounded wait is honored once; a longer one is
// returned as a RateLimitError.
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any, out any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	waited := false
	resetSession := false
	for {
		err := c.callOnce(ctx, name, args, out)
		if errors.Is(err, errSessionExpired) && !resetSession {
			resetSession = true
			c.initialized = false
			continue
		}
		if !waited {
			if d, ok := retryDelay(err); ok && d <= c.maxRetryWait {
				waited = true
				if d <= 0 {
					d = time.Second
				}
				if serr := c.sleep(ctx, d); serr != nil {
					return serr
				}
				continue
			}
		}
		return err
	}
}

func retryDelay(err error) (time.Duration, bool) {
	var rl *RateLimitError
	if errors.As(err, &rl) {
		return rl.RetryAfter, true
	}
	var un *UnavailableError
	if errors.As(err, &un) {
		return un.RetryAfter, true
	}
	return 0, false
}

func (c *Client) callOnce(ctx context.Context, name string, args map[string]any, out any) error {
	if err := c.ensureSession(ctx); err != nil {
		return err
	}
	full, ok := c.toolNames[name]
	if !ok {
		return fmt.Errorf("eywa: the MCP endpoint does not offer the %q tool to this agent", name)
	}
	if args == nil {
		args = map[string]any{}
	}
	res, err := c.post(ctx, "tools/call", map[string]any{"name": full, "arguments": args}, false)
	if err != nil {
		return err
	}
	return decodeToolResult(res, out)
}

func decodeToolResult(res json.RawMessage, out any) error {
	var r struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		IsError           bool            `json:"isError"`
	}
	if err := json.Unmarshal(res, &r); err != nil {
		return fmt.Errorf("eywa: decoding tool result: %w", err)
	}
	text := ""
	for _, c := range r.Content {
		if c.Type == "text" {
			text = c.Text
			break
		}
	}
	if r.IsError {
		var se struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(r.StructuredContent, &se)
		msg := se.Error.Message
		if msg == "" {
			msg = text
		}
		return &ToolError{Code: se.Error.Code, Message: msg}
	}
	payload := []byte(r.StructuredContent)
	if len(bytes.TrimSpace(payload)) == 0 || string(bytes.TrimSpace(payload)) == "null" {
		payload = []byte(text)
	}
	// Defensive: an error envelope the gateway did not translate.
	var env struct {
		EywaError *rpcErrorBody `json:"eywa_error"`
		ToolError *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"eywa_tool_error"`
	}
	if json.Unmarshal(payload, &env) == nil {
		if env.EywaError != nil {
			return rpcFailure(env.EywaError, time.Now())
		}
		if env.ToolError != nil {
			return &ToolError{Code: env.ToolError.Code, Message: env.ToolError.Message}
		}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("eywa: decoding tool payload: %w", err)
	}
	return nil
}
