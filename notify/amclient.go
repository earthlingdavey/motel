package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// amClient talks to Alertmanager over its unix socket, the only way to
// reach it: the socket unit opens no TCP port, and its mode admits root
// and the motel uid (DESIGN-notify.md, "Who can reach the socket").
type amClient struct {
	socket string
	http   *http.Client
}

// amRequestTimeout bounds one request. Socket activation starts
// Alertmanager on the first connect, about a second on hotserve-1.
// Divided with the clocks it sits between (clock.go), down to the
// requestFloor that start needs.
const amRequestTimeout = max(10*time.Second/clockDiv, requestFloor)

func newAMClient(socket string) *amClient {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
		MaxIdleConns:    2,
		IdleConnTimeout: 30 * time.Second,
		// No proxy: the environment's HTTP_PROXY must never see this.
		Proxy: nil,
	}
	return &amClient{socket: socket, http: &http.Client{Transport: tr, Timeout: amRequestTimeout}}
}

// amAlert is one alert in Alertmanager's v2 API (POST /api/v2/alerts).
type amAlert struct {
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations,omitempty"`
	StartsAt    time.Time         `json:"startsAt,omitzero"`
	EndsAt      time.Time         `json:"endsAt,omitzero"`
}

// maxErrBody is how much of an error response is kept for a message.
const maxErrBody = 512

func (c *amClient) do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://alertmanager"+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("alertmanager at %s: %w", c.socket, unwrapURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("alertmanager at %s: reading %s: %w", c.socket, path, err)
	}
	if resp.StatusCode/100 != 2 {
		msg := strings.TrimSpace(string(data))
		if len(msg) > maxErrBody {
			msg = msg[:maxErrBody] + "…"
		}
		return nil, &amStatusError{path: path, code: resp.StatusCode, msg: msg}
	}
	return data, nil
}

// amStatusError is an answer that is not 2xx: Alertmanager refused the
// request (a bad alert, a config it cannot load). A retry does not
// change it.
type amStatusError struct {
	path string
	code int
	msg  string
}

func (e *amStatusError) Error() string {
	return fmt.Sprintf("alertmanager answered %d to %s: %s", e.code, e.path, e.msg)
}

// unwrapURLError drops net/http's `Post "http://alertmanager/…":`
// prefix, which names a host that does not exist.
func unwrapURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

func (c *amClient) reload(ctx context.Context) error {
	_, err := c.do(ctx, http.MethodPost, "/-/reload", nil)
	return err
}

func (c *amClient) postAlerts(ctx context.Context, alerts []amAlert) error {
	body, err := json.Marshal(alerts)
	if err != nil {
		return err
	}
	_, err = c.do(ctx, http.MethodPost, "/api/v2/alerts", body)
	return err
}

// metrics is Alertmanager's own /metrics, which channel_failed reads.
func (c *amClient) metrics(ctx context.Context) ([]byte, error) {
	return c.do(ctx, http.MethodGet, "/metrics", nil)
}
