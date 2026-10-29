package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/pprof/profile"
	"google.golang.org/protobuf/encoding/protowire"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
	"github.com/authentik-labs/akbench/internal/render"
)

// The profile breakdown answers "what is authentik's CPU busy with, per
// target": the functions burning the most CPU and how it splits between
// authentik's own code, Django, the database driver and other libraries -
// read from the continuous profiles every authentik process pushes to its
// workload cluster's Pyroscope (internal/render/pyroscope.go).
//
// Profiles aren't tagged with the k6 test or target the way traces are:
// authentik labels them with its pod's hostname, which starts with the
// deployment's Helm release name (render.HelmReleaseName). So this is the
// target's whole CPU over the window, load from this test or otherwise.
//
// It asks Pyroscope for the window's merged pprof profile rather than its
// flame graph: the flame graph is truncated to maxNodes, and at authentik's
// call-stack depth even 16k nodes leave a third of a 6h window under
// "other". The pprof is complete and, as protobuf, small enough
// (~18MB for 6h of one busy target).
const (
	pyroscopeCPUProfileType = "process_cpu:cpu:nanoseconds:cpu:nanoseconds"
	pyroscopeServiceName    = "authentik"
	// profileTopFunctions is generous so the UI's cross-target table
	// rarely finds a function missing from one target's list.
	profileTopFunctions = 60
	// maxProfileBytes bounds a merged profile read into memory.
	maxProfileBytes = 256 << 20
)

// profileComponents are authentik's processes, as named in its pods'
// names (<release>-authentik-<component>-...).
const profileComponentServer = "server"

var profileComponents = []string{profileComponentServer, "worker"}

// ProfileFunction is one function's share of the target's CPU time.
type ProfileFunction struct {
	Name string `json:"name"`
	// File is the function's source file, shortened to its path inside its
	// package (or authentik's source tree).
	File     string `json:"file"`
	Category string `json:"category"`
	// Self is the share of CPU spent in the function itself, Total the
	// share spent in it or anything it calls.
	Self  float64 `json:"self"`
	Total float64 `json:"total"`
}

// ProfileCategory is one kind of code's share of the target's CPU time,
// attributed by the function running on-CPU.
type ProfileCategory struct {
	Name  string  `json:"name"`
	Share float64 `json:"share"`
}

// ProfileTarget is one target's breakdown.
type ProfileTarget struct {
	Target string `json:"target"`
	// CPUSeconds is the CPU time profiled over the window, Cores the
	// average number of cores that is.
	CPUSeconds float64           `json:"cpuSeconds"`
	Cores      float64           `json:"cores"`
	Categories []ProfileCategory `json:"categories"`
	Functions  []ProfileFunction `json:"functions"`
	Error      string            `json:"error,omitempty"`
}

// ProfileBreakdown is the response body.
type ProfileBreakdown struct {
	TestID      string          `json:"testid"`
	Window      string          `json:"window"`
	Component   string          `json:"component"`
	GeneratedAt time.Time       `json:"generatedAt"`
	Targets     []ProfileTarget `json:"targets"`
}

// profileBreakdown answers GET /profiles/breakdown?testid=&window=15m|1h|6h&component=server|worker.
func (s *Server) profileBreakdown(w http.ResponseWriter, r *http.Request) {
	testid := r.URL.Query().Get("testid")
	window := r.URL.Query().Get("window")
	if window == "" {
		window = "1h"
	}
	component := r.URL.Query().Get("component")
	if component == "" {
		component = profileComponentServer
	}
	span, ok := breakdownWindows[window]
	if testid == "" || !ok || !slices.Contains(profileComponents, component) {
		writeError(w, r, http.StatusBadRequest, "testid, window (15m, 1h or 6h) and component (server or worker) are required")
		return
	}
	key := testid + "|" + window + "|" + component
	serveBreakdown(s, w, r, s.profiles, testid, key, func(ctx context.Context, tc *benchv1alpha1.TestCase) (ProfileBreakdown, bool) {
		return s.queryProfileBreakdown(ctx, tc, window, component, span)
	})
}

// queryProfileBreakdown reads one merged profile per target of tc over the
// trailing span. complete is false when any query failed.
func (s *Server) queryProfileBreakdown(ctx context.Context, tc *benchv1alpha1.TestCase, window, component string, span time.Duration) (resp ProfileBreakdown, complete bool) {
	end := breakdownEnd(tc)
	resp = ProfileBreakdown{TestID: tc.Name, Window: window, Component: component, GeneratedAt: end}
	targets := s.grafanaTargetsForTestID(ctx, tc)
	resp.Targets = make([]ProfileTarget, len(targets))
	var failed atomic.Bool
	var wg sync.WaitGroup
	for i, target := range targets {
		resp.Targets[i] = ProfileTarget{Target: target.Target, Error: target.Error, Categories: []ProfileCategory{}, Functions: []ProfileFunction{}}
		if target.Error != "" {
			continue
		}
		wg.Go(func() {
			prof, err := s.selectMergeProfile(ctx, target.datasourceURL(render.PyroscopeDatasourceUID), target.AdminPassword,
				profileLabelSelector(target.Deployment, component), end.Add(-span), end)
			if err != nil {
				resp.Targets[i].Error = err.Error()
				failed.Store(true)
				return
			}
			resp.Targets[i] = summarizeProfile(target.Target, prof, span)
		})
	}
	wg.Wait()
	return resp, !failed.Load()
}

// profileLabelSelector selects one deployment's component processes by
// their pods' hostnames. The deployment name is a DNS subdomain, so
// quoting it as a regexp literal is all it needs.
func profileLabelSelector(deployment, component string) string {
	return fmt.Sprintf(`{service_name=%q, hostname=~%q}`,
		pyroscopeServiceName, regexp.QuoteMeta(render.AuthentikFullname(deployment))+"-"+component+"-.+")
}

// selectMergeProfile calls Pyroscope's QuerierService.SelectMergeProfile
// over Connect's protobuf codec. The request is encoded by hand rather than
// pulling in Pyroscope's generated API for one four-field message:
// SelectMergeProfileRequest{profile_typeID=1, label_selector=2, start=3, end=4},
// start/end in Unix milliseconds. The response is a google.v1.Profile,
// which is pprof's own profile.proto.
func (s *Server) selectMergeProfile(ctx context.Context, pyroscopeURL, adminPassword, selector string, start, end time.Time) (*profile.Profile, error) {
	var body []byte
	body = protowire.AppendTag(body, 1, protowire.BytesType)
	body = protowire.AppendString(body, pyroscopeCPUProfileType)
	body = protowire.AppendTag(body, 2, protowire.BytesType)
	body = protowire.AppendString(body, selector)
	body = protowire.AppendTag(body, 3, protowire.VarintType)
	body = protowire.AppendVarint(body, uint64(start.UnixMilli()))
	body = protowire.AppendTag(body, 4, protowire.VarintType)
	body = protowire.AppendVarint(body, uint64(end.UnixMilli()))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		pyroscopeURL+"/querier.v1.QuerierService/SelectMergeProfile", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/proto")
	// Like searchTraceSpans: Grafana's datasource proxy, reached directly.
	req.SetBasicAuth(render.GrafanaAdminUser, adminPassword)
	res, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("query Pyroscope: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("query Pyroscope: %s", res.Status)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, maxProfileBytes))
	if err != nil {
		return nil, fmt.Errorf("read Pyroscope response: %w", err)
	}
	if len(data) == 0 {
		// An empty message: no profiles matched.
		return &profile.Profile{}, nil
	}
	prof, err := profile.ParseData(data)
	if err != nil {
		return nil, fmt.Errorf("decode Pyroscope response: %w", err)
	}
	return prof, nil
}

// Code categories, by source file.
const (
	categoryAuthentik = "authentik"
	categoryDjango    = "Django"
	categoryDatabase  = "Database driver"
	categoryLibraries = "Other libraries"
	categoryPython    = "Python runtime"
)

var profileCategoryOrder = []string{categoryAuthentik, categoryDjango, categoryDatabase, categoryLibraries, categoryPython}

// profileCategory classifies a Python source file: anything installed into
// site-packages is a dependency, the interpreter's own lib/pythonX.Y is the
// runtime, and the rest is authentik's source tree.
func profileCategory(file string) string {
	if _, pkg, ok := strings.Cut(file, "/site-packages/"); ok {
		switch {
		case strings.HasPrefix(pkg, "django/"):
			return categoryDjango
		case strings.HasPrefix(pkg, "psycopg"):
			return categoryDatabase
		}
		return categoryLibraries
	}
	if file == "" || strings.Contains(file, "/lib/python") {
		return categoryPython
	}
	return categoryAuthentik
}

// shortFile trims a source file to its path inside its package, or for
// the standard library and authentik's tree, inside that.
func shortFile(file string) string {
	if _, rest, ok := strings.Cut(file, "/site-packages/"); ok {
		return rest
	}
	if _, rest, ok := strings.Cut(file, "/lib/python"); ok {
		if _, after, ok := strings.Cut(rest, "/"); ok {
			return after
		}
	}
	return strings.TrimPrefix(file, "/")
}

func summarizeProfile(target string, prof *profile.Profile, span time.Duration) ProfileTarget {
	out := ProfileTarget{Target: target, Categories: []ProfileCategory{}, Functions: []ProfileFunction{}}
	valueIndex := 0
	for i, st := range prof.SampleType {
		if st.Type == "cpu" {
			valueIndex = i
		}
	}
	type key struct{ name, file string }
	self := map[key]int64{}
	total := map[key]int64{}
	categories := map[string]int64{}
	var all int64
	seen := map[key]bool{}
	for _, sample := range prof.Sample {
		if valueIndex >= len(sample.Value) {
			continue
		}
		v := sample.Value[valueIndex]
		if v <= 0 {
			continue
		}
		all += v
		clear(seen)
		leaf := true
		// Location[0] is the leaf; within a location, Line[0] is the
		// innermost inlined frame.
		for _, loc := range sample.Location {
			for _, line := range loc.Line {
				if line.Function == nil {
					continue
				}
				k := key{line.Function.Name, line.Function.Filename}
				if leaf {
					self[k] += v
					categories[profileCategory(k.file)] += v
					leaf = false
				}
				// Count a recursive function's total once per sample.
				if !seen[k] {
					seen[k] = true
					total[k] += v
				}
			}
		}
	}
	if all == 0 {
		return out
	}
	// The profile type is CPU nanoseconds.
	out.CPUSeconds = float64(all) / 1e9
	out.Cores = out.CPUSeconds / span.Seconds()
	n := float64(all)
	for _, name := range profileCategoryOrder {
		if categories[name] > 0 {
			out.Categories = append(out.Categories, ProfileCategory{Name: name, Share: float64(categories[name]) / n})
		}
	}
	for k, v := range self {
		out.Functions = append(out.Functions, ProfileFunction{
			Name: k.name, File: shortFile(k.file), Category: profileCategory(k.file),
			Self: float64(v) / n, Total: float64(total[k]) / n,
		})
	}
	slices.SortFunc(out.Functions, func(a, b ProfileFunction) int {
		switch {
		case a.Self > b.Self:
			return -1
		case a.Self < b.Self:
			return 1
		}
		return strings.Compare(a.Name+a.File, b.Name+b.File)
	})
	if len(out.Functions) > profileTopFunctions {
		out.Functions = out.Functions[:profileTopFunctions]
	}
	return out
}
