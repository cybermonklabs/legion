package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/urljoin"
)

// The dashboard streams a Dispatch build serves. A build from #2449 on gives a tab one live socket
// (/ws/live) and serves no SSE; one before it serves the SSE feed (GET /api/v1/events) and no live
// socket. The replay measures builds of both kinds, so the harness opens whichever the build under
// test serves and records it in summary.json as config.dashboard.
const (
	dashboardLive = "live"
	dashboardSSE  = "sse"
)

// probeDashboard asks the Dispatch at dispatch which dashboard stream it serves, once, before the
// window: it dials the live socket, and, when that is refused, asks for the SSE feed. It returns
// the kind (dashboardLive or dashboardSSE) and the URL the step's streams open. A build that
// serves neither is an error naming what each answered: the step would measure no frames. A URL
// that does not join, or whose joined URL does not parse again (url.URL.String can write text
// url.Parse refuses, such as an IPv6 zone escape), is refused in fixed words: url.Parse's error,
// which http.NewRequest's carries, quotes the URL whole, user information and query included.
func probeDashboard(ctx context.Context, dispatch string) (string, string, error) {
	joined, err := urljoin.Join(dispatch, ssePath)
	if err != nil {
		return "", "", errors.New("the Dispatch URL is not an absolute URL with a host (it is not printed)")
	}
	sseURL := joined.String()
	liveURL, err := liveSocketURL(dispatch)
	if err != nil {
		return "", "", err
	}
	header := http.Header{"X-Dispatch-User": {"viewer-probe"}}
	dialer := &websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, resp, dialErr := dialer.DialContext(ctx, liveURL, header)
	if dialErr == nil {
		_ = conn.Close()
		return dashboardLive, liveURL, nil
	}
	// Neither stream's failure prints as its client's own text. gorilla's and net/http's quote a
	// status line the answering party wrote that they cannot read; net/http's also quotes a Location
	// that does not parse, and names the URL it asked with the user name of its user information (a
	// foreign -url may carry one; gorilla refuses to dial it). So the live socket's failure prints by
	// its class (redact.Class: the rule prints no ws:// URL), the SSE request's as redact.Cause reads
	// it beside the URL it asked.
	var liveAnswer string
	if resp != nil {
		liveAnswer = redact.StatusLine(resp.StatusCode)
		_ = resp.Body.Close()
	} else {
		liveAnswer = redact.Class(dialErr).Error()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sseURL, nil)
	if err != nil {
		return "", "", errors.New("the Dispatch URL joined with the SSE feed's path does not parse (it is not printed)")
	}
	req.Header = header.Clone()
	req.Header.Set("Accept", "text/event-stream")
	client := &http.Client{Timeout: 30 * time.Second}
	sseResp, sseErr := client.Do(req)
	sseAnswer := "no answer"
	if sseErr != nil {
		sseAnswer = redact.Cause(sseErr, sseURL).Error()
	}
	if sseErr == nil {
		sseAnswer = redact.StatusLine(sseResp.StatusCode)
		streams := sseResp.StatusCode == http.StatusOK &&
			strings.HasPrefix(sseResp.Header.Get("Content-Type"), "text/event-stream")
		_ = sseResp.Body.Close()
		if streams {
			return dashboardSSE, sseURL, nil
		}
	}
	return "", "", fmt.Errorf("the Dispatch at %s serves no dashboard stream: /ws/live answered %s, and GET %s answered %s",
		redactURL(dispatch), liveAnswer, ssePath, sseAnswer)
}
