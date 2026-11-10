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
	"strconv"
	"strings"
	"time"
)

// storeClient talks to the metrics store over its unix socket, the only
// way to reach it from the host: the store listens on TCP inside the
// motel-metrics namespace alone, and motel-vm.socket bridges to it
// (DESIGN-metrics.md, "The shape").
type storeClient struct {
	socket string
	http   *http.Client
}

// storeRequestTimeout bounds a push or a check. A socket-activated
// proxy starts on the first connect in milliseconds; the store answers
// a small import in a few.
const storeRequestTimeout = 10 * time.Second

func newStoreClient(socket string) *storeClient {
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
	return &storeClient{socket: socket, http: &http.Client{Transport: tr, Timeout: storeRequestTimeout}}
}

// maxStoreBody bounds what is read of an answer: a query motel makes
// returns at most a few series of an hour at a minute's step.
const maxStoreBody = 4 << 20

func (c *storeClient) do(ctx context.Context, method, path string, body []byte, contentType string) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://store"+path, rd)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the metrics store at %s: %w", c.socket, unwrapURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxStoreBody+1))
	if err != nil {
		return nil, fmt.Errorf("the metrics store at %s: reading %s: %w", c.socket, path, err)
	}
	if len(data) > maxStoreBody {
		return nil, fmt.Errorf("the metrics store at %s: its answer to %s is over %d bytes", c.socket, strings.SplitN(path, "?", 2)[0], maxStoreBody)
	}
	if resp.StatusCode/100 != 2 {
		msg := strings.TrimSpace(string(data))
		if len(msg) > maxErrBody {
			msg = msg[:maxErrBody] + "…"
		}
		return nil, fmt.Errorf("the metrics store answered %d to %s: %s", resp.StatusCode, strings.SplitN(path, "?", 2)[0], msg)
	}
	return data, nil
}

// importPath is the store's Prometheus text import, every series
// labelled job="motel" (measured on Debian's 1.112.0: 204, the label
// applied).
const importPath = "/api/v1/import/prometheus?extra_label=job=motel"

func (c *storeClient) importText(ctx context.Context, text []byte) error {
	_, err := c.do(ctx, http.MethodPost, importPath, text, "text/plain")
	return err
}

// point is one sample of a query's answer.
type point struct {
	t time.Time
	v float64
}

// series is one series of a query's answer.
type series struct {
	labels map[string]string
	points []point
}

// query is an instant query at the store's present (which is
// -search.latencyOffset behind the wall clock).
func (c *storeClient) query(ctx context.Context, q string) ([]series, error) {
	data, err := c.do(ctx, http.MethodGet, "/api/v1/query?"+url.Values{"query": {q}}.Encode(), nil, "")
	if err != nil {
		return nil, err
	}
	return parseQueryAnswer(data)
}

// queryRange is a range query over [start, end] at step.
func (c *storeClient) queryRange(ctx context.Context, q string, start, end time.Time, step time.Duration) ([]series, error) {
	v := url.Values{
		"query": {q},
		"start": {strconv.FormatInt(start.Unix(), 10)},
		"end":   {strconv.FormatInt(end.Unix(), 10)},
		"step":  {strconv.Itoa(int(step / time.Second))},
	}
	data, err := c.do(ctx, http.MethodGet, "/api/v1/query_range?"+v.Encode(), nil, "")
	if err != nil {
		return nil, err
	}
	return parseQueryAnswer(data)
}

// parseQueryAnswer reads the Prometheus query API's answer, a vector or
// a matrix; anything else, or a status that is not success, is an
// error.
func parseQueryAnswer(data []byte) ([]series, error) {
	var ans struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Value  []any             `json:"value"`
				Values [][]any           `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &ans); err != nil {
		return nil, fmt.Errorf("the metrics store's answer: %w", err)
	}
	if ans.Status != "success" {
		return nil, fmt.Errorf("the metrics store's answer: status %q: %s", ans.Status, ans.Error)
	}
	if t := ans.Data.ResultType; t != "vector" && t != "matrix" {
		return nil, fmt.Errorf("the metrics store's answer: result type %q", t)
	}
	out := make([]series, 0, len(ans.Data.Result))
	for _, r := range ans.Data.Result {
		s := series{labels: r.Metric}
		pairs := r.Values
		if ans.Data.ResultType == "vector" {
			pairs = [][]any{r.Value}
		}
		for _, pr := range pairs {
			p, err := parsePoint(pr)
			if err != nil {
				return nil, err
			}
			s.points = append(s.points, p)
		}
		out = append(out, s)
	}
	return out, nil
}

var errBadPoint = errors.New("the metrics store's answer: a point is not [time, \"value\"]")

func parsePoint(pr []any) (point, error) {
	if len(pr) != 2 {
		return point{}, errBadPoint
	}
	ts, ok := pr[0].(float64)
	vs, ok2 := pr[1].(string)
	if !ok || !ok2 {
		return point{}, errBadPoint
	}
	v, err := strconv.ParseFloat(vs, 64)
	if err != nil {
		return point{}, errBadPoint
	}
	sec := int64(ts)
	return point{t: time.Unix(sec, int64((ts-float64(sec))*1e9)).UTC(), v: v}, nil
}
