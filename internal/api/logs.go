package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
	"github.com/authentik-labs/akbench/internal/render"
)

// The log breakdown answers "what is authentik complaining about, per
// target": how many lines each process logs at each level, and the most
// frequent warning-or-worse events - read from the container logs Alloy
// ships to each workload cluster's Loki (internal/render/loki.go).
//
// Like profiles, logs aren't tagged with the k6 test or target: they're
// selected by namespace and pod name (<release>-authentik-<component>-...),
// so this is the target's whole log volume over the window, load from this
// test or otherwise.
//
// authentik logs structlog/logrus JSON with "level", "logger" and "event"
// keys. Both queries are LogQL metric queries evaluated once at the end of
// the window, so Loki counts server-side and only the totals come back.
// Lines that aren't JSON (startup banners, tracebacks' continuation lines)
// fail | json with an __error__ label, which Loki refuses to return from a
// metric query; the levels query drops it so they count under no level.
const (
	logTopEvents     = 15
	maxLogEventChars = 300
	// maxLokiErrorBytes bounds the error body quoted from a failed query.
	maxLokiErrorBytes = 1024
)

// logProblemLevels are the levels the top events are drawn from:
// structlog's warning/error/critical and logrus' fatal/panic.
const logProblemLevels = "warning|error|critical|fatal|panic"

// LogEvent is one distinct warning-or-worse log event.
type LogEvent struct {
	Level  string `json:"level"`
	Logger string `json:"logger"`
	Event  string `json:"event"`
	Count  int64  `json:"count"`
}

// LogComponent is one authentik process's log volume, by level. Lines that
// aren't JSON (or have no level) count under the empty level.
type LogComponent struct {
	Component string           `json:"component"`
	Lines     int64            `json:"lines"`
	Levels    map[string]int64 `json:"levels"`
}

// LogTarget is one target's breakdown.
type LogTarget struct {
	Target         string         `json:"target"`
	Lines          int64          `json:"lines"`
	LinesPerSecond float64        `json:"linesPerSecond"`
	Components     []LogComponent `json:"components"`
	TopEvents      []LogEvent     `json:"topEvents"`
	Error          string         `json:"error,omitempty"`
}

// LogBreakdown is the response body.
type LogBreakdown struct {
	TestID      string      `json:"testid"`
	Window      string      `json:"window"`
	GeneratedAt time.Time   `json:"generatedAt"`
	Targets     []LogTarget `json:"targets"`
}

// logBreakdown answers GET /logs/breakdown?testid=&window=15m|1h|6h.
func (s *Server) logBreakdown(w http.ResponseWriter, r *http.Request) {
	testid := r.URL.Query().Get("testid")
	window := r.URL.Query().Get("window")
	if window == "" {
		window = "1h"
	}
	span, ok := breakdownWindows[window]
	if testid == "" || !ok {
		writeError(w, r, http.StatusBadRequest, "testid and window (15m, 1h or 6h) are required")
		return
	}
	serveBreakdown(s, w, r, s.logs, testid, testid+"|"+window, func(ctx context.Context, tc *benchv1alpha1.TestCase) (LogBreakdown, bool) {
		return s.queryLogBreakdown(ctx, tc, window, span)
	})
}

// queryLogBreakdown runs the level and top-event queries per target of tc
// over the trailing span. complete is false when any query failed.
func (s *Server) queryLogBreakdown(ctx context.Context, tc *benchv1alpha1.TestCase, window string, span time.Duration) (resp LogBreakdown, complete bool) {
	end := time.Now()
	resp = LogBreakdown{TestID: tc.Name, Window: window, GeneratedAt: end}
	targets := s.grafanaTargetsForTestID(ctx, tc)
	resp.Targets = make([]LogTarget, len(targets))
	var failed atomic.Bool
	var wg sync.WaitGroup
	for i, target := range targets {
		resp.Targets[i] = LogTarget{Target: target.Target, Error: target.Error, Components: []LogComponent{}, TopEvents: []LogEvent{}}
		if target.Error != "" {
			continue
		}
		wg.Go(func() {
			lokiURL := target.datasourceURL(render.LokiDatasourceUID)
			selector := logStreamSelector(target.Namespace, target.Deployment)
			levels, err := s.lokiInstantQuery(ctx, lokiURL, target.AdminPassword, logLevelsQuery(selector, span), end)
			var events []lokiSample
			if err == nil {
				events, err = s.lokiInstantQuery(ctx, lokiURL, target.AdminPassword, logTopEventsQuery(selector, span), end)
			}
			if err != nil {
				resp.Targets[i].Error = err.Error()
				failed.Store(true)
				return
			}
			resp.Targets[i] = summarizeLogs(target.Target, target.Deployment, levels, events, span)
		})
	}
	wg.Wait()
	return resp, !failed.Load()
}

// logStreamSelector selects one deployment's authentik containers (not
// e.g. its postgres) by namespace and pod name. Both are DNS names, so
// quoting the deployment as a regexp literal is all it needs.
func logStreamSelector(namespace, deployment string) string {
	return fmt.Sprintf(`{%s=%q, %s=~%q}`,
		render.LokiLabelNamespace, namespace,
		render.LokiLabelPod, regexp.QuoteMeta(deployment)+"-authentik-("+strings.Join(profileComponents, "|")+")-.+")
}

// logRange is span as a LogQL range.
func logRange(span time.Duration) string {
	return strconv.Itoa(int(span.Seconds())) + "s"
}

func logLevelsQuery(selector string, span time.Duration) string {
	return fmt.Sprintf(`sum by (%s, level) (count_over_time(%s | json level="level" | drop __error__, __error_details__ [%s]))`,
		render.LokiLabelPod, selector, logRange(span))
}

func logTopEventsQuery(selector string, span time.Duration) string {
	return fmt.Sprintf(`topk(%d, sum by (level, logger, event) (count_over_time(%s | json level="level", logger="logger", event="event" | level=~%q [%s])))`,
		logTopEvents, selector, logProblemLevels, logRange(span))
}

// lokiSample is one series of a Loki instant query's vector result.
type lokiSample struct {
	Metric map[string]string
	Value  float64
}

// lokiQueryResponse is the subset of Loki's /loki/api/v1/query response
// used here: a vector of [<unix seconds>, "<value>"] samples.
type lokiQueryResponse struct {
	Data struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  [2]any            `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

func (s *Server) lokiInstantQuery(ctx context.Context, lokiURL, adminPassword, query string, at time.Time) ([]lokiSample, error) {
	q := url.Values{}
	q.Set("query", query)
	q.Set("time", strconv.FormatInt(at.UnixNano(), 10))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, lokiURL+"/loki/api/v1/query?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	// Like searchTraceSpans: Grafana's datasource proxy, reached directly.
	req.SetBasicAuth(render.GrafanaAdminUser, adminPassword)
	res, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("query Loki: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		// Loki explains a rejected query in a plain-text body.
		msg, _ := io.ReadAll(io.LimitReader(res.Body, maxLokiErrorBytes))
		if text := strings.TrimSpace(string(msg)); text != "" {
			return nil, fmt.Errorf("query Loki: %s: %s", res.Status, text)
		}
		return nil, fmt.Errorf("query Loki: %s", res.Status)
	}
	var body lokiQueryResponse
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode Loki response: %w", err)
	}
	if body.Data.ResultType != "vector" {
		return nil, fmt.Errorf("decode Loki response: result type %q, want vector", body.Data.ResultType)
	}
	samples := make([]lokiSample, 0, len(body.Data.Result))
	for _, r := range body.Data.Result {
		raw, _ := r.Value[1].(string)
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, fmt.Errorf("decode Loki response: value %q: %w", raw, err)
		}
		samples = append(samples, lokiSample{Metric: r.Metric, Value: v})
	}
	return samples, nil
}

// podComponent is the authentik component a pod of deployment belongs to,
// from its name (<deployment>-authentik-<component>-...).
func podComponent(deployment, pod string) string {
	rest, ok := strings.CutPrefix(pod, deployment+"-authentik-")
	if !ok {
		return ""
	}
	component, _, _ := strings.Cut(rest, "-")
	return component
}

func summarizeLogs(target, deployment string, levels, events []lokiSample, span time.Duration) LogTarget {
	out := LogTarget{Target: target, Components: []LogComponent{}, TopEvents: []LogEvent{}}
	components := map[string]*LogComponent{}
	for _, sample := range levels {
		name := podComponent(deployment, sample.Metric[render.LokiLabelPod])
		if name == "" {
			continue
		}
		c := components[name]
		if c == nil {
			c = &LogComponent{Component: name, Levels: map[string]int64{}}
			components[name] = c
		}
		n := int64(sample.Value)
		c.Lines += n
		c.Levels[sample.Metric["level"]] += n
		out.Lines += n
	}
	for _, name := range profileComponents {
		if c := components[name]; c != nil {
			out.Components = append(out.Components, *c)
		}
	}
	out.LinesPerSecond = float64(out.Lines) / span.Seconds()
	for _, sample := range events {
		event := sample.Metric["event"]
		if len(event) > maxLogEventChars {
			event = event[:maxLogEventChars] + "…"
		}
		out.TopEvents = append(out.TopEvents, LogEvent{
			Level: sample.Metric["level"], Logger: sample.Metric["logger"], Event: event, Count: int64(sample.Value),
		})
	}
	slices.SortFunc(out.TopEvents, func(a, b LogEvent) int {
		switch {
		case a.Count > b.Count:
			return -1
		case a.Count < b.Count:
			return 1
		}
		return strings.Compare(a.Event, b.Event)
	})
	return out
}
