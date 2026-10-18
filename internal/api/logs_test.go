package api

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/authentik-labs/akbench/internal/render"
)

// Loki's instant-query answers for the two log breakdown queries: lines per
// pod and level (two server pods, a worker, and a pod of another
// deployment the regexp must not have matched), and the top events.
const (
	fakeLokiLevels = `{"status":"success","data":{"resultType":"vector","result":[
 {"metric":{"pod":"ak-a-authentik-server-5d8f-abc","level":"info"},"value":[1700000000,"900"]},
 {"metric":{"pod":"ak-a-authentik-server-5d8f-def","level":"info"},"value":[1700000000,"600"]},
 {"metric":{"pod":"ak-a-authentik-server-5d8f-def","level":"warning"},"value":[1700000000,"30"]},
 {"metric":{"pod":"ak-a-authentik-worker-77c9-xyz","level":"error"},"value":[1700000000,"12"]},
 {"metric":{"pod":"ak-a-authentik-worker-77c9-xyz","level":""},"value":[1700000000,"258"]},
 {"metric":{"pod":"unrelated","level":"info"},"value":[1700000000,"5"]}
]}}`
	fakeLokiEvents = `{"status":"success","data":{"resultType":"vector","result":[
 {"metric":{"level":"error","logger":"authentik.tasks","event":"Task failed"},"value":[1700000000,"12"]},
 {"metric":{"level":"warning","logger":"authentik.flows","event":"Flow not applicable"},"value":[1700000000,"30"]}
]}}`
)

func TestLogBreakdown(t *testing.T) {
	var queries atomic.Int32
	var levelsQuery, eventsQuery, at string
	s := traceFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/grafana-proxy/bench/api/datasources/proxy/uid/loki/loki/api/v1/query" {
			http.NotFound(w, r)
			return
		}
		if user, pass, ok := r.BasicAuth(); !ok || user != render.GrafanaAdminUser || pass != grafanaTestAdminPassword {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		queries.Add(1)
		q := r.URL.Query().Get("query")
		at = r.URL.Query().Get("time")
		if strings.HasPrefix(q, "topk(") {
			eventsQuery = q
			_, _ = fmt.Fprint(w, fakeLokiEvents)
			return
		}
		levelsQuery = q
		_, _ = fmt.Fprint(w, fakeLokiLevels)
	}))

	rec := do(t, s, http.MethodGet, "/api/v1/logs/breakdown?testid=login&window=15m", nil)
	requireStatus(t, rec, http.StatusOK)
	got := decodeBody[LogBreakdown](t, rec)
	if len(got.Targets) != 1 {
		t.Fatalf("breakdown: %+v", got)
	}
	selector := `{namespace="ak-a", pod=~"ak-a-authentik-(server|worker)-.+"}`
	if !strings.Contains(levelsQuery, selector) || !strings.Contains(levelsQuery, "[900s]") || !strings.Contains(levelsQuery, "sum by (pod, level)") {
		t.Errorf("levels query = %s", levelsQuery)
	}
	if !strings.Contains(eventsQuery, selector) || !strings.Contains(eventsQuery, `level=~"warning|error|critical|fatal|panic"`) {
		t.Errorf("events query = %s", eventsQuery)
	}
	if ns, err := strconv.ParseInt(at, 10, 64); err != nil || time.Since(time.Unix(0, ns)) > time.Minute {
		t.Errorf("query time = %q, want now in Unix nanoseconds", at)
	}

	a := got.Targets[0]
	if a.Error != "" {
		t.Fatalf("target error: %s", a.Error)
	}
	if a.Lines != 1800 {
		t.Errorf("lines = %d, want 1800 (the unrelated pod excluded)", a.Lines)
	}
	approx(t, "linesPerSecond", a.LinesPerSecond, 2)
	if len(a.Components) != 2 || a.Components[0].Component != "server" || a.Components[1].Component != "worker" {
		t.Fatalf("components: %+v", a.Components)
	}
	server, worker := a.Components[0], a.Components[1]
	if server.Lines != 1530 || server.Levels["info"] != 1500 || server.Levels["warning"] != 30 {
		t.Errorf("server = %+v", server)
	}
	if worker.Lines != 270 || worker.Levels["error"] != 12 || worker.Levels[""] != 258 {
		t.Errorf("worker = %+v", worker)
	}
	if len(a.TopEvents) != 2 || a.TopEvents[0].Event != "Flow not applicable" || a.TopEvents[0].Count != 30 || a.TopEvents[1].Logger != "authentik.tasks" {
		t.Errorf("top events (most frequent first): %+v", a.TopEvents)
	}

	requireStatus(t, do(t, s, http.MethodGet, "/api/v1/logs/breakdown?testid=login&window=15m", nil), http.StatusOK)
	if n := queries.Load(); n != 2 {
		t.Errorf("Loki queried %d times, want 2 (cached)", n)
	}
}

func TestLogBreakdownDoesNotCacheFailures(t *testing.T) {
	var queries atomic.Int32
	s := traceFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if queries.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	}))
	got := decodeBody[LogBreakdown](t, do(t, s, http.MethodGet, "/api/v1/logs/breakdown?testid=login&window=1h", nil))
	if got.Targets[0].Error == "" {
		t.Fatal("expected the failed query's error")
	}
	got = decodeBody[LogBreakdown](t, do(t, s, http.MethodGet, "/api/v1/logs/breakdown?testid=login&window=1h", nil))
	if a := got.Targets[0]; a.Error != "" || a.Lines != 0 || len(a.TopEvents) != 0 {
		t.Errorf("retry should be an empty, error-free target: %+v", a)
	}
}

func TestLogBreakdownValidates(t *testing.T) {
	s, _ := newServer(t, Config{})
	requireStatus(t, do(t, s, http.MethodGet, "/api/v1/logs/breakdown?testid=x&window=7d", nil), http.StatusBadRequest)
	requireStatus(t, do(t, s, http.MethodGet, "/api/v1/logs/breakdown", nil), http.StatusBadRequest)
}

func TestPodComponent(t *testing.T) {
	for pod, want := range map[string]string{
		"ak-a-authentik-server-5d8f-abc": "server",
		"ak-a-authentik-worker-77c9-xyz": "worker",
		"ak-ab-authentik-server-1-2":     "",
		"ak-a-postgres-1":                "",
	} {
		if got := podComponent("ak-a", pod); got != want {
			t.Errorf("podComponent(%q) = %q, want %q", pod, got, want)
		}
	}
}
