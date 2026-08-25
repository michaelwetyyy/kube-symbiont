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

// Package metrics instruments the ShadowWorkload measure → clamp → resize
// loop on the manager's Prometheus registry.
//
// The series contract is frozen: every series is keyed by namespace and name
// only (plus a closed failure-reason enum for failures), and all series for a
// deleted ShadowWorkload are removed so the registry never accumulates stale
// objects. High-cardinality values — URLs, PromQL, node names, image IDs,
// error text or any other user input — must never become labels here.
package metrics

import (
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Label keys used across every kube_symbiont_shadowworkload_* series.
const (
	LabelNamespace = "namespace"
	LabelName      = "name"
	LabelReason    = "reason"
)

// Frozen series names. Renaming any of these breaks the scrape contract.
const (
	MetricMeasuredCPU    = "kube_symbiont_shadowworkload_measured_cpu_cores"
	MetricMeasuredMemory = "kube_symbiont_shadowworkload_measured_memory_bytes"
	MetricReservedCPU    = "kube_symbiont_shadowworkload_reserved_cpu_cores"
	MetricReservedMemory = "kube_symbiont_shadowworkload_reserved_memory_bytes"
	MetricLastSuccess    = "kube_symbiont_shadowworkload_last_successful_measurement_timestamp_seconds"
	MetricUpdates        = "kube_symbiont_shadowworkload_reservation_updates_total"
	MetricFailures       = "kube_symbiont_shadowworkload_measurement_failures_total"
)

// FailureReason is a closed enumeration of why a measurement or a reservation
// update failed. As a distinct type it cannot be confused with raw error
// text: an unknown value is dropped rather than exported as a label.
type FailureReason string

const (
	// FailurePrometheusUnavailable covers transport, HTTP and API-status
	// errors from the metrics backend (excluding multi-sample replies).
	FailurePrometheusUnavailable FailureReason = "prometheus_unavailable"

	// FailureMultiSample marks a query that resolved to more than one vector
	// sample; the controller refuses to pick one silently.
	FailureMultiSample FailureReason = "multi_sample"

	// FailureResizeRejected marks an in-place resize rejected by the API
	// server; the phantom keeps its last accepted requests.
	FailureResizeRejected FailureReason = "resize_rejected"

	// FailureSourceMissing marks an unresolvable spec.source: a missing or
	// mismatched source block, or an unusable metrics backend URL.
	FailureSourceMissing FailureReason = "source_missing"
)

// knownFailureReasons bounds the reason label domain to the constants above.
var knownFailureReasons = map[FailureReason]struct{}{
	FailurePrometheusUnavailable: {},
	FailureMultiSample:           {},
	FailureResizeRejected:        {},
	FailureSourceMissing:         {},
}

// valid reports whether the reason is part of the code-owned enum.
func (fr FailureReason) valid() bool {
	_, ok := knownFailureReasons[fr]
	return ok
}

// Recorder owns the kube_symbiont_shadowworkload_* series. All methods are
// safe on a nil *Recorder so callers can instrument unconditionally; tests
// and library users that construct a Reconciler by hand get no-ops unless a
// recorder was injected through SetupWithManager.
type Recorder struct {
	measuredCPU       *prometheus.GaugeVec
	measuredMemory    *prometheus.GaugeVec
	reservedCPU       *prometheus.GaugeVec
	reservedMemory    *prometheus.GaugeVec
	lastSuccessfulRun *prometheus.GaugeVec
	reservationUpdate *prometheus.CounterVec
	measurementFail   *prometheus.CounterVec
}

// New returns an unregistered Recorder with the frozen series contract:
//
//	kube_symbiont_shadowworkload_measured_cpu_cores{namespace,name}                        gauge
//	kube_symbiont_shadowworkload_measured_memory_bytes{namespace,name}                     gauge
//	kube_symbiont_shadowworkload_reserved_cpu_cores{namespace,name}                        gauge
//	kube_symbiont_shadowworkload_reserved_memory_bytes{namespace,name}                     gauge
//	kube_symbiont_shadowworkload_last_successful_measurement_timestamp_seconds{namespace,name} gauge
//	kube_symbiont_shadowworkload_reservation_updates_total{namespace,name}                 counter
//	kube_symbiont_shadowworkload_measurement_failures_total{namespace,name,reason}         counter
func New() *Recorder {
	common := make([]string, 0, 3)
	common = append(common, LabelNamespace, LabelName)
	return &Recorder{
		measuredCPU: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: MetricMeasuredCPU,
			Help: "Bare-metal CPU footprint last measured for this ShadowWorkload, in cores, before clamping.",
		}, common),
		measuredMemory: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: MetricMeasuredMemory,
			Help: "Bare-metal memory footprint last measured for this ShadowWorkload, in bytes, before clamping.",
		}, common),
		reservedCPU: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: MetricReservedCPU,
			Help: "CPU requests currently held by this ShadowWorkload's phantom on the scheduler ledger, in cores.",
		}, common),
		reservedMemory: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: MetricReservedMemory,
			Help: "Memory requests currently held by this ShadowWorkload's phantom on the scheduler ledger, in bytes.",
		}, common),
		lastSuccessfulRun: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: MetricLastSuccess,
			Help: "Unix timestamp of the last successful Prometheus measurement for this ShadowWorkload.",
		}, common),
		reservationUpdate: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: MetricUpdates,
			Help: "Accepted reservation mutations (phantom created or in-place resize accepted) for this ShadowWorkload.",
		}, common),
		measurementFail: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: MetricFailures,
			Help: "Failed measurements or reservation updates for this ShadowWorkload, by bounded reason.",
		}, append(common, LabelReason)),
	}
}

// Register adds every collector to reg. Duplicate re-registration (for
// example a second manager sharing one registry) is tolerated.
func (r *Recorder) Register(reg prometheus.Registerer) error {
	if r == nil {
		return nil
	}
	for _, c := range r.gaugeVecs() {
		if err := registerToleratingDuplicates(reg, c); err != nil {
			return err
		}
	}
	for _, c := range r.counterVecs() {
		if err := registerToleratingDuplicates(reg, c); err != nil {
			return err
		}
	}
	return nil
}

func registerToleratingDuplicates(reg prometheus.Registerer, c prometheus.Collector) error {
	if err := reg.Register(c); err != nil {
		var alreadyRegistered prometheus.AlreadyRegisteredError
		if !errors.As(err, &alreadyRegistered) {
			return err
		}
	}
	return nil
}

// gaugeVecs returns every gauge vector for registration and cleanup.
func (r *Recorder) gaugeVecs() []*prometheus.GaugeVec {
	return []*prometheus.GaugeVec{
		r.measuredCPU,
		r.measuredMemory,
		r.reservedCPU,
		r.reservedMemory,
		r.lastSuccessfulRun,
	}
}

// counterVecs returns every counter vector for registration and cleanup.
func (r *Recorder) counterVecs() []*prometheus.CounterVec {
	return []*prometheus.CounterVec{
		r.reservationUpdate,
		r.measurementFail,
	}
}

// ObservedMeasurement records the raw measured footprint of a successful
// query pass, before floor/ceiling clamping. An idle workload (no series)
// legitimately measures zero.
func (r *Recorder) ObservedMeasurement(namespace, name string, cpuCores, memoryBytes float64) {
	if r == nil {
		return
	}
	r.measuredCPU.WithLabelValues(namespace, name).Set(cpuCores)
	r.measuredMemory.WithLabelValues(namespace, name).Set(memoryBytes)
}

// MeasurementSucceeded stamps the unix-seconds timestamp of the last
// successful measurement pass.
func (r *Recorder) MeasurementSucceeded(namespace, name string, at time.Time) {
	if r == nil || at.IsZero() {
		return
	}
	r.lastSuccessfulRun.WithLabelValues(namespace, name).Set(float64(at.Unix()))
}

// SetReserved records the effective requests currently held by this
// ShadowWorkload's phantom on the scheduler ledger. While prerequisites are
// unavailable the last observed truth is kept untouched.
func (r *Recorder) SetReserved(namespace, name string, cpuCores, memoryBytes float64) {
	if r == nil {
		return
	}
	r.reservedCPU.WithLabelValues(namespace, name).Set(cpuCores)
	r.reservedMemory.WithLabelValues(namespace, name).Set(memoryBytes)
}

// ReservationUpdated counts one accepted reservation mutation: a phantom
// creation or an accepted in-place resize.
func (r *Recorder) ReservationUpdated(namespace, name string) {
	if r == nil {
		return
	}
	r.reservationUpdate.WithLabelValues(namespace, name).Inc()
}

// MeasurementFailed counts a failed measurement or reservation update under
// one of the enumerated reasons. Values outside the enum are dropped so raw
// error text can never reach the label domain.
func (r *Recorder) MeasurementFailed(namespace, name string, reason FailureReason) {
	if r == nil || !reason.valid() {
		return
	}
	r.measurementFail.WithLabelValues(namespace, name, string(reason)).Inc()
}

// Describe implements prometheus.Collector.
func (r *Recorder) Describe(ch chan<- *prometheus.Desc) {
	for _, vec := range r.gaugeVecs() {
		vec.Describe(ch)
	}
	for _, vec := range r.counterVecs() {
		vec.Describe(ch)
	}
}

// Collect implements prometheus.Collector.
func (r *Recorder) Collect(ch chan<- prometheus.Metric) {
	for _, vec := range r.gaugeVecs() {
		vec.Collect(ch)
	}
	for _, vec := range r.counterVecs() {
		vec.Collect(ch)
	}
}

// RemoveShadowWorkload drops every series belonging to a deleted
// ShadowWorkload. DeletePartialMatch keys on namespace/name only, so it also
// clears the reason-partitioned failure counters.
func (r *Recorder) RemoveShadowWorkload(namespace, name string) {
	if r == nil {
		return
	}
	match := prometheus.Labels{LabelNamespace: namespace, LabelName: name}
	for _, vec := range r.gaugeVecs() {
		vec.DeletePartialMatch(match)
	}
	for _, vec := range r.counterVecs() {
		vec.DeletePartialMatch(match)
	}
}
