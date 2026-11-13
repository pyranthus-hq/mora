package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"
)

const (
	rootsRequestTimeout = time.Second
	maxRootsReplyBytes  = 32 << 10
	maxClientRoots      = 64
)

var (
	// ErrRootsUnavailable means the client did not declare the roots capability.
	ErrRootsUnavailable = errors.New("MCP client roots unavailable")
	// ErrRootsRequestFailed hides errors containing client or filesystem details.
	ErrRootsRequestFailed = errors.New("MCP client roots request failed")
	// ErrInvalidRootsReply means the response exceeded a limit or was malformed.
	ErrInvalidRootsReply = errors.New("invalid MCP client roots response")
)

type rootsSessionKey struct{}
type rootsSession struct {
	client    *clientTransport
	supported bool
}

func declaresRoots(params json.RawMessage) bool {
	var p struct {
		Capabilities struct {
			Roots map[string]json.RawMessage `json:"roots"`
		}
	}
	return json.Unmarshal(params, &p) == nil && p.Capabilities.Roots != nil
}

// Root is a client-reported file URI and an optional display name.
type Root struct {
	URI  string `json:"uri"`
	Name string `json:"name,omitempty"`
}

// ListClientRoots requests the current roots from the initialized stdio
// connection carried by ctx. The caller still owns path and scope validation.
func ListClientRoots(ctx context.Context) ([]Root, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	session, ok := ctx.Value(rootsSessionKey{}).(rootsSession)
	if !ok {
		return nil, ErrNoClientChannel
	}
	if !session.supported {
		return nil, ErrRootsUnavailable
	}
	raw, err := session.client.call(ctx, "roots/list", nil, rootsRequestTimeout)
	switch err {
	case nil:
	case ErrNoClientChannel, context.Canceled, context.DeadlineExceeded:
		return nil, err
	default:
		return nil, ErrRootsRequestFailed
	}
	var reply struct {
		Roots []Root `json:"roots"`
	}
	if len(raw) > maxRootsReplyBytes || json.Unmarshal(raw, &reply) != nil || reply.Roots == nil || len(reply.Roots) > maxClientRoots {
		return nil, ErrInvalidRootsReply
	}
	for _, root := range reply.Roots {
		uri, err := url.Parse(root.URI)
		if err != nil || !strings.HasPrefix(root.URI, "file://") || uri.Path == "" || uri.User != nil || strings.ContainsAny(root.URI, "?#") {
			return nil, ErrInvalidRootsReply
		}
	}
	return reply.Roots, nil
}
