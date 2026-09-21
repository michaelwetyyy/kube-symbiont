/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package sources resolves ShadowWorkload source declarations into PromQL
// query pairs and queries Prometheus for their results.
package sources

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	symbiontv1alpha1 "github.com/michaelwetyyy/kube-symbiont/api/v1alpha1"
)

// Queries is a resolved pair of instant-query expressions returning CPU cores
// and memory bytes respectively.
type Queries struct {
	CPUCores    string
	MemoryBytes string
}

// Description is the durable, human-readable resolution of a source. It is
// safe to surface in CR status and deliberately excludes generated PromQL.
type Description struct {
	Type             symbiontv1alpha1.SourceType
	CgroupPath       string
	CadvisorJob      string
	CadvisorInstance string
}

// Describe returns the concrete host source selected by a validated source
// declaration. Raw PromQL has no host-resolution metadata beyond its type.
func Describe(spec *symbiontv1alpha1.ShadowWorkloadSpec) Description {
	description := Description{Type: spec.Source.Type}
	switch spec.Source.Type {
	case symbiontv1alpha1.SourceTypeDocker:
		if spec.Source.Docker != nil {
			description.CadvisorJob = cadvisorJob(spec.Source.Docker.CadvisorJob)
			description.CadvisorInstance = spec.Source.Docker.CadvisorInstance
		}
	case symbiontv1alpha1.SourceTypeCgroup:
		if spec.Source.Cgroup != nil {
			description.CgroupPath = spec.Source.Cgroup.Path
			description.CadvisorJob = cadvisorJob(spec.Source.Cgroup.CadvisorJob)
			description.CadvisorInstance = spec.Source.Cgroup.CadvisorInstance
		}
	case symbiontv1alpha1.SourceTypeSystemd:
		if spec.Source.Systemd != nil {
			description.CgroupPath = "/system.slice/" + spec.Source.Systemd.Unit
			description.CadvisorJob = cadvisorJob(spec.Source.Systemd.CadvisorJob)
			description.CadvisorInstance = spec.Source.Systemd.CadvisorInstance
		}
	case symbiontv1alpha1.SourceTypePromQL:
	}
	return description
}

func cadvisorJob(job string) string {
	if job == "" {
		return "cadvisor"
	}
	return job
}

// dockerCgroupIDSelector isolates bare-metal Docker containers from k8s pods.
// Per the 2026-08-21 scheduling-gap study, image-label matching is refuted:
// cAdvisor monitors the whole cgroup tree and k8s pod series carry image
// labels too. The reliable discriminator on cgroup-v2/systemd hosts is the
// cgroup id prefix /system.slice/docker-<id>.
const dockerCgroupIDSelector = `id=~"/system.slice/docker-.*"`

const prometheusNaN = "NaN"

// windowPattern mirrors the CRD validation pattern for range windows;
// re-validated here so generated queries can never embed arbitrary strings.
var windowPattern = regexp.MustCompile(`^([0-9]+(\.[0-9]+)?(ms|s|m|h))+$`)
var systemdUnitPattern = regexp.MustCompile(`^[A-Za-z0-9_.:\-]+\.(service|scope)$`)
var cgroupPathPattern = regexp.MustCompile(`^/[A-Za-z0-9_.:@\-/*]+$`)

// ErrMultiSample marks a query that resolved to more than one vector sample.
// Callers classify it with errors.Is instead of parsing error text, keeping
// failure reporting bounded and label-safe.
var ErrMultiSample = errors.New("multi-sample result")

// ErrPartialSample marks a pair where exactly one resource dimension produced
// a series. Applying the present half while treating the other as zero can
// under-reserve the missing dimension, so callers must preserve last truth.
var ErrPartialSample = errors.New("partial resource measurement")

// ErrInvalidSample marks a non-finite, negative or unrepresentable CPU/memory
// measurement. Such values must never reach Kubernetes resource quantities.
var ErrInvalidSample = errors.New("invalid resource measurement")

// Resolve turns a validated ShadowWorkloadSpec into its PromQL pair.
func Resolve(spec *symbiontv1alpha1.ShadowWorkloadSpec) (Queries, error) {
	switch spec.Source.Type {
	case symbiontv1alpha1.SourceTypeDocker:
		if spec.Source.Docker == nil {
			return Queries{}, fmt.Errorf("source type %q requires source.docker", spec.Source.Type)
		}
		return resolveDocker(spec.Source.Docker, spec.Metrics.Window)
	case symbiontv1alpha1.SourceTypePromQL:
		if spec.Source.PromQL == nil {
			return Queries{}, fmt.Errorf("source type %q requires source.promql", spec.Source.Type)
		}
		return Queries{
			CPUCores:    spec.Source.PromQL.CPUCores,
			MemoryBytes: spec.Source.PromQL.MemoryBytes,
		}, nil
	case symbiontv1alpha1.SourceTypeCgroup:
		if spec.Source.Cgroup == nil {
			return Queries{}, fmt.Errorf("source type %q requires source.cgroup", spec.Source.Type)
		}
		return resolveCgroup(spec.Source.Cgroup, spec.Metrics.Window)
	case symbiontv1alpha1.SourceTypeSystemd:
		if spec.Source.Systemd == nil {
			return Queries{}, fmt.Errorf("source type %q requires source.systemd", spec.Source.Type)
		}
		return resolveSystemd(spec.Source.Systemd, spec.Metrics.Window)
	default:
		return Queries{}, fmt.Errorf("unsupported source type %q", spec.Source.Type)
	}
}

// resolveDocker generates id-prefix selectors scoped to the node's standalone
// cadvisor job (and instance when pinned). selector "all" shadows every
// bare-metal Docker container on the node; it is currently the only value the
// API admits for v0.1.
func resolveSystemd(systemd *symbiontv1alpha1.SystemdSource, window string) (Queries, error) {
	if !systemdUnitPattern.MatchString(systemd.Unit) {
		return Queries{}, fmt.Errorf("invalid systemd unit %q", systemd.Unit)
	}
	return resolveHostCgroup("/system.slice/"+systemd.Unit, false, systemd.CadvisorJob, systemd.CadvisorInstance, window)
}

func resolveCgroup(cgroup *symbiontv1alpha1.CgroupSource, window string) (Queries, error) {
	if !cgroupPathPattern.MatchString(cgroup.Path) || strings.Contains(cgroup.Path, "//") {
		return Queries{}, fmt.Errorf("invalid cgroup path %q", cgroup.Path)
	}
	return resolveHostCgroup(cgroup.Path, strings.Contains(cgroup.Path, "*"), cgroup.CadvisorJob, cgroup.CadvisorInstance, window)
}

func resolveHostCgroup(path string, glob bool, job, instance, window string) (Queries, error) {
	if !windowPattern.MatchString(window) {
		return Queries{}, fmt.Errorf("invalid metrics.window %q", window)
	}
	if instance == "" {
		return Queries{}, errors.New("host source requires cadvisorInstance for per-node accounting")
	}
	job = cadvisorJob(job)
	labelMatchers := fmt.Sprintf("job=%s,instance=%s", promLabel(job), promLabel(instance))
	var idMatcher string
	if glob {
		quoted := regexp.QuoteMeta(path)
		quoted = strings.ReplaceAll(quoted, `\*`, `[^/]*`)
		idMatcher = fmt.Sprintf(`id=~%s`, promLabel("^"+quoted+"$"))
	} else {
		idMatcher = fmt.Sprintf("id=%s", promLabel(path))
	}
	matchers := "{" + labelMatchers + "," + idMatcher + "}"
	return Queries{
		CPUCores:    fmt.Sprintf("sum(rate(container_cpu_usage_seconds_total%s[%s]))", matchers, window),
		MemoryBytes: fmt.Sprintf("sum(avg_over_time(container_memory_working_set_bytes%s[%s]))", matchers, window),
	}, nil
}

func resolveDocker(docker *symbiontv1alpha1.DockerSource, window string) (Queries, error) {
	if !windowPattern.MatchString(window) {
		return Queries{}, fmt.Errorf("invalid metrics.window %q", window)
	}

	job := docker.CadvisorJob
	job = cadvisorJob(job)
	labelMatchers := fmt.Sprintf("job=%s", promLabel(job))
	if docker.CadvisorInstance == "" {
		return Queries{}, fmt.Errorf("docker source requires cadvisorInstance for per-node accounting")
	}
	labelMatchers += fmt.Sprintf(",instance=%s", promLabel(docker.CadvisorInstance))
	matchers := "{" + labelMatchers + "," + dockerCgroupIDSelector + "}"

	return Queries{
		// Counters need rate() first, then sum across containers.
		CPUCores: fmt.Sprintf("sum(rate(container_cpu_usage_seconds_total%s[%s]))", matchers, window),
		// Working set is a gauge and what eviction watches; smooth with avg_over_time.
		MemoryBytes: fmt.Sprintf("sum(avg_over_time(container_memory_working_set_bytes%s[%s]))", matchers, window),
	}, nil
}

// promLabel renders a Prometheus label value literal, escaping backslashes
// and double quotes.
func promLabel(v string) string {
	escaped := strings.ReplaceAll(v, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return `"` + escaped + `"`
}

// Client is a minimal Prometheus HTTP-API v1 client issuing instant queries.
type Client struct {
	baseURL *url.URL
	http    *http.Client
}

// NewClient builds a client against a Prometheus base URL such as
// http://kube-prometheus-stack-prometheus.monitoring:9090.
func NewClient(rawURL string, timeout time.Duration) (*Client, error) {
	parsed, err := url.Parse(strings.TrimRight(rawURL, "/"))
	if err != nil {
		return nil, errors.New("invalid prometheusURL")
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.Opaque != "" {
		return nil, errors.New("invalid prometheusURL: absolute http(s) URL with a host required")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("invalid prometheusURL: credentials, query parameters and fragments are not allowed")
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Client{
		baseURL: parsed,
		http: &http.Client{
			Timeout: timeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return errors.New("prometheus redirects are disabled")
			},
		},
	}, nil
}

// Query issues one instant query and requires exactly one vector sample. An
// empty vector (workload off, no series) yields (0, false, nil); the caller's
// floor clamp handles it. Multiple samples are rejected because silently
// choosing one would make scheduler accounting depend on response ordering.
func (c *Client) Query(ctx context.Context, expr string) (float64, bool, error) {
	endpoint := *c.baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/api/v1/query"
	q := endpoint.Query()
	q.Set("query", expr)
	endpoint.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return 0, false, fmt.Errorf("build query request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		// Do not return the transport error: net/http errors can embed the
		// user-supplied destination and must not be copied into CR status/events.
		return 0, false, errors.New("prometheus request failed")
	}
	defer resp.Body.Close() //nolint:errcheck

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, false, fmt.Errorf("read prometheus response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return 0, false, fmt.Errorf("prometheus returned HTTP %d", resp.StatusCode)
	}

	var payload struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Value [2]any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return 0, false, fmt.Errorf("decode prometheus response: %w", err)
	}
	if payload.Status != "success" {
		// The backend's response body is untrusted and may contain query text,
		// internal identifiers or reflected secrets. Keep the durable error fixed.
		return 0, false, errors.New("prometheus query returned an error status")
	}
	if payload.Data.ResultType != "vector" {
		return 0, false, errors.New("unexpected prometheus result type; want vector")
	}
	if len(payload.Data.Result) == 0 {
		return 0, false, nil
	}
	if len(payload.Data.Result) != 1 {
		return 0, false, fmt.Errorf("%w: prometheus query returned %d samples; aggregate to one",
			ErrMultiSample, len(payload.Data.Result))
	}
	raw := payload.Data.Result[0].Value[1]
	str, ok := raw.(string)
	if !ok {
		return 0, false, fmt.Errorf("unexpected sample value type %T", raw)
	}
	value, err := parseSample(str)
	if err != nil {
		return 0, false, err
	}
	if math.IsNaN(value) {
		return 0, false, ErrInvalidSample
	}
	return value, true, nil
}

// QueryPair resolves both halves of a Queries pair in sequence. Both resource
// dimensions must have the same presence state: a fully absent pair means the
// workload is off, while a partial pair is unsafe because zeroing only one side
// could shrink that reservation below real usage.
func (c *Client) QueryPair(ctx context.Context, q Queries) (cpuCores, memoryBytes float64, found bool, err error) {
	cpu, cpuFound, err := c.Query(ctx, q.CPUCores)
	if err != nil {
		return 0, 0, false, fmt.Errorf("cpu query: %w", err)
	}
	mem, memFound, err := c.Query(ctx, q.MemoryBytes)
	if err != nil {
		return 0, 0, false, fmt.Errorf("memory query: %w", err)
	}
	if cpuFound != memFound {
		return 0, 0, false, fmt.Errorf("%w: cpu present=%t memory present=%t", ErrPartialSample, cpuFound, memFound)
	}
	if !cpuFound {
		return 0, 0, false, nil
	}
	if !validCPUResourceSample(cpu) || !validMemoryResourceSample(mem) {
		return 0, 0, false, ErrInvalidSample
	}
	return cpu, mem, true, nil
}

func validCPUResourceSample(value float64) bool {
	// float64(MaxInt64) rounds to 2^63. Step down from the CPU value whose
	// conversion to millicores could round back to that unrepresentable edge.
	maxCPU := math.Nextafter(float64(math.MaxInt64)/1000, 0)
	return validFiniteNonNegative(value) && value <= maxCPU
}

func validMemoryResourceSample(value float64) bool {
	maxInt64Float := math.Nextafter(float64(math.MaxInt64), 0)
	return validFiniteNonNegative(value) && value <= maxInt64Float
}

func validFiniteNonNegative(value float64) bool {
	return value >= 0 && !math.IsInf(value, 0) && !math.IsNaN(value)
}

func parseSample(s string) (float64, error) {
	switch s {
	case "+Inf":
		return math.Inf(1), nil
	case "-Inf":
		return math.Inf(-1), nil
	case prometheusNaN:
		return math.NaN(), nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, errors.New("prometheus sample is not numeric")
	}
	return v, nil
}
