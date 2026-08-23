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

package shadow

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	symbiontv1alpha1 "github.com/michaelwetyyy/kube-symbiont/api/v1alpha1"
)

const (
	minimumCPU    = "10m"
	minimumMemory = "32Mi"
	sixGiB        = "6Gi"
)

func pair(cpu, mem string) symbiontv1alpha1.ResourcePair {
	return symbiontv1alpha1.ResourcePair{
		CPU:    resource.MustParse(cpu),
		Memory: resource.MustParse(mem),
	}
}

func TestPhantomName(t *testing.T) {
	tests := []struct {
		name      string
		cr        string
		wantExact string
	}{
		{"short name kept verbatim", "lab-docker", "shadow-lab-docker"},
		{"plain cr name", "node1-docker", "shadow-node1-docker"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PhantomName(tt.cr); got != tt.wantExact {
				t.Fatalf("PhantomName(%q) = %q, want %q", tt.cr, got, tt.wantExact)
			}
		})
	}

	long := "a-very-long-shadowworkload-name-that-will-definitely-exceed-the-sixty-three-character-pod-name-limit-set-by-kubernetes"
	got := PhantomName(long)
	if len(got) > MaxPodNameLength {
		t.Fatalf("truncated name %q exceeds %d chars", got, MaxPodNameLength)
	}
	if got == PhantomNamePrefix+long {
		t.Fatal("long name should have been truncated")
	}
	if again := PhantomName(long); again != got {
		t.Fatalf("PhantomName not deterministic: %q vs %q", got, again)
	}
	other := PhantomName(long + "x")
	if other == got {
		t.Fatal("distinct long CR names collided after truncation")
	}
}

func TestClamp(t *testing.T) {
	floor := pair(minimumCPU, minimumMemory)
	ceiling := pair("8", "32Gi")

	tests := []struct {
		name         string
		desired      symbiontv1alpha1.ResourcePair
		wantCPU, mem string
	}{
		{"below floor clamps up", pair("1m", "1Mi"), minimumCPU, minimumMemory},
		{"above ceiling clamps down", pair("100", "64Gi"), "8", "32Gi"},
		{"between passes through", pair("1500m", sixGiB), "1500m", sixGiB},
		{"workload off settles at floor", pair("0", "0"), minimumCPU, minimumMemory},
		{"exactly at bounds unchanged", floor, minimumCPU, minimumMemory},
		{"cpu below mem above", pair("1m", "64Gi"), minimumCPU, "32Gi"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Clamp(tt.desired, floor, ceiling)
			if got.CPU.String() != tt.wantCPU {
				t.Errorf("CPU = %s, want %s", got.CPU.String(), tt.wantCPU)
			}
			if got.Memory.String() != tt.mem {
				t.Errorf("Memory = %s, want %s", got.Memory.String(), tt.mem)
			}
		})
	}
}

func TestDriftExceeds(t *testing.T) {
	tests := []struct {
		name      string
		current   symbiontv1alpha1.ResourcePair
		desired   symbiontv1alpha1.ResourcePair
		threshold int32
		want      bool
	}{
		{"within threshold", pair("1000m", "4Gi"), pair("1050m", "4Gi"), 10, false},
		{"cpu drift over", pair("1000m", "4Gi"), pair("1200m", "4Gi"), 10, true},
		{"mem drift over", pair("1000m", "4Gi"), pair("1000m", "5Gi"), 10, true},
		{"shrink counts too", pair("1000m", "4Gi"), pair("400m", "4Gi"), 10, true},
		{"equal no drift", pair("500m", "2Gi"), pair("500m", "2Gi"), 0, false},
		{"zero current any positive desire", pair("0", "0"), pair("1m", "1Mi"), 10, true},
		{"zero current zero desire idle", pair("0", "0"), pair("0", "0"), 10, false},
		{"threshold zero any change fires", pair("1000m", "4Gi"), pair("1010m", "4Gi"), 0, true},
		{"tiny sub-threshold change", pair("10000m", "40Gi"), pair("10050m", "40Gi"), 10, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DriftExceeds(tt.current, tt.desired, tt.threshold); got != tt.want {
				t.Fatalf("DriftExceeds(%v -> %v @%d%%) = %v, want %v",
					tt.current, tt.desired, tt.threshold, got, tt.want)
			}
		})
	}
}

func TestPairOf(t *testing.T) {
	p := PairOf(1.2345, 6*1024*1024*1024)
	if p.CPU.String() != "1234m" && p.CPU.String() != "1235m" {
		t.Fatalf("CPU rounding unexpected: %s", p.CPU.String())
	}
	if p.Memory.String() != sixGiB {
		t.Fatalf("Memory = %s, want 6Gi", p.Memory.String())
	}
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	sch := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	if err := symbiontv1alpha1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	return sch
}

func swFixture() *symbiontv1alpha1.ShadowWorkload {
	return &symbiontv1alpha1.ShadowWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "lab-docker", Namespace: "kube-symbiont", UID: "uid-1234"},
		Spec: symbiontv1alpha1.ShadowWorkloadSpec{
			Node: "lab",
			Source: symbiontv1alpha1.SourceSpec{
				Type:   symbiontv1alpha1.SourceTypeDocker,
				Docker: &symbiontv1alpha1.DockerSource{Selector: symbiontv1alpha1.SelectorAll},
			},
			Metrics: symbiontv1alpha1.MetricsConfig{
				PrometheusURL: "http://prom:9090",
				Window:        "5m",
			},
			Update: symbiontv1alpha1.UpdatePolicy{
				DeltaThresholdPercent: 10,
				PollInterval:          "30s",
				Floor:                 pair(minimumCPU, minimumMemory),
				Ceiling:               pair("8", "32Gi"),
			},
		},
	}
}

// TestBuildPhantomPod verifies every invariant the design note demands of the
// phantom: pause image, node pin, Guaranteed QoS (requests==limits), NotRequired
// resize policy for cpu AND memory, ballast PriorityClass, controller ownerRef.
func TestBuildPhantomPod(t *testing.T) {
	sw := swFixture()
	initial := pair("1200m", sixGiB)
	pod, err := BuildPhantomPod(sw, testScheme(t), initial)
	if err != nil {
		t.Fatalf("BuildPhantomPod: %v", err)
	}

	if pod.Name != "shadow-lab-docker" || pod.Namespace != "kube-symbiont" {
		t.Errorf("identity = %s/%s", pod.Namespace, pod.Name)
	}
	if pod.Spec.NodeName != "lab" {
		t.Errorf("NodeName = %q, want hard pin to lab", pod.Spec.NodeName)
	}
	if pod.Spec.PriorityClassName != BallastPriorityClassName {
		t.Errorf("PriorityClassName = %q, want %q", pod.Spec.PriorityClassName, BallastPriorityClassName)
	}
	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("containers = %d, want 1", len(pod.Spec.Containers))
	}
	c := pod.Spec.Containers[0]
	if c.Image != PhantomImage {
		t.Errorf("Image = %q, want %q", c.Image, PhantomImage)
	}

	req, lim := c.Resources.Requests, c.Resources.Limits
	for _, dim := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		rq, lq := req[dim], lim[dim]
		if rq.Cmp(lq) != 0 {
			t.Errorf("%s requests (%s) != limits (%s): QoS must be Guaranteed", dim, rq.String(), lq.String())
		}
	}
	cpuReq, memReq := req[corev1.ResourceCPU], req[corev1.ResourceMemory]
	if cpuReq.String() != "1200m" || memReq.String() != sixGiB {
		t.Errorf("initial pair wrong: cpu=%s memory=%s", cpuReq.String(), memReq.String())
	}

	policies := map[corev1.ResourceName]corev1.ResourceResizeRestartPolicy{}
	for _, p := range c.ResizePolicy {
		policies[p.ResourceName] = p.RestartPolicy
	}
	for _, dim := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		if policies[dim] != corev1.NotRequired {
			t.Errorf("%s resize policy = %v, want NotRequired", dim, policies[dim])
		}
	}

	ref := metav1.GetControllerOf(pod)
	if ref == nil || ref.UID != sw.UID || ref.Kind != "ShadowWorkload" ||
		ref.APIVersion != symbiontv1alpha1.GroupVersion.String() {
		t.Errorf("controller ownerRef missing/wrong: %+v", ref)
	}
}

func TestCurrentPairOf(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("250m"),
				corev1.ResourceMemory: resource.MustParse("1Gi"),
			},
		},
	}}}}
	got := CurrentPairOf(pod)
	if got.CPU.String() != "250m" || got.Memory.String() != "1Gi" {
		t.Fatalf("CurrentPairOf = cpu=%s mem=%s", got.CPU.String(), got.Memory.String())
	}
	empty := CurrentPairOf(&corev1.Pod{})
	if !empty.CPU.IsZero() || !empty.Memory.IsZero() {
		t.Fatal("empty pod should yield zero-valued quantities")
	}
}
