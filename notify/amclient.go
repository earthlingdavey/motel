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
const amRequestTimeout = 10 * time.Second

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

// maxAMBody bounds what is read of an answer. The largest is every
// alert Alertmanager holds, for the people's reads (heldAlerts): 4 MiB
// is 5,500 to 7,500 alerts with no details, as motel's own events are,
// and about 1,300 to 1,400 from `moteld monitor send` with its 2 KiB of
// details (docs/monitor.md, "More alerts than motel reads").
const maxAMBody = 4 << 20

// do asks Alertmanager. A 2xx answer over maxAMBody is an
// amOverLimitError, never handed on cut.
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
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAMBody+1))
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
	if len(data) > maxAMBody {
		return nil, &amOverLimitError{socket: c.socket, path: strings.SplitN(path, "?", 2)[0]}
	}
	return data, nil
}

// amOverLimitError is a 2xx answer longer than maxAMBody: Alertmanager
// answered, with more than motel reads.
type amOverLimitError struct {
	socket, path string
}

func (e *amOverLimitError) Error() string {
	return fmt.Sprintf("alertmanager at %s: its answer to %s is over %d bytes, the most motel reads", e.socket, e.path, maxAMBody)
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
// failureCopies is every ChannelFailed Alertmanager holds whose
// suspects are ch alone, each by its labels: the copies addressed to
// channels whoever they were (a channel a load has since taken out or
// set to criticals alone too) and the read-only one, whatever its
// integration.
func (c *amClient) failureCopies(ctx context.Context, ch string) ([]map[string]string, error) {
	q := url.Values{"filter": {fmt.Sprintf("alertname=%q", alertChannelFailed), fmt.Sprintf("suspects=%q", ch)}}
	data, err := c.do(ctx, http.MethodGet, "/api/v2/alerts?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	var held []struct {
		Labels map[string]string `json:"labels"`
	}
	if err := json.Unmarshal(data, &held); err != nil {
		return nil, fmt.Errorf("alertmanager at %s: GET /api/v2/alerts: %w", c.socket, err)
	}
	out := make([]map[string]string, 0, len(held))
	for _, h := range held {
		if h.Labels["alertname"] == alertChannelFailed && h.Labels["suspects"] == ch {
			out = append(out, h.Labels)
		}
	}
	return out, nil
}

func (c *amClient) metrics(ctx context.Context) ([]byte, error) {
	return c.do(ctx, http.MethodGet, "/metrics", nil)
}
