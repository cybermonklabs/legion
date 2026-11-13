package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"golang.org/x/time/rate"
)

// requestsPerSecond bounds the reads fetch makes of the live Dispatch.
const requestsPerSecond = 20

// retryWaits are the waits before each retry of a read that met a 5xx, a 429 or a network
// error; a read is tried len(retryWaits)+1 times in all.
var retryWaits = []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}

// errNotFound is a read Dispatch answered 404.
var errNotFound = errors.New("not found")

// statusError is a read Dispatch refused with a status other than 404.
type statusError struct {
	status int
	body   string
}

func (e *statusError) Error() string { return fmt.Sprintf("status %d: %s", e.status, e.body) }

// unreadable is whether err is a read of a text gone from Dispatch, or one this credential may
// not make: a 404 or a 403, which no retry clears.
func unreadable(err error) bool {
	var status *statusError
	return errors.Is(err, errNotFound) || errors.As(err, &status) && status.status == http.StatusForbidden
}

// client reads the Dispatch API with a bearer token, at most requestsPerSecond reads a second,
// retrying the failures a retry can clear.
type client struct {
	base      string
	tokenFile string
	http      *http.Client
	limit     *rate.Limiter
	requests  int
}

func newClient(base, tokenFile string) *client {
	return &client{
		base:      strings.TrimRight(base, "/"),
		tokenFile: tokenFile,
		http:      &http.Client{Timeout: 60 * time.Second},
		limit:     rate.NewLimiter(requestsPerSecond, 1),
	}
}

// get reads path (with its query) into out.
func (c *client) get(ctx context.Context, path string, query url.Values, out any) error {
	target := c.base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	for attempt := 0; ; attempt++ {
		err := c.once(ctx, target, out)
		var status *statusError
		retry := err != nil && !errors.Is(err, errNotFound) && ctx.Err() == nil &&
			(!errors.As(err, &status) || status.status >= 500 || status.status == http.StatusTooManyRequests)
		if !retry || attempt == len(retryWaits) {
			return err
		}
		select {
		case <-time.After(retryWaits[attempt]):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (c *client) once(ctx context.Context, target string, out any) error {
	if err := c.limit.Wait(ctx); err != nil {
		return err
	}
	c.requests++
	token, err := os.ReadFile(c.tokenFile)
	if err != nil {
		return fmt.Errorf("reading the Dispatch token file: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		_, _ = io.Copy(io.Discard, resp.Body)
		return errNotFound
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &statusError{status: resp.StatusCode, body: strings.TrimSpace(string(body))}
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decoding %s: %w", target, err)
	}
	return nil
}
