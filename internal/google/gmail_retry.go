package google

import (
	"context"
	"errors"
	"time"

	"google.golang.org/api/googleapi"
)

func waitGoogle(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func retryableGmail(err error) bool {
	var api *googleapi.Error
	if !errors.As(err, &api) {
		return false
	}
	if api.Code == 429 || api.Code >= 500 {
		return true
	}
	if api.Code == 403 {
		for _, e := range api.Errors {
			if e.Reason == "rateLimitExceeded" || e.Reason == "userRateLimitExceeded" {
				return true
			}
		}
	}
	return false
}

// Bounded, cancellable retry at the individual request: a late-page rate limit
// must not discard and refetch every preceding thread on each attempt.
func gmailRequest[T any](ctx context.Context, wait func(context.Context, time.Duration) error, request func(...googleapi.CallOption) (T, error)) (T, error) {
	for attempt := 0; ; attempt++ {
		value, err := request()
		if err == nil || !retryableGmail(err) || attempt >= 6 {
			return value, err
		}
		if wait != nil {
			if e := wait(ctx, time.Second*time.Duration(1<<attempt)); e != nil {
				var zero T
				return zero, e
			}
		}
		if ctx.Err() != nil {
			var zero T
			return zero, ctx.Err()
		}
	}
}
