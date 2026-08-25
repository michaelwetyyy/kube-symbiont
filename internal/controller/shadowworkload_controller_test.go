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

	zapr "github.com/go-logr/zapr"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
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
	err error
}

type recordingNodeReader struct {
	client.Reader
	nodeGets int
}

type statusPatchFailingClient struct {
	client.Client
}

type statusPatchFailingWriter struct {
	client.SubResourceWriter
}

func (c *statusPatchFailingClient) Status() client.SubResourceWriter {
	return &statusPatchFailingWriter{SubResourceWriter: c.Client.Status()}
}

func (w *statusPatchFailingWriter) Patch(
	context.Context,
	client.Object,
	client.Patch,
	...client.SubResourcePatchOption,
) error {
	return errors.New("injected status patch failure")
}

func (r *recordingNodeReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*corev1.Node); ok {
		r.nodeGets++
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}

func (s *stubQuerier) QueryPair(_ context.Context, _ sources.Queries) (float64, float64, bool, error) {
	return s.cpu, s.mem, true, s.err
}

// drainEvents empties the fake recorder's event channel without blocking and
// returns the drained event strings.
func drainEvents(recorder *record.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-recorder.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

// makeTestNode builds a Node fixture sized like the lab host so resize
// admission has allocatable to check against. ready controls whether a
// NodeReady=True condition is reported (nil = no Ready condition at all).
func makeTestNode(name string, ready *corev1.ConditionStatus) *corev1.Node {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	capacity := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("32"),
		corev1.ResourceMemory: resource.MustParse("64Gi"),
		corev1.ResourcePods:   resource.MustParse("110"),
	}
	node.Status.Capacity = capacity
	node.Status.Allocatable = capacity.DeepCopy()
	if ready != nil {
		node.Status.Conditions = []corev1.NodeCondition{{
			Type:               corev1.NodeReady,
			Status:             *ready,
			LastTransitionTime: metav1.Now(),
		}}
	}
	return node
}

const (
	testNodeName        = "lab"
	terminatingNodeName = "doomed"
)

var readyTrue = corev1.ConditionTrue

func filterWarnings(events []string) []string {
	var out []string
	for _, e := range events {
		if len(e) >= 7 && e[:7] == "Warning" {
			out = append(out, e)
		}
	}
	return out
}

func validShadowWorkload(name string) *symbiontv1alpha1.ShadowWorkload {
	return &symbiontv1alpha1.ShadowWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: symbiontv1alpha1.ShadowWorkloadSpec{
			Node: testNodeName,
			Source: symbiontv1alpha1.SourceSpec{
				Type:   symbiontv1alpha1.SourceTypeDocker,
				Docker: &symbiontv1alpha1.DockerSource{Selector: symbiontv1alpha1.SelectorAll, CadvisorInstance: "192.0.2.10:4194"},
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
		fakeEvents *record.FakeRecorder
		nodeReader *recordingNodeReader
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
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: testNodeName}}
		// In-place resize admission validates the delta against the node's
		// allocatable; a bare Node fixture reports zero and would reject the
		// resize. Size it like the real lab host (64GB RAM). Real kubelets
		// also report NodeReady=True, and the reconciler requires it before
		// creating or resizing any phantom.
		capacity := corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("32"),
			corev1.ResourceMemory: resource.MustParse("64Gi"),
			corev1.ResourcePods:   resource.MustParse("110"),
		}
		node.Status.Capacity = capacity
		node.Status.Allocatable = capacity.DeepCopy()
		node.Status.Conditions = []corev1.NodeCondition{{
			Type:               corev1.NodeReady,
			Status:             corev1.ConditionTrue,
			LastTransitionTime: metav1.Now(),
		}}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())

		stub = &stubQuerier{cpu: 2, mem: 6 * 1024 * 1024 * 1024}
		fakeEvents = record.NewFakeRecorder(64)
		nodeReader = &recordingNodeReader{Reader: k8sClient}
		reconciler = &ShadowWorkloadReconciler{
			Client:    k8sClient,
			Scheme:    k8sClient.Scheme(),
			Recorder:  fakeEvents,
			APIReader: nodeReader,
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
		deleteAndWait(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: testNodeName}})
		deleteAndWait(&schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: shadow.BallastPriorityClassName}})
	})

	It("should reject a source spec violating the one-of constraint", func() {
		bad := validShadowWorkload("bad-oneof")
		bad.Spec.Source.Docker = nil // type=docker with no docker block
		err := k8sClient.Create(ctx, bad)
		Expect(err).To(HaveOccurred())
		Expect(apierrors.IsInvalid(err)).To(BeTrue())
	})

	DescribeTable("should reject unsafe update bounds at admission",
		func(mutate func(*symbiontv1alpha1.ShadowWorkload)) {
			bad := validShadowWorkload("bad-update-bounds")
			mutate(bad)
			err := k8sClient.Create(ctx, bad)
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsInvalid(err)).To(BeTrue())
		},
		Entry("negative CPU floor", func(sw *symbiontv1alpha1.ShadowWorkload) {
			sw.Spec.Update.Floor.CPU = resource.MustParse("-1m")
		}),
		Entry("negative memory ceiling", func(sw *symbiontv1alpha1.ShadowWorkload) {
			sw.Spec.Update.Ceiling.Memory = resource.MustParse("-1Mi")
		}),
		Entry("CPU floor above ceiling", func(sw *symbiontv1alpha1.ShadowWorkload) {
			sw.Spec.Update.Floor.CPU = resource.MustParse("9")
		}),
		Entry("memory floor above ceiling", func(sw *symbiontv1alpha1.ShadowWorkload) {
			sw.Spec.Update.Floor.Memory = resource.MustParse("33Gi")
		}),
	)

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
		Expect(pod.Spec.NodeName).To(Equal(testNodeName))
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
		Expect(nodeReader.nodeGets).To(BeNumerically(">=", 2), "node eligibility must use the dedicated API reader")
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

	It("should preserve accepted requests and status when a resize is rejected", func() {
		Expect(k8sClient.Create(ctx, validShadowWorkload(resourceName))).To(Succeed())
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		phantomKey := types.NamespacedName{Name: "shadow-" + resourceName, Namespace: key.Namespace}
		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, phantomKey, pod)).To(Succeed())
		Expect(pod.Spec.Containers[0].Resources.Requests.Memory().String()).To(Equal("6Gi"))

		By("requesting more memory than the node can admit")
		current := &symbiontv1alpha1.ShadowWorkload{}
		Expect(k8sClient.Get(ctx, key, current)).To(Succeed())
		current.Spec.Update.Ceiling.Memory = resource.MustParse("128Gi")
		Expect(k8sClient.Update(ctx, current)).To(Succeed())
		stub.mem = 128 * 1024 * 1024 * 1024

		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		By("keeping both the Pod and ShadowWorkload status at last accepted truth")
		Expect(k8sClient.Get(ctx, phantomKey, pod)).To(Succeed())
		Expect(pod.Spec.Containers[0].Resources.Requests.Memory().String()).To(Equal("6Gi"))
		Expect(k8sClient.Get(ctx, key, current)).To(Succeed())
		Expect(current.Status.CurrentMemory.String()).To(Equal("6Gi"))
		degraded := meta.FindStatusCondition(current.Status.Conditions, "Degraded")
		Expect(degraded).NotTo(BeNil())
		Expect(degraded.Reason).To(Equal("ResizeRejected"))
	})

	It("should retain last truth during a metrics outage and recover", func() {
		Expect(k8sClient.Create(ctx, validShadowWorkload(resourceName))).To(Succeed())
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		before := &symbiontv1alpha1.ShadowWorkload{}
		Expect(k8sClient.Get(ctx, key, before)).To(Succeed())
		lastResize := before.Status.LastResize
		stub.err = errors.New("test metrics outage")
		stub.cpu, stub.mem = 4, 12*1024*1024*1024

		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		outage := &symbiontv1alpha1.ShadowWorkload{}
		Expect(k8sClient.Get(ctx, key, outage)).To(Succeed())
		Expect(outage.Status.CurrentCPU.String()).To(Equal("2"))
		Expect(outage.Status.CurrentMemory.String()).To(Equal("6Gi"))
		Expect(outage.Status.LastResize.Equal(&lastResize)).To(BeTrue())
		Expect(meta.FindStatusCondition(outage.Status.Conditions, "Degraded").Reason).To(Equal("PrometheusUnavailable"))

		stub.err = nil
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		recovered := &symbiontv1alpha1.ShadowWorkload{}
		Expect(k8sClient.Get(ctx, key, recovered)).To(Succeed())
		Expect(recovered.Status.CurrentCPU.String()).To(Equal("4"))
		Expect(meta.IsStatusConditionFalse(recovered.Status.Conditions, "Degraded")).To(BeTrue())
	})

	It("should return status patch failures so controller-runtime retries", func() {
		Expect(k8sClient.Create(ctx, validShadowWorkload(resourceName))).To(Succeed())
		failing := *reconciler
		failing.Client = &statusPatchFailingClient{Client: k8sClient}

		result, err := failing.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).To(MatchError(ContainSubstring("patch ShadowWorkload status: injected status patch failure")))
		Expect(result).To(Equal(reconcile.Result{}))
	})

	It("should retain last truth when the ballast PriorityClass disappears", func() {
		Expect(k8sClient.Create(ctx, validShadowWorkload(resourceName))).To(Succeed())
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		phantomKey := types.NamespacedName{Name: "shadow-" + resourceName, Namespace: key.Namespace}
		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, phantomKey, pod)).To(Succeed())
		uid := pod.UID
		requests := pod.Spec.Containers[0].Resources.Requests.DeepCopy()

		deleteAndWait(&schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: shadow.BallastPriorityClassName}})
		stub.cpu, stub.mem = 4, 12*1024*1024*1024
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, phantomKey, pod)).To(Succeed())
		Expect(pod.UID).To(Equal(uid))
		Expect(pod.Spec.Containers[0].Resources.Requests).To(Equal(requests))
		current := &symbiontv1alpha1.ShadowWorkload{}
		Expect(k8sClient.Get(ctx, key, current)).To(Succeed())
		Expect(current.Status.CurrentCPU.String()).To(Equal("2"))
		Expect(current.Status.CurrentMemory.String()).To(Equal("6Gi"))
		degraded := meta.FindStatusCondition(current.Status.Conditions, "Degraded")
		Expect(degraded).NotTo(BeNil())
		Expect(degraded.Reason).To(Equal("PriorityClassMissing"))
	})

	It("should adopt the existing phantom after a controller restart", func() {
		Expect(k8sClient.Create(ctx, validShadowWorkload(resourceName))).To(Succeed())
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		restarted := &ShadowWorkloadReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Recorder: record.NewFakeRecorder(16),
			QuerierFor: func(string) (MetricsQuerier, error) {
				return &stubQuerier{cpu: 2, mem: 6 * 1024 * 1024 * 1024}, nil
			},
		}
		_, err = restarted.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		podList := &corev1.PodList{}
		Expect(k8sClient.List(ctx, podList, client.InNamespace(key.Namespace),
			client.MatchingLabels{shadow.LabelShadowWorkload: resourceName})).To(Succeed())
		Expect(podList.Items).To(HaveLen(1))
		current := &symbiontv1alpha1.ShadowWorkload{}
		Expect(k8sClient.Get(ctx, key, current)).To(Succeed())
		Expect(meta.IsStatusConditionTrue(current.Status.Conditions, "Ready")).To(BeTrue())
	})

	It("should not create a phantom when the target node does not exist", func() {
		By("creating a ShadowWorkload pinned to a node that was never registered")
		sw := validShadowWorkload("ghost-node-sw")
		sw.Spec.Node = "ghost"
		Expect(k8sClient.Create(ctx, sw)).To(Succeed())
		defer deleteAndWait(&symbiontv1alpha1.ShadowWorkload{
			ObjectMeta: metav1.ObjectMeta{Name: "ghost-node-sw", Namespace: key.Namespace},
		})
		swKey := types.NamespacedName{Name: "ghost-node-sw", Namespace: key.Namespace}
		phantomKey := types.NamespacedName{Name: "shadow-ghost-node-sw", Namespace: key.Namespace}

		By("reconciling twice on the poll clock")
		for range 2 {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: swKey})
			Expect(err).NotTo(HaveOccurred())
		}

		By("asserting no phantom was stranded Pending")
		pod := &corev1.Pod{}
		notReadyErr := k8sClient.Get(ctx, phantomKey, pod)
		Expect(apierrors.IsNotFound(notReadyErr)).To(BeTrue(), "no phantom may exist while the node is not Ready")

		updated := &symbiontv1alpha1.ShadowWorkload{}
		Expect(k8sClient.Get(ctx, swKey, updated)).To(Succeed())
		Expect(updated.Status.PhantomPod).To(BeEmpty())
		Expect(meta.IsStatusConditionTrue(updated.Status.Conditions, "Degraded")).To(BeTrue())
		degraded := meta.FindStatusCondition(updated.Status.Conditions, "Degraded")
		Expect(degraded.Reason).To(Equal("NodeMissing"))
		Expect(meta.IsStatusConditionFalse(updated.Status.Conditions, "Ready")).To(BeTrue())

		By("asserting the warning event fired on transition only, not every poll")
		events := drainEvents(fakeEvents)
		warnings := filterWarnings(events)
		Expect(warnings).To(HaveLen(1), "steady-poll reconciles must not spam warning events: %v", warnings)
	})

	It("should defer phantom creation until the node reports Ready", func() {
		By("registering a node that has never reported Ready")
		node := makeTestNode("late", nil)
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		defer deleteAndWait(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "late"}})

		sw := validShadowWorkload("late-node-sw")
		sw.Spec.Node = "late"
		Expect(k8sClient.Create(ctx, sw)).To(Succeed())
		defer deleteAndWait(&symbiontv1alpha1.ShadowWorkload{
			ObjectMeta: metav1.ObjectMeta{Name: "late-node-sw", Namespace: key.Namespace},
		})
		swKey := types.NamespacedName{Name: "late-node-sw", Namespace: key.Namespace}
		phantomKey := types.NamespacedName{Name: "shadow-late-node-sw", Namespace: key.Namespace}

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: swKey})
		Expect(err).NotTo(HaveOccurred())

		pod := &corev1.Pod{}
		notReadyErr := k8sClient.Get(ctx, phantomKey, pod)
		Expect(apierrors.IsNotFound(notReadyErr)).To(BeTrue(), "no phantom may exist while the node is not Ready")
		updated := &symbiontv1alpha1.ShadowWorkload{}
		Expect(k8sClient.Get(ctx, swKey, updated)).To(Succeed())
		degraded := meta.FindStatusCondition(updated.Status.Conditions, "Degraded")
		Expect(degraded).NotTo(BeNil())
		Expect(degraded.Reason).To(Equal("NodeNotReady"))

		By("reporting the node Ready and reconciling once more")
		node.Status.Conditions = []corev1.NodeCondition{{
			Type: corev1.NodeReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.Now(),
		}}
		Expect(k8sClient.Status().Update(ctx, node)).To(Succeed())

		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: swKey})
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() error {
			return k8sClient.Get(ctx, phantomKey, pod)
		}).Should(Succeed(), "phantom should appear on the first eligible reconcile")
		Expect(pod.Spec.NodeName).To(Equal("late"))
		Expect(k8sClient.Get(ctx, swKey, updated)).To(Succeed())
		Expect(updated.Status.PhantomPod).To(Equal(phantomKey.Name))
		Expect(meta.IsStatusConditionTrue(updated.Status.Conditions, "Ready")).To(BeTrue())
	})

	It("should not create a phantom while the node is terminating", func() {
		By("deleting a Ready node held open by a test finalizer")
		node := makeTestNode(terminatingNodeName, &readyTrue)
		node.Finalizers = []string{"symbiont.tensorhost.com/test-hold"}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		defer func() {
			held := &corev1.Node{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: terminatingNodeName}, held); err == nil {
				held.Finalizers = nil
				_ = k8sClient.Update(ctx, held)
			}
			deleteAndWait(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: terminatingNodeName}})
		}()
		Expect(k8sClient.Delete(ctx, node)).To(Succeed())

		held := &corev1.Node{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: terminatingNodeName}, held)).To(Succeed())
		Expect(held.DeletionTimestamp).NotTo(BeNil())

		sw := validShadowWorkload("doomed-node-sw")
		sw.Spec.Node = terminatingNodeName
		Expect(k8sClient.Create(ctx, sw)).To(Succeed())
		defer deleteAndWait(&symbiontv1alpha1.ShadowWorkload{
			ObjectMeta: metav1.ObjectMeta{Name: "doomed-node-sw", Namespace: key.Namespace},
		})
		swKey := types.NamespacedName{Name: "doomed-node-sw", Namespace: key.Namespace}

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: swKey})
		Expect(err).NotTo(HaveOccurred())

		notFoundErr := k8sClient.Get(ctx,
			types.NamespacedName{Name: "shadow-doomed-node-sw", Namespace: key.Namespace}, &corev1.Pod{})
		Expect(apierrors.IsNotFound(notFoundErr)).To(BeTrue(), "no phantom may exist while the node is terminating")
		updated := &symbiontv1alpha1.ShadowWorkload{}
		Expect(k8sClient.Get(ctx, swKey, updated)).To(Succeed())
		degraded := meta.FindStatusCondition(updated.Status.Conditions, "Degraded")
		Expect(degraded).NotTo(BeNil())
		Expect(degraded.Reason).To(Equal("NodeTerminating"))
	})
})

// loggingCtx carries the observer-backed logger for the hygiene specs.
var loggingCtx context.Context

var _ = Describe("Steady-state logging hygiene", func() {
	const resourceName = "log-shadow"

	ctx := context.Background()
	key := types.NamespacedName{Name: resourceName, Namespace: "default"}

	cleanup := func(obj client.Object) {
		_ = k8sClient.Delete(ctx, obj, client.GracePeriodSeconds(0))
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), obj))
		}).Should(BeTrue())
	}

	var (
		stub     *stubQuerier
		observed *observer.ObservedLogs
	)

	BeforeEach(func() {
		pc := &schedulingv1.PriorityClass{
			ObjectMeta: metav1.ObjectMeta{Name: shadow.BallastPriorityClassName},
			Value:      1000,
		}
		Expect(k8sClient.Create(ctx, pc)).To(Succeed())
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: testNodeName}}
		capacity := corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("32"),
			corev1.ResourceMemory: resource.MustParse("64Gi"),
			corev1.ResourcePods:   resource.MustParse("110"),
		}
		node.Status.Capacity = capacity
		node.Status.Allocatable = capacity.DeepCopy()
		node.Status.Conditions = []corev1.NodeCondition{{
			Type:               corev1.NodeReady,
			Status:             corev1.ConditionTrue,
			LastTransitionTime: metav1.Now(),
		}}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		stub = &stubQuerier{cpu: 2, mem: 6 * 1024 * 1024 * 1024}
		core, logs := observer.New(zapcore.InfoLevel)
		observed = logs
		logger := zapr.NewLogger(zap.New(core))
		loggingCtx = logf.IntoContext(ctx, logger.WithName("logging-hygiene"))
	})

	AfterEach(func() {
		sw := &symbiontv1alpha1.ShadowWorkload{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}}
		cleanup(sw)
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "shadow-" + key.Name, Namespace: key.Namespace}}
		cleanup(pod)
		cleanup(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: testNodeName}})
		cleanup(&schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: shadow.BallastPriorityClassName}})
	})

	reconcileUnderTest := func() error {
		r := &ShadowWorkloadReconciler{
			Client:    k8sClient,
			Scheme:    k8sClient.Scheme(),
			Recorder:  record.NewFakeRecorder(64),
			APIReader: k8sClient,
			QuerierFor: func(string) (MetricsQuerier, error) {
				return stub, nil
			},
		}
		_, err := r.Reconcile(loggingCtx, reconcile.Request{NamespacedName: key})
		return err
	}

	infoCount := func(msg string) int {
		n := 0
		for _, e := range observed.All() {
			if e.Level == zapcore.InfoLevel && e.Message == msg {
				n++
			}
		}
		return n
	}

	It("emits no per-poll Info lines for unchanged steady state", func() {
		Expect(k8sClient.Create(ctx, validShadowWorkload(resourceName))).To(Succeed())
		Expect(reconcileUnderTest()).To(Succeed())
		Expect(reconcileUnderTest()).To(Succeed())
		Expect(infoCount("Measured bare-metal footprint")).To(Equal(0))
		Expect(infoCount("Reconciled")).To(Equal(0))
	})

	It("still reports accepted resizes at Info level", func() {
		Expect(k8sClient.Create(ctx, validShadowWorkload(resourceName))).To(Succeed())
		Expect(reconcileUnderTest()).To(Succeed())
		stub.cpu = 4 // drift beyond deltaThresholdPercent forces a resize
		Expect(reconcileUnderTest()).To(Succeed())
		Expect(infoCount("Resized phantom Pod")).To(Equal(1))
	})

	It("keeps backend URLs out of failure log key-values", func() {
		privateURL := "http://prometheus.monitoring.svc:9090"
		spec := validShadowWorkload(resourceName)
		spec.Spec.Metrics.PrometheusURL = privateURL
		Expect(k8sClient.Create(ctx, spec)).To(Succeed())

		r := &ShadowWorkloadReconciler{
			Client:    k8sClient,
			Scheme:    k8sClient.Scheme(),
			Recorder:  record.NewFakeRecorder(64),
			APIReader: k8sClient,
			QuerierFor: func(string) (MetricsQuerier, error) {
				return failingQuerier{}, nil
			},
		}
		_, err := r.Reconcile(loggingCtx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var found *observer.LoggedEntry
		for i := range observed.All() {
			e := observed.All()[i]
			if e.Level == zapcore.ErrorLevel && e.Message == "Failed to query metrics backend" {
				found = &observed.All()[i]
			}
		}
		Expect(found).NotTo(BeNil())
		for _, kv := range found.Context {
			Expect(kv.Key).NotTo(Equal("prometheusURL"))
			Expect(fmt.Sprintf("%v", kv.Interface)).NotTo(ContainSubstring(privateURL))
		}
	})
})

// failingQuerier always fails so the Prometheus-unavailable path runs.
type failingQuerier struct{}

func (failingQuerier) QueryPair(_ context.Context, _ sources.Queries) (float64, float64, bool, error) {
	return 0, 0, false, errors.New("prometheus query failed: connection refused")
}
