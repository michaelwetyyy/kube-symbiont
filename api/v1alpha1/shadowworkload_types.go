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

package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// SourceType selects which bare-metal source a ShadowWorkload measures.
//
// +kubebuilder:validation:Enum=docker;promql;cgroup;systemd
type SourceType string

const (
	// SourceTypeDocker shadows Docker containers discovered via cAdvisor
	// cgroup id-prefix selectors (/system.slice/docker-<id>).
	SourceTypeDocker SourceType = "docker"

	// SourceTypePromQL passes raw PromQL through to the configured
	// Prometheus. Escape hatch for anything the typed sources cannot
	// express.
	SourceTypePromQL SourceType = "promql"

	// SourceTypeCgroup shadows one cgroup path or a bounded cgroup path glob.
	SourceTypeCgroup SourceType = "cgroup"

	// SourceTypeSystemd shadows one systemd unit through its cgroup path.
	SourceTypeSystemd SourceType = "systemd"
)

const (
	// SelectorAll is currently the only implemented docker selector value:
	// shadow every bare-metal Docker container on the target node.
	SelectorAll = "all"

	// DefaultDeltaThresholdPercent is applied when spec.update.deltaThresholdPercent
	// is omitted.
	DefaultDeltaThresholdPercent = 10

	// DefaultPollInterval is applied when spec.update.pollInterval is omitted.
	DefaultPollInterval = "30s"
)

// ShadowWorkloadSpec defines the desired state of a ShadowWorkload: which node
// to account for, what bare-metal workload to measure there, where metrics
// come from, and how aggressively the phantom may resize.
type ShadowWorkloadSpec struct {
	// node is the Kubernetes Node hosting the bare-metal workload. The phantom
	// pod is pinned to it via pod.spec.nodeName, so its requests are subtracted
	// from that node's allocatable in the scheduler ledger.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Node string `json:"node"`

	// source declares what to measure on that node. Exactly one source block
	// must be set and it must match type.
	// +kubebuilder:validation:Required
	Source SourceSpec `json:"source"`

	// metrics configures the Prometheus backend queried for measurements.
	// +kubebuilder:validation:Required
	Metrics MetricsConfig `json:"metrics"`

	// update controls when observed drift turns into a phantom resize.
	// +kubebuilder:validation:Required
	Update UpdatePolicy `json:"update"`
}

// SourceSpec is a one-of container selecting the measured bare-metal source.
// +kubebuilder:validation:XValidation:rule="[has(self.docker), has(self.promql), has(self.cgroup), has(self.systemd)].filter(x, x).size() == 1",message="exactly one source block must be set"
// +kubebuilder:validation:XValidation:rule="(self.type == 'docker') == has(self.docker) && (self.type == 'promql') == has(self.promql) && (self.type == 'cgroup') == has(self.cgroup) && (self.type == 'systemd') == has(self.systemd)",message="source.type must match the configured source block"
type SourceSpec struct {
	// type selects the source implementation to resolve.
	Type SourceType `json:"type"`

	// docker shadows Docker containers running directly on the node
	// (outside the kubepods cgroup), as seen by the standalone cAdvisor job.
	// Required when type is docker.
	// +optional
	Docker *DockerSource `json:"docker,omitempty"`

	// promql supplies raw PromQL for CPU cores and memory bytes. Required when
	// type is promql. Both queries must return a single vector sample after
	// aggregation (use sum()).
	// +optional
	PromQL *PromQLSource `json:"promql,omitempty"`

	// cgroup shadows a cgroup path (or bounded glob) from standalone cAdvisor.
	// +optional
	Cgroup *CgroupSource `json:"cgroup,omitempty"`

	// systemd shadows one systemd unit from standalone cAdvisor.
	// +optional
	Systemd *SystemdSource `json:"systemd,omitempty"`
}

// DockerSource resolves to PromQL matching all bare-metal Docker containers on
// the target node. Per the 2026-08-21 scheduling-gap study, discrimination from
// k8s pods uses the cgroup id prefix (/system.slice/docker-*) — image-label
// matching matches k8s pods too and was refuted.
type DockerSource struct {
	// selector picks which containers to shadow. v0.1 implements only "all":
	// every Docker container on the node's system.slice.
	// +kubebuilder:validation:Enum=all
	Selector string `json:"selector"`

	// cadvisorJob is the Prometheus job label emitted by the standalone
	// cAdvisor instance monitoring this node.
	// +kubebuilder:default="cadvisor"
	CadvisorJob string `json:"cadvisorJob,omitempty"`

	// cadvisorInstance pins the Prometheus instance label (e.g.
	// "192.0.2.10:4194") to this node's standalone cAdvisor target. It is
	// required so a multi-target job can never aggregate other nodes into this
	// node's scheduler reservation.
	// +kubebuilder:validation:MinLength=1
	CadvisorInstance string `json:"cadvisorInstance"`
}

// CgroupSource resolves one cgroup path or bounded cgroup path glob through a
// standalone cAdvisor scrape target. Glob syntax supports only `*` within path
// segments; the generated regex is anchored to the full cgroup id.
type CgroupSource struct {
	// path is an absolute cgroup-v2 path, for example /system.slice/example.service
	// or /system.slice/my-worker-*.scope.
	// +kubebuilder:validation:Pattern=`^/[A-Za-z0-9_.:@\-/*]+$`
	// +kubebuilder:validation:MinLength=2
	Path string `json:"path"`

	// cadvisorJob is the Prometheus job label emitted by standalone cAdvisor.
	// +kubebuilder:default="cadvisor"
	CadvisorJob string `json:"cadvisorJob,omitempty"`

	// cadvisorInstance pins this source to one host scrape target.
	// +kubebuilder:validation:MinLength=1
	CadvisorInstance string `json:"cadvisorInstance"`
}

// SystemdSource resolves a simple system-manager service or scope unit in the
// default system.slice to /system.slice/<unit> on a cgroup-v2/systemd host.
// Units placed in custom slices, instantiated @ units, and slice units should
// use the explicit cgroup source because their cgroup path is not this simple.
type SystemdSource struct {
	// unit is a simple system-manager service or scope unit such as
	// minecraft.service. Path separators, template/instance markers and slice
	// units are rejected; use source.cgroup for those layouts.
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_.:\-]+\.(service|scope)$`
	Unit string `json:"unit"`

	// cadvisorJob is the Prometheus job label emitted by standalone cAdvisor.
	// +kubebuilder:default="cadvisor"
	CadvisorJob string `json:"cadvisorJob,omitempty"`

	// cadvisorInstance pins this source to one host scrape target.
	// +kubebuilder:validation:MinLength=1
	CadvisorInstance string `json:"cadvisorInstance"`
}

// PromQLSource passes raw CPU/memory queries straight through to Prometheus.
type PromQLSource struct {
	// cpuCores is an instant-query expression returning total CPU cores used by
	// the workload. Rate the counter first, e.g.
	// sum(rate(container_cpu_usage_seconds_total{...}[5m])).
	// +kubebuilder:validation:MinLength=1
	CPUCores string `json:"cpuCores"`

	// memoryBytes is an instant-query expression returning resident memory in
	// bytes, e.g.
	// sum(avg_over_time(container_memory_working_set_bytes{...}[5m])).
	// Working set is preferred: it is what eviction watches.
	// +kubebuilder:validation:MinLength=1
	MemoryBytes string `json:"memoryBytes"`
}

// MetricsConfig points the controller at the Prometheus server backing all
// source types.
type MetricsConfig struct {
	// prometheusURL is the base URL of the Prometheus HTTP API, e.g.
	// http://kube-prometheus-stack-prometheus.monitoring:9090.
	// +kubebuilder:validation:Pattern=`^https?://.+$`
	PrometheusURL string `json:"prometheusURL"`

	// window is the range window the controller injects into generated
	// queries (docker source) — the smoothing horizon of the moving average.
	// promql passthrough embeds its own ranges; window still documents the
	// intended smoothing horizon.
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(ms|s|m|h))+$`
	Window string `json:"window"`
}

// ResourcePair is a CPU-cores + memory-bytes pair expressed as standard
// quantities (e.g. 10m / 32Mi).
type ResourcePair struct {
	// cpu is a Kubernetes CPU quantity ("500m", "2").
	// +kubebuilder:default="10m"
	CPU resource.Quantity `json:"cpu"`

	// memory is a Kubernetes memory quantity ("32Mi", "8Gi").
	// +kubebuilder:default="32Mi"
	Memory resource.Quantity `json:"memory"`
}

// UpdatePolicy governs the measure → clamp → resize loop cadence and hysteresis.
// +kubebuilder:validation:XValidation:rule="!quantity(self.floor.cpu).isLessThan(quantity('0'))",message="update.floor.cpu must be non-negative"
// +kubebuilder:validation:XValidation:rule="!quantity(self.floor.memory).isLessThan(quantity('0'))",message="update.floor.memory must be non-negative"
// +kubebuilder:validation:XValidation:rule="!quantity(self.ceiling.cpu).isLessThan(quantity('0'))",message="update.ceiling.cpu must be non-negative"
// +kubebuilder:validation:XValidation:rule="!quantity(self.ceiling.memory).isLessThan(quantity('0'))",message="update.ceiling.memory must be non-negative"
// +kubebuilder:validation:XValidation:rule="!quantity(self.ceiling.cpu).isLessThan(quantity(self.floor.cpu))",message="update.floor.cpu must not exceed update.ceiling.cpu"
// +kubebuilder:validation:XValidation:rule="!quantity(self.ceiling.memory).isLessThan(quantity(self.floor.memory))",message="update.floor.memory must not exceed update.ceiling.memory"
type UpdatePolicy struct {
	// deltaThresholdPercent is the minimum relative change (per dimension,
	// CPU or memory) between the phantom's current requests and the clamped
	// measurement before a resize is issued. Lower values track more tightly;
	// higher values dampen flapping.
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	DeltaThresholdPercent int32 `json:"deltaThresholdPercent"`

	// pollInterval is how often the controller re-measures and re-evaluates
	// the phantom. Metric-driven reconciliation runs on this clock regardless
	// of CR changes.
	// +kubebuilder:default="30s"
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(ms|s|m|h))+$`
	PollInterval string `json:"pollInterval"`

	// floor clamps the phantom downward: when the workload is off or emits no
	// metrics, requests settle here instead of zero, keeping a small standing
	// reservation.
	Floor ResourcePair `json:"floor"`

	// ceiling safety-clamps the phantom upward so a runaway measurement can
	// never request more than this.
	Ceiling ResourcePair `json:"ceiling"`
}

// MeasurementStatus records the last successful raw source measurement.
// It is intentionally preserved across backend outages so status remains a
// durable last-known-good observation instead of collapsing to zero.
type MeasurementStatus struct {
	// time is when the measurement completed successfully.
	Time metav1.Time `json:"time"`

	// cpu is the raw measured CPU footprint before floor/ceiling clamping.
	CPU resource.Quantity `json:"cpu"`

	// memory is the raw measured memory footprint before floor/ceiling clamping.
	Memory resource.Quantity `json:"memory"`

	// seriesFound distinguishes a real zero-valued sample from an empty query
	// result (for example, a stopped service with no current cAdvisor series).
	SeriesFound bool `json:"seriesFound"`
}

// ResolvedSourceStatus exposes the concrete source that produced the durable
// lastMeasurement snapshot without leaking generated PromQL into status.
type ResolvedSourceStatus struct {
	Type SourceType `json:"type"`

	// cgroupPath is populated for typed cgroup/systemd sources.
	// +optional
	CgroupPath string `json:"cgroupPath,omitempty"`

	// cadvisorJob is populated for host sources backed by standalone cAdvisor.
	// +optional
	CadvisorJob string `json:"cadvisorJob,omitempty"`

	// cadvisorInstance is the exact host scrape target used for accounting.
	// +optional
	CadvisorInstance string `json:"cadvisorInstance,omitempty"`
}

// ShadowWorkloadStatus defines the observed state of ShadowWorkload.
type ShadowWorkloadStatus struct {
	// phantomPod is the name of the ballast pod maintained on spec.node, if
	// any has been created yet.
	// +optional
	PhantomPod string `json:"phantomPod,omitempty"`

	// currentCPU mirrors the phantom's current CPU request.
	// +optional
	CurrentCPU resource.Quantity `json:"currentCPU,omitempty"`

	// currentMemory mirrors the phantom's current memory request.
	// +optional
	CurrentMemory resource.Quantity `json:"currentMemory,omitempty"`

	// lastResize is the time of the most recent accepted in-place resize.
	// +optional
	LastResize metav1.Time `json:"lastResize,omitempty"`

	// lastMeasurement is the most recent successful raw source measurement.
	// It is retained during subsequent source/backend failures.
	// +optional
	LastMeasurement *MeasurementStatus `json:"lastMeasurement,omitempty"`

	// resolvedSource describes the concrete source that produced
	// lastMeasurement. It advances atomically with lastMeasurement and is
	// retained with that last-known-good snapshot during later failures.
	// +optional
	ResolvedSource *ResolvedSourceStatus `json:"resolvedSource,omitempty"`

	// conditions represent observations of the phantom lifecycle. Known types:
	// Ready (phantom exists and tracks measurements), Degraded (node missing,
	// Prometheus unreachable, resize rejected, PriorityClass absent).
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=sw
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=".spec.node"
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=".spec.source.type"
// +kubebuilder:printcolumn:name="ReserveCPU",type=string,JSONPath=".status.currentCPU"
// +kubebuilder:printcolumn:name="ReserveMem",type=string,JSONPath=".status.currentMemory"
// +kubebuilder:printcolumn:name="MeasuredCPU",type=string,JSONPath=".status.lastMeasurement.cpu"
// +kubebuilder:printcolumn:name="MeasuredMem",type=string,JSONPath=".status.lastMeasurement.memory"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Phantom",type=string,JSONPath=".status.phantomPod",priority=1
// +kubebuilder:printcolumn:name="MeasuredAt",type="date",JSONPath=".status.lastMeasurement.time",priority=1

// ShadowWorkload maintains a phantom ("ballast") pod whose resource requests
// mirror the real footprint of a bare-metal workload, making that footprint
// visible to the Kubernetes scheduler's allocatable ledger.
type ShadowWorkload struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ShadowWorkload
	// +required
	Spec ShadowWorkloadSpec `json:"spec"`

	// status defines the observed state of ShadowWorkload
	// +optional
	Status ShadowWorkloadStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ShadowWorkloadList contains a list of ShadowWorkload
type ShadowWorkloadList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ShadowWorkload `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ShadowWorkload{}, &ShadowWorkloadList{})
		return nil
	})
}
