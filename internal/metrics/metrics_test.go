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

package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

const (
	nsA = "prod"
	swA = "sw-alpha"
)

var stampA = time.Unix(1756000000, 0)

// allSeries gathers every series currently exposed by reg, keyed by metric
// name, with each sample's label pairs and value.
func allSeries(t *testing.T, reg *prometheus.Registry) map[string][]sampleView {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	out := make(map[string][]sampleView)
	for _, mf := range families {
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			value := 0.0
			switch {
			case m.GetGauge() != nil:
				value = m.GetGauge().GetValue()
			case m.GetCounter() != nil:
				value = m.GetCounter().GetValue()
			}
			out[mf.GetName()] = append(out[mf.GetName()], sampleView{labels: labels, value: value})
		}
	}
	return out
}

type sampleView struct {
	labels map[string]string
	value  float64
}

func TestFrozenSeriesContract(t *testing.T) {
	rec := New()
	reg := prometheus.NewRegistry()
	if err := rec.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	rec.ObservedMeasurement(nsA, swA, 2.5, 6442450944)
	rec.MeasurementSucceeded(nsA, swA, stampA)
	rec.SetReserved(nsA, swA, 2.5, 4294967296)
	rec.ReservationUpdated(nsA, swA)
	rec.MeasurementFailed(nsA, swA, FailurePrometheusUnavailable)

	want := `
# HELP kube_symbiont_shadowworkload_last_successful_measurement_timestamp_seconds Unix timestamp of the last successful Prometheus measurement for this ShadowWorkload.
# TYPE kube_symbiont_shadowworkload_last_successful_measurement_timestamp_seconds gauge
kube_symbiont_shadowworkload_last_successful_measurement_timestamp_seconds{name="sw-alpha",namespace="prod"} 1.756e+09
# HELP kube_symbiont_shadowworkload_measured_cpu_cores Bare-metal CPU footprint last measured for this ShadowWorkload, in cores, before clamping.
# TYPE kube_symbiont_shadowworkload_measured_cpu_cores gauge
kube_symbiont_shadowworkload_measured_cpu_cores{name="sw-alpha",namespace="prod"} 2.5
# HELP kube_symbiont_shadowworkload_measured_memory_bytes Bare-metal memory footprint last measured for this ShadowWorkload, in bytes, before clamping.
# TYPE kube_symbiont_shadowworkload_measured_memory_bytes gauge
kube_symbiont_shadowworkload_measured_memory_bytes{name="sw-alpha",namespace="prod"} 6.442450944e+09
# HELP kube_symbiont_shadowworkload_measurement_failures_total Failed measurements or reservation updates for this ShadowWorkload, by bounded reason.
# TYPE kube_symbiont_shadowworkload_measurement_failures_total counter
kube_symbiont_shadowworkload_measurement_failures_total{name="sw-alpha",namespace="prod",reason="prometheus_unavailable"} 1
# HELP kube_symbiont_shadowworkload_reserved_cpu_cores CPU requests currently held by this ShadowWorkload's phantom on the scheduler ledger, in cores.
# TYPE kube_symbiont_shadowworkload_reserved_cpu_cores gauge
kube_symbiont_shadowworkload_reserved_cpu_cores{name="sw-alpha",namespace="prod"} 2.5
# HELP kube_symbiont_shadowworkload_reserved_memory_bytes Memory requests currently held by this ShadowWorkload's phantom on the scheduler ledger, in bytes.
# TYPE kube_symbiont_shadowworkload_reserved_memory_bytes gauge
kube_symbiont_shadowworkload_reserved_memory_bytes{name="sw-alpha",namespace="prod"} 4.294967296e+09
# HELP kube_symbiont_shadowworkload_reservation_updates_total Accepted reservation mutations (phantom created or in-place resize accepted) for this ShadowWorkload.
# TYPE kube_symbiont_shadowworkload_reservation_updates_total counter
kube_symbiont_shadowworkload_reservation_updates_total{name="sw-alpha",namespace="prod"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want)); err != nil {
		t.Fatalf("frozen contract drifted:\n%v", err)
	}
}

func TestFailureReasonEnumBounds(t *testing.T) {
	allowed := map[FailureReason]struct{}{
		FailurePrometheusUnavailable: {},
		FailureMultiSample:           {},
		FailureResizeRejected:        {},
		FailureSourceMissing:         {},
	}
	if len(allowed) != 4 {
		t.Fatalf("enum cardinality changed: %d reasons", len(allowed))
	}
	for reason := range allowed {
		if !reason.valid() {
			t.Errorf("documented reason %q not accepted by recorder", reason)
		}
		if !strings.Contains(string(reason), "_") || strings.ToUpper(string(reason)) == string(reason) {
			t.Errorf("reason %q must stay snake_case", reason)
		}
	}

	rec := New()
	reg := prometheus.NewRegistry()
	_ = rec.Register(reg)
	rec.ObservedMeasurement(nsA, swA, 1, 1)
	rec.MeasurementFailed(nsA, swA, FailureReason("node down: boom"))
	rec.MeasurementFailed(nsA, swA, FailureReason("http://secret-host:9090 refused"))
	series := allSeries(t, reg)
	failures, ok := series[MetricFailures]
	if ok && len(failures) != 0 {
		t.Fatalf("unbounded reason leaked into failure label domain: %+v", failures)
	}
}

func TestRemoveShadowWorkloadDropsEverySeries(t *testing.T) {
	rec := New()
	reg := prometheus.NewRegistry()
	_ = rec.Register(reg)
	rec.ObservedMeasurement("ns-one", "sw-a", 1, 1024)
	rec.MeasurementSucceeded("ns-one", "sw-a", stampA)
	rec.SetReserved("ns-one", "sw-a", 1, 2048)
	rec.ReservationUpdated("ns-one", "sw-a")
	rec.MeasurementFailed("ns-one", "sw-a", FailureMultiSample)
	rec.ObservedMeasurement("ns-two", "sw-b", 3, 4096)
	rec.MeasurementSucceeded("ns-two", "sw-b", stampA)
	rec.SetReserved("ns-two", "sw-b", 3, 8192)
	rec.ReservationUpdated("ns-two", "sw-b")

	familyNames := []string{
		"kube_symbiont_shadowworkload_measured_cpu_cores",
		"kube_symbiont_shadowworkload_measured_memory_bytes",
		"kube_symbiont_shadowworkload_reserved_cpu_cores",
		"kube_symbiont_shadowworkload_reserved_memory_bytes",
		"kube_symbiont_shadowworkload_last_successful_measurement_timestamp_seconds",
		"kube_symbiont_shadowworkload_reservation_updates_total",
		MetricFailures,
	}
	for _, name := range familyNames {
		want := 2
		if name == MetricFailures {
			want = 1 // only sw-a ever failed
		}
		if got := testutil.CollectAndCount(rec, name); got != want {
			t.Fatalf("%s: got %d series before removal, want %d", name, got, want)
		}
	}

	rec.RemoveShadowWorkload("ns-one", "sw-a")
	series := allSeries(t, reg)
	for _, name := range familyNames {
		for _, s := range series[name] {
			if s.labels[LabelNamespace] == "ns-one" || s.labels[LabelName] == "sw-a" {
				t.Errorf("%s: stale series survived CR deletion: %+v", name, s.labels)
			}
		}
	}
	for _, name := range familyNames[:len(familyNames)-1] {
		if got := testutil.CollectAndCount(rec, name); got != 1 {
			t.Errorf("%s: survivor workload lost: got %d series, want 1", name, got)
		}
	}
}

func TestNoExtraLabelsEverAppear(t *testing.T) {
	rec := New()
	reg := prometheus.NewRegistry()
	_ = rec.Register(reg)
	for _, reason := range []FailureReason{
		FailurePrometheusUnavailable, FailureMultiSample, FailureResizeRejected, FailureSourceMissing,
	} {
		rec.ObservedMeasurement("ns", "name-with-\"quotes\"", 1, 2)
		rec.MeasurementFailed("ns", `back\slash`, reason)
	}
	allowedCommon := map[string]struct{}{LabelNamespace: {}, LabelName: {}}
	allowedFailures := map[string]struct{}{LabelNamespace: {}, LabelName: {}, LabelReason: {}}
	for name, samples := range allSeries(t, reg) {
		allowed := allowedCommon
		if name == MetricFailures {
			allowed = allowedFailures
		}
		for _, s := range samples {
			for key := range s.labels {
				if _, ok := allowed[key]; !ok {
					t.Errorf("%s: unexpected label %q (cardinality leak)", name, key)
				}
			}
		}
	}
}

func TestRegisterToleratesDuplicateRegistration(t *testing.T) {
	reg := prometheus.NewRegistry()
	first := New()
	if err := first.Register(reg); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	if err := first.Register(reg); err != nil {
		t.Fatalf("re-Register on same registry should be tolerated: %v", err)
	}
	if err := New().Register(reg); err != nil {
		t.Fatalf("second recorder on shared registry should be tolerated: %v", err)
	}
}

func TestNilRecorderIsNoop(t *testing.T) {
	var rec *Recorder
	rec.ObservedMeasurement(nsA, swA, 1, 2)
	rec.MeasurementSucceeded(nsA, swA, time.Now())
	rec.SetReserved(nsA, swA, 1, 2)
	rec.ReservationUpdated(nsA, swA)
	rec.MeasurementFailed(nsA, swA, FailureSourceMissing)
	rec.MeasurementFailed(nsA, swA, "untyped")
	rec.RemoveShadowWorkload(nsA, swA)
	if err := rec.Register(prometheus.NewRegistry()); err != nil {
		t.Fatalf("nil Register: %v", err)
	}
}
