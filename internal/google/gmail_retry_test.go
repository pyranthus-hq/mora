package google

import (
	"context"
	"errors"
	"google.golang.org/api/googleapi"
	"testing"
	"time"
)

func TestGmailQuotaRetryIsBoundedAndDoesNotRetryAuth(t *testing.T) {
	for _, tc := range []struct {
		code   int
		reason string
		want   int
	}{{403, "rateLimitExceeded", 7}, {429, "", 7}, {503, "", 7}, {403, "forbidden", 1}, {401, "", 1}} {
		calls, waits := 0, 0
		_, err := gmailRequest(context.Background(), func(context.Context, time.Duration) error { waits++; return nil }, func(...googleapi.CallOption) (int, error) {
			calls++
			return 0, &googleapi.Error{Code: tc.code, Errors: []googleapi.ErrorItem{{Reason: tc.reason}}}
		})
		if err == nil || calls != tc.want || waits != tc.want-1 {
			t.Fatalf("%+v calls=%d waits=%d err=%v", tc, calls, waits, err)
		}
	}
}

func TestGmailRetryCancellationStopsRequests(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	_, err := gmailRequest(ctx, func(ctx context.Context, _ time.Duration) error { cancel(); return waitGoogle(ctx, time.Hour) }, func(...googleapi.CallOption) (int, error) { calls++; return 0, &googleapi.Error{Code: 429} })
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatal(calls, err)
	}
}
