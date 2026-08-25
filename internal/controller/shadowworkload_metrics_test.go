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

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	symbiontv1alpha1 "github.com/michaelwetyyy/kube-symbiont/api/v1alpha1"
	"github.com/michaelwetyyy/kube-symbiont/internal/metrics"
	"github.com/michaelwetyyy/kube-symbiont/internal/shadow"
	"github.com/michaelwetyyy/kube-symbiont/internal/sources"
)

// gatheredSeries indexes every exposed sample by metric name so specs can
// assert on exact label sets and values without reaching into vec internals.
type gatheredSeries map[string][]exposedSample

type exposedSample struct {
	labels map[string]string
	value  float64
}

func gatherSeries(reg *prometheus.Registry) gatheredSeries {
	out := gatheredSeries{}
	families, err := reg.Gather()
	Expect(err).NotTo(HaveOccurred())
	for _, mf := range families {
		for _, m := range mf.GetMetric() {
			sample := exposedSample{labels: map[string]string{}}
			for _, lp := range m.GetLabel() {
				sample.labels[lp.GetName()] = lp.GetValue()
			}
			switch {
			case m.GetGauge() != nil:
				sample.value = m.GetGauge().GetValue()
			case m.GetCounter() != nil:
				sample.value = m.GetCounter().GetValue()
			default:
				Fail("unexpected untyped metric " + mf.GetName())
			}
			out[mf.GetName()] = append(out[mf.GetName()], sample)
		}
	}
	return out
}

func (g gatheredSeries) one(name string, labels map[string]string) (float64, bool) {
	for _, s := range g[name] {
		match := len(s.labels) == len(labels)
		for k, v := range labels {
			if s.labels[k] != v {
				match = false
			}
		}
		if match {
			return s.value, true
		}
	}
	return 0, false
}

var _ = Describe("ShadowWorkload Metrics", func() {
	const (
		metricResource = "metric-shadow"
		metricNS       = "default"
	)

	ctx := context.Background()
	metricKey := types.NamespacedName{Name: metricResource, Namespace: metricNS}
	seriesLabels := map[string]string{
		metrics.LabelNamespace: metricNS,
		metrics.LabelName:      metricResource,
	}

	failureLabels := func(reason metrics.FailureReason) map[string]string {
		return map[string]string{
			metrics.LabelNamespace: metricNS,
			metrics.LabelName:      metricResource,
			metrics.LabelReason:    string(reason),
		}
	}

	const (
		famMeasuredCPU    = metrics.MetricMeasuredCPU
		famMeasuredMemory = metrics.MetricMeasuredMemory
		famReservedCPU    = metrics.MetricReservedCPU
		famReservedMemory = metrics.MetricReservedMemory
		famLastSuccess    = metrics.MetricLastSuccess
		famUpdates        = metrics.MetricUpdates
		famFailures       = metrics.MetricFailures
	)

	var (
		metricRegistry   *prometheus.Registry
		swMetrics        *metrics.Recorder
		metricsReconcile *ShadowWorkloadReconciler
		metricsStub      *stubQuerier
	)

	deleteAndWait := func(obj client.Object) {
		_ = k8sClient.Delete(ctx, obj, client.GracePeriodSeconds(0))
		objKey := client.ObjectKeyFromObject(obj)
		Eventually(func() bool {
			err := k8sClient.Get(ctx, objKey, obj)
			return apierrors.IsNotFound(err)
		}).Should(BeTrue())
	}

	BeforeEach(func() {
		By("installing cluster prerequisites: ballast PriorityClass and target node")
		Expect(k8sClient.Create(ctx, &schedulingv1.PriorityClass{
			ObjectMeta:    metav1.ObjectMeta{Name: shadow.BallastPriorityClassName},
			Value:         1000,
			GlobalDefault: false,
		})).To(Succeed())
		node := makeTestNode(testNodeName, &readyTrue)
		Expect(k8sClient.Create(ctx, node)).To(Succeed())

		metricsStub = &stubQuerier{cpu: 2, mem: 6 * 1024 * 1024 * 1024}
		swMetrics = metrics.New()
		metricRegistry = prometheus.NewRegistry()
		Expect(swMetrics.Register(metricRegistry)).To(Succeed())
		metricsReconcile = &ShadowWorkloadReconciler{
			Client:    k8sClient,
			Scheme:    k8sClient.Scheme(),
			Recorder:  record.NewFakeRecorder(32),
			APIReader: k8sClient,
			Metrics:   swMetrics,
			QuerierFor: func(string) (MetricsQuerier, error) {
				return metricsStub, nil
			},
		}
		Expect(k8sClient.Create(ctx, validShadowWorkload(metricResource))).To(Succeed())
	})

	AfterEach(func() {
		deleteAndWait(&symbiontv1alpha1.ShadowWorkload{
			ObjectMeta: metav1.ObjectMeta{Name: metricResource, Namespace: metricKey.Namespace},
		})
		deleteAndWait(&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "shadow-" + metricResource, Namespace: metricKey.Namespace},
		})
		deleteAndWait(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: testNodeName}})
		deleteAndWait(&schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: shadow.BallastPriorityClassName}})
	})

	reconcileOnce := func() {
		_, err := metricsReconcile.Reconcile(ctx, reconcile.Request{NamespacedName: metricKey})
		Expect(err).NotTo(HaveOccurred())
	}

	It("should expose the frozen series after a successful reconcile", func() {
		By("reconciling once with a healthy measurement pass")
		reconcileOnce()

		g := gatherSeries(metricRegistry)
		cpu, ok := g.one(famMeasuredCPU, seriesLabels)
		Expect(ok).To(BeTrue(), "measured cpu series must exist")
		Expect(cpu).To(Equal(2.0))
		mem, _ := g.one(famMeasuredMemory, seriesLabels)
		Expect(mem).To(Equal(float64(6 * 1024 * 1024 * 1024)))

		reservedCPU, ok := g.one(famReservedCPU, seriesLabels)
		Expect(ok).To(BeTrue(), "reserved cpu series must exist")
		Expect(reservedCPU).To(Equal(2.0))
		reservedMem, _ := g.one(famReservedMemory, seriesLabels)
		Expect(reservedMem).To(Equal(float64(6 * 1024 * 1024 * 1024)))

		stamp, ok := g.one(famLastSuccess, seriesLabels)
		Expect(ok).To(BeTrue(), "last-successful-measurement timestamp must exist")
		Expect(stamp).To(BeNumerically(">", float64(time.Now().Add(-2*time.Minute).Unix())))

		updates, _ := g.one(famUpdates, seriesLabels)
		Expect(updates).To(Equal(1.0), "phantom creation counts as the first reservation update")

		_, exists := g.one(famFailures, seriesLabels)
		Expect(exists).To(BeFalse(), "a clean pass must not emit failure series")
	})

	It("should classify a metrics outage as prometheus_unavailable and keep last truth", func() {
		reconcileOnce()
		before := gatherSeries(metricRegistry)
		stampBefore, _ := before.one(famLastSuccess, seriesLabels)

		By("failing the backend query on the next poll")
		metricsStub.err = errors.New("simulated outage")
		reconcileOnce()

		g := gatherSeries(metricRegistry)
		failures, ok := g.one(famFailures, failureLabels(metrics.FailurePrometheusUnavailable))
		Expect(ok).To(BeTrue())
		Expect(failures).To(Equal(1.0))

		stampAfter, _ := g.one(famLastSuccess, seriesLabels)
		Expect(stampAfter).To(Equal(stampBefore), "failed passes must not refresh the success timestamp")

		measuredCPU, _ := g.one(famMeasuredCPU, seriesLabels)
		Expect(measuredCPU).To(Equal(2.0), "outage leaves the last measured footprint untouched")

		_, multiSample := g.one(famFailures, failureLabels(metrics.FailureMultiSample))
		Expect(multiSample).To(BeFalse())
	})

	It("should classify multi-sample replies via the bounded enum", func() {
		metricsStub.err = fmt.Errorf("cpu query: %w", sources.ErrMultiSample)
		reconcileOnce()

		g := gatherSeries(metricRegistry)
		failures, ok := g.one(famFailures, failureLabels(metrics.FailureMultiSample))
		Expect(ok).To(BeTrue())
		Expect(failures).To(Equal(1.0))
		_, unavailable := g.one(famFailures, failureLabels(metrics.FailurePrometheusUnavailable))
		Expect(unavailable).To(BeFalse(), "multi-sample replies are their own reason, not a generic outage")
	})

	It("should count rejected resizes and keep reserved at last accepted requests", func() {
		reconcileOnce()

		current := &symbiontv1alpha1.ShadowWorkload{}
		Expect(k8sClient.Get(ctx, metricKey, current)).To(Succeed())
		current.Spec.Update.Ceiling.Memory = resource.MustParse("128Gi")
		Expect(k8sClient.Update(ctx, current)).To(Succeed())
		metricsStub.mem = 128 * 1024 * 1024 * 1024

		reconcileOnce()

		g := gatherSeries(metricRegistry)
		failures, ok := g.one(famFailures, failureLabels(metrics.FailureResizeRejected))
		Expect(ok).To(BeTrue())
		Expect(failures).To(Equal(1.0))

		reservedMem, _ := g.one(famReservedMemory, seriesLabels)
		Expect(reservedMem).To(Equal(float64(6*1024*1024*1024)), "rejected resize keeps the accepted reservation")
		measuredMem, _ := g.one(famMeasuredMemory, seriesLabels)
		Expect(measuredMem).To(Equal(float64(128*1024*1024*1024)), "the raw measurement still moves")

		updates, _ := g.one(famUpdates, seriesLabels)
		Expect(updates).To(Equal(1.0), "rejections are not reservation updates")
	})

	It("should remove every series when the ShadowWorkload is deleted", func() {
		reconcileOnce()
		Expect(gatherSeries(metricRegistry)).NotTo(BeEmpty())

		By("deleting the CR and letting the reconciler observe the tombstone")
		deleteAndWait(&symbiontv1alpha1.ShadowWorkload{
			ObjectMeta: metav1.ObjectMeta{Name: metricResource, Namespace: metricKey.Namespace},
		})
		reconcileOnce()

		g := gatherSeries(metricRegistry)
		for _, family := range []string{
			famMeasuredCPU, famMeasuredMemory, famReservedCPU, famReservedMemory,
			famLastSuccess, famUpdates, famFailures,
		} {
			Expect(g[family]).To(BeEmpty(), "deletion must drop all %s series", family)
		}
	})

	It("must never leak cardinality beyond namespace/name/reason labels", func() {
		By("running create, outage and rejection paths against input-laden fixtures")
		reconcileOnce()
		metricsStub.err = errors.New("transport blew up mentioning http://secret-prometheus:9090/api/v1/query")
		reconcileOnce()
		metricsStub.err = nil
		reconcileOnce()

		allowedCommon := map[string]struct{}{
			metrics.LabelNamespace: {}, metrics.LabelName: {},
		}
		allowedFailures := map[string]struct{}{
			metrics.LabelNamespace: {}, metrics.LabelName: {}, metrics.LabelReason: {},
		}
		for family, samples := range gatherSeries(metricRegistry) {
			allowed := allowedCommon
			if family == famFailures {
				allowed = allowedFailures
			}
			for _, s := range samples {
				for labelKey, labelValue := range s.labels {
					if _, ok := allowed[labelKey]; !ok {
						Fail(fmt.Sprintf("%s leaked label %q=%q", family, labelKey, labelValue))
					}
					if labelKey != metrics.LabelReason && labelValue != metricNS && labelValue != metricResource {
						Fail(fmt.Sprintf("%s label %q carries unexpected value %q", family, labelKey, labelValue))
					}
					if labelKey == metrics.LabelReason {
						allowedReasons := map[string]struct{}{
							string(metrics.FailurePrometheusUnavailable): {},
							string(metrics.FailureMultiSample):           {},
							string(metrics.FailureResizeRejected):        {},
							string(metrics.FailureSourceMissing):         {},
						}
						if _, ok := allowedReasons[labelValue]; !ok {
							Fail("failure reason outside the code-owned enum: " + labelValue)
						}
					}
				}
			}
		}
	})
})
