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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
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
	"github.com/michaelwetyyy/kube-symbiont/internal/shadow"
	"github.com/michaelwetyyy/kube-symbiont/internal/sources"
)

// stubQuerier returns a canned CPU-cores/memory-bytes measurement so the
// reconcile loop runs end-to-end against envtest without a Prometheus.
type stubQuerier struct {
	cpu float64
	mem float64
}

func (s *stubQuerier) QueryPair(_ context.Context, _ sources.Queries) (float64, float64, bool, error) {
	return s.cpu, s.mem, true, nil
}

func validShadowWorkload(name string) *symbiontv1alpha1.ShadowWorkload {
	return &symbiontv1alpha1.ShadowWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: symbiontv1alpha1.ShadowWorkloadSpec{
			Node: "lab",
			Source: symbiontv1alpha1.SourceSpec{
				Type:   symbiontv1alpha1.SourceTypeDocker,
				Docker: &symbiontv1alpha1.DockerSource{Selector: symbiontv1alpha1.SelectorAll},
			},
			Metrics: symbiontv1alpha1.MetricsConfig{
				PrometheusURL: "http://prometheus.monitoring:9090",
				Window:        "5m",
			},
			Update: symbiontv1alpha1.UpdatePolicy{
				DeltaThresholdPercent: 10,
				PollInterval:          "30s",
				Floor: symbiontv1alpha1.ResourcePair{
					CPU: resource.MustParse("10m"), Memory: resource.MustParse("32Mi"),
				},
				Ceiling: symbiontv1alpha1.ResourcePair{
					CPU: resource.MustParse("8"), Memory: resource.MustParse("32Gi"),
				},
			},
		},
	}
}

var _ = Describe("ShadowWorkload Controller", func() {
	const resourceName = "test-shadow"

	ctx := context.Background()
	key := types.NamespacedName{Name: resourceName, Namespace: "default"}

	var (
		reconciler *ShadowWorkloadReconciler
		stub       *stubQuerier
	)

	// deleteAndWait makes cleanup deterministic: envtest ships no garbage
	// collector, so an async Delete would let a previous spec's phantom leak
	// into the next spec's reconcile as a foreign-owned pod. Force grace
	// period zero too — a nodeName-pinned pod is otherwise deleted
	// gracefully and lingers Terminating forever without a kubelet.
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
		pc := &schedulingv1.PriorityClass{
			ObjectMeta:    metav1.ObjectMeta{Name: shadow.BallastPriorityClassName},
			Value:         1000,
			GlobalDefault: false,
		}
		Expect(k8sClient.Create(ctx, pc)).To(Succeed())
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "lab"}}
		// In-place resize admission validates the delta against the node's
		// allocatable; a bare Node fixture reports zero and would reject the
		// resize. Size it like the real lab host (64GB RAM).
		capacity := corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("32"),
			corev1.ResourceMemory: resource.MustParse("64Gi"),
			corev1.ResourcePods:   resource.MustParse("110"),
		}
		node.Status.Capacity = capacity
		node.Status.Allocatable = capacity.DeepCopy()
		Expect(k8sClient.Create(ctx, node)).To(Succeed())

		stub = &stubQuerier{cpu: 2, mem: 6 * 1024 * 1024 * 1024}
		reconciler = &ShadowWorkloadReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Recorder: record.NewFakeRecorder(64),
			QuerierFor: func(string) (MetricsQuerier, error) {
				return stub, nil
			},
		}
	})

	AfterEach(func() {
		sw := &symbiontv1alpha1.ShadowWorkload{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}}
		deleteAndWait(sw)
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "shadow-" + key.Name, Namespace: key.Namespace}}
		deleteAndWait(pod)
		deleteAndWait(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "lab"}})
		deleteAndWait(&schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: shadow.BallastPriorityClassName}})
	})

	It("should reject a source spec violating the one-of constraint", func() {
		bad := validShadowWorkload("bad-oneof")
		bad.Spec.Source.Docker = nil // type=docker with no docker block
		err := k8sClient.Create(ctx, bad)
		Expect(err).To(HaveOccurred())
		Expect(apierrors.IsInvalid(err)).To(BeTrue())
	})

	It("should create the phantom and resize it in place when drift exceeds the threshold", func() {
		By("creating a valid ShadowWorkload")
		Expect(k8sClient.Create(ctx, validShadowWorkload(resourceName))).To(Succeed())

		phantomKey := types.NamespacedName{Name: "shadow-" + resourceName, Namespace: key.Namespace}

		By("reconciling once: phantom should be created at the measured pair")
		res, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter.String()).To(Equal("30s"))

		pod := &corev1.Pod{}
		Eventually(func() error {
			return k8sClient.Get(ctx, phantomKey, pod)
		}).Should(Succeed())
		Expect(pod.Spec.NodeName).To(Equal("lab"))
		Expect(pod.Spec.PriorityClassName).To(Equal(shadow.BallastPriorityClassName))
		req := pod.Spec.Containers[0].Resources.Requests
		lim := pod.Spec.Containers[0].Resources.Limits
		Expect(req.Cpu().String()).To(Equal("2"))
		Expect(req.Memory().String()).To(Equal("6Gi"))
		// Guaranteed QoS: requests == limits.
		Expect(lim.Cpu().String()).To(Equal("2"))
		Expect(lim.Memory().String()).To(Equal("6Gi"))
		Expect(metav1.GetControllerOf(pod)).NotTo(BeNil())

		updated := &symbiontv1alpha1.ShadowWorkload{}
		Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
		Expect(updated.Status.PhantomPod).To(Equal(phantomKey.Name))
		Expect(updated.Status.CurrentCPU.String()).To(Equal("2"))
		Expect(updated.Status.CurrentMemory.String()).To(Equal("6Gi"))

		By("doubling the measurement: drift exceeds threshold, in-place resize expected")
		stub.cpu = 4

		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, phantomKey, pod)).To(Succeed())
			r := pod.Spec.Containers[0].Resources.Requests
			l := pod.Spec.Containers[0].Resources.Limits
			// Requests AND limits move together (Guaranteed QoS preserved).
			g.Expect(r.Cpu().String()).To(Equal("4"))
			g.Expect(l.Cpu().String()).To(Equal("4"))
		}).Should(Succeed())

		Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
		Expect(updated.Status.CurrentCPU.String()).To(Equal("4"))
		Expect(updated.Status.LastResize.IsZero()).To(BeFalse())
		Expect(updated.Status.Conditions).NotTo(BeEmpty())
	})

	It("should floor the phantom when the workload emits nothing", func() {
		stub.cpu, stub.mem = 0, 0
		Expect(k8sClient.Create(ctx, validShadowWorkload(resourceName))).To(Succeed())

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		updated := &symbiontv1alpha1.ShadowWorkload{}
		Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
		Expect(updated.Status.CurrentCPU.String()).To(Equal("10m"))
		Expect(updated.Status.CurrentMemory.String()).To(Equal("32Mi"))
	})
})
