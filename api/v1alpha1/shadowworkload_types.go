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
// +kubebuilder:validation:Enum=docker;cgroup;systemd;promql
type SourceType string

const (
	// SourceTypeDocker shadows Docker containers discovered via cAdvisor
	// cgroup id-prefix selectors (/system.slice/docker-<id>).
	SourceTypeDocker SourceType = "docker"

	// SourceTypeCgroup shadows cgroups selected by an absolute path glob in
	// cAdvisor's id label.
	SourceTypeCgroup SourceType = "cgroup"

	// SourceTypeSystemd shadows one systemd system service or scope. The unit
	// name and slice are resolved to an exact cgroup id.
	SourceTypeSystemd SourceType = "systemd"

	// SourceTypePromQL passes raw PromQL through to the configured
	// Prometheus. Escape hatch for anything the typed sources cannot
	// express.
	SourceTypePromQL SourceType = "promql"
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
// +kubebuilder:validation:XValidation:rule="[has(self.docker), has(self.cgroup), has(self.systemd), has(self.promql)].filter(x, x).size() == 1",message="exactly one of source.docker, source.cgroup, source.systemd or source.promql must be set"
// +kubebuilder:validation:XValidation:rule="(self.type == 'docker') == has(self.docker)",message="source.type must match the configured source block"
// +kubebuilder:validation:XValidation:rule="(self.type == 'cgroup') == has(self.cgroup)",message="source.type must match the configured source block"
// +kubebuilder:validation:XValidation:rule="(self.type == 'systemd') == has(self.systemd)",message="source.type must match the configured source block"
// +kubebuilder:validation:XValidation:rule="(self.type == 'promql') == has(self.promql)",message="source.type must match the configured source block"
type SourceSpec struct {
	// type selects the source implementation to resolve.
	Type SourceType `json:"type"`

	// docker shadows Docker containers running directly on the node
	// (outside the kubepods cgroup), as seen by the standalone cAdvisor job.
	// Required when type is docker.
	// +optional
	Docker *DockerSource `json:"docker,omitempty"`

	// cgroup shadows cgroups selected by an absolute path glob in cAdvisor's
	// id label. Required when type is cgroup.
	// +optional
	Cgroup *CgroupSource `json:"cgroup,omitempty"`

	// systemd shadows one system service or scope in a named systemd slice.
	// Required when type is systemd.
	// +optional
	Systemd *SystemdSource `json:"systemd,omitempty"`

	// promql supplies raw PromQL for CPU cores and memory bytes. Required when
	// type is promql. Both queries must return a single vector sample after
	// aggregation (use sum()).
	// +optional
	PromQL *PromQLSource `json:"promql,omitempty"`
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

// CgroupSource resolves an absolute cgroup path glob to a Prometheus id-label
// regular expression. The first path component is literal; * and ? may be used
// in subsequent components and never match '/'. This keeps the selector inside
// one explicit top-level hierarchy and prevents accidental kubepods accounting.
// +kubebuilder:validation:XValidation:rule="!self.pathGlob.contains('**')",message="cgroup.pathGlob does not support recursive ** wildcards"
// +kubebuilder:validation:XValidation:rule="!(self.pathGlob == '/kubepods' || self.pathGlob.startsWith('/kubepods/') || self.pathGlob.startsWith('/kubepods.'))",message="cgroup.pathGlob must not select the Kubernetes cgroup hierarchy"
type CgroupSource struct {
	// pathGlob selects one or more cAdvisor cgroup id paths. It must name a
	// literal non-kubepods top-level cgroup. '*' matches zero or more characters
	// and '?' matches one character within a path component. Recursive '**'
	// matching is intentionally unsupported.
	// +kubebuilder:validation:MinLength=2
	// +kubebuilder:validation:MaxLength=1024
	// +kubebuilder:validation:Pattern=`^/[-A-Za-z0-9_.:@%+=,~]+(/[-A-Za-z0-9_.:@%+=,~*?]+)*$`
	PathGlob string `json:"pathGlob"`

	// cadvisorJob is the Prometheus job label emitted by the standalone
	// cAdvisor instance monitoring this node.
	// +kubebuilder:default="cadvisor"
	CadvisorJob string `json:"cadvisorJob,omitempty"`

	// cadvisorInstance pins the selector to one standalone cAdvisor target.
	// It is required so a multi-target job cannot aggregate another node into
	// this node's scheduler reservation.
	// +kubebuilder:validation:MinLength=1
	CadvisorInstance string `json:"cadvisorInstance"`
}

// SystemdSource resolves a system service or scope and its systemd slice to an
// exact cAdvisor cgroup id. User-manager units are intentionally outside this
// typed source because their hierarchy includes a runtime UID; use cgroup for
// those paths.
type SystemdSource struct {
	// unit is the systemd .service or .scope unit to measure.
	// +kubebuilder:validation:MinLength=7
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_.:@-]+[.](service|scope)$`
	Unit string `json:"unit"`

	// slice is the systemd system slice containing unit. Nested slice names
	// such as media-services.slice are expanded to their cgroup hierarchy.
	// +kubebuilder:default="system.slice"
	// +kubebuilder:validation:MinLength=7
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_.]+(-[A-Za-z0-9_.]+)*[.]slice$`
	Slice string `json:"slice,omitempty"`

	// cadvisorJob is the Prometheus job label emitted by the standalone
	// cAdvisor instance monitoring this node.
	// +kubebuilder:default="cadvisor"
	CadvisorJob string `json:"cadvisorJob,omitempty"`

	// cadvisorInstance pins the selector to one standalone cAdvisor target.
	// It is required so a multi-target job cannot aggregate another node into
	// this node's scheduler reservation.
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
	// queries (docker, cgroup and systemd sources) — the smoothing horizon of
	// the moving average.
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
// +kubebuilder:printcolumn:name="Phantom",type=string,JSONPath=".status.phantomPod"
// +kubebuilder:printcolumn:name="CPU",type=string,JSONPath=".status.currentCPU"
// +kubebuilder:printcolumn:name="Memory",type=string,JSONPath=".status.currentMemory"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"

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
