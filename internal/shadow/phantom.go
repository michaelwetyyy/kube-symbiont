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

// Package shadow holds the pure mechanics of the phantom-pod mechanism:
// naming, pod construction, measurement clamping and drift evaluation.
// Everything here is deterministic and free of Kubernetes API calls so it can
// be unit-tested without a cluster.
package shadow

import (
	"fmt"
	"hash/fnv"
	"math"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	symbiontv1alpha1 "github.com/michaelwetyyy/kube-symbiont/api/v1alpha1"
)

const (
	// PhantomNamePrefix prepends every phantom pod derived from a
	// ShadowWorkload named <cr>: shadow-<cr>.
	PhantomNamePrefix = "shadow-"

	// MaxPodNameLength is the DNS-subdomain limit Kubernetes enforces on pod
	// names.
	MaxPodNameLength = 63

	// PhantomContainerName is the single pause container inside the phantom.
	PhantomContainerName = "phantom"

	// PhantomImage is the pause image: ~1MB resident RAM regardless of the
	// requests it carries. The kernel sees ~1MB; the scheduler sees the full
	// reservation. That asymmetry is the mechanism.
	PhantomImage = "registry.k8s.io/pause:3.9"

	// BallastPriorityClassName must be installed out-of-band (see
	// config/symbiont/priorityclass.yaml). High enough that priority-0
	// workloads cannot preempt the phantom it protects.
	BallastPriorityClassName = "symbiont-ballast"

	// LabelManagedBy / LabelShadowWorkload mark phantoms and tie them back to
	// their ShadowWorkload for observability.
	LabelManagedBy       = "app.kubernetes.io/managed-by"
	LabelShadowWorkload  = "symbiont.tensorhost.com/shadow-workload"
	ManagedByValue       = "kube-symbiont"
	AnnotationDisclaimer = "symbiont.tensorhost.com/note"
	DisclaimerText       = "phantom ballast pod: requests mirror real bare-metal usage; consumes ~1MB itself"
)

// PhantomName derives the phantom pod name for a ShadowWorkload. Names that
// would exceed the 63-char pod-name limit are truncated and disambiguated with
// a short FNV hash of the original name, deterministically.
func PhantomName(crName string) string {
	full := PhantomNamePrefix + crName
	if len(full) <= MaxPodNameLength {
		return full
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(crName))
	suffix := fmt.Sprintf("%06x", h.Sum64())
	keep := MaxPodNameLength - len(PhantomNamePrefix) - len(suffix) - 1
	return PhantomNamePrefix + crName[:keep] + "-" + suffix
}

// PairOf converts a measured CPU-cores / memory-bytes pair into quantities,
// rounding CPU to whole milli-cores and memory to whole bytes to avoid
// sub-resolution flapping between polls.
func PairOf(cpuCores, memoryBytes float64) symbiontv1alpha1.ResourcePair {
	cpu := resource.NewMilliQuantity(int64(math.Round(cpuCores*1000)), resource.DecimalSI)
	mem := resource.NewQuantity(int64(math.Round(memoryBytes)), resource.BinarySI)
	return symbiontv1alpha1.ResourcePair{CPU: *cpu, Memory: *mem}
}

// Clamp constrains desired into [floor, ceiling] per dimension. A workload
// that is off or emits nothing measures as zero and settles on the floor,
// preserving a small standing reservation instead of dropping the phantom to
// zero.
func Clamp(desired, floor, ceiling symbiontv1alpha1.ResourcePair) symbiontv1alpha1.ResourcePair {
	clamped := symbiontv1alpha1.ResourcePair{}
	clamped.CPU = clampQuantity(desired.CPU, floor.CPU, ceiling.CPU)
	clamped.Memory = clampQuantity(desired.Memory, floor.Memory, ceiling.Memory)
	return clamped
}

func clampQuantity(desired, floor, ceiling resource.Quantity) resource.Quantity {
	out := desired.DeepCopy()
	if out.Cmp(floor) < 0 {
		out = floor.DeepCopy()
	}
	if out.Cmp(ceiling) > 0 {
		out = ceiling.DeepCopy()
	}
	return out
}

// DriftExceeds reports whether any dimension drifted further than
// thresholdPercent relative to the phantom's current requests. Shrinks count
// via absolute drift: releasing stale reservations matters as much as chasing
// spikes. A zero-current dimension against any positive desire is maximal
// drift (first fill after create).
func DriftExceeds(current, desired symbiontv1alpha1.ResourcePair, thresholdPercent int32) bool {
	return drift(current.CPU, desired.CPU, thresholdPercent) ||
		drift(current.Memory, desired.Memory, thresholdPercent)
}

func drift(current, desired resource.Quantity, thresholdPercent int32) bool {
	cur := current.AsApproximateFloat64()
	want := desired.AsApproximateFloat64()
	if want < 0 {
		want = 0
	}
	if cur <= 0 {
		return want > 0
	}
	change := math.Abs(want-cur) / cur * 100
	return change > float64(thresholdPercent)
}

// BuildPhantomPod constructs the ballast pod for a ShadowWorkload at the given
// initial resource pair:
//
//   - registry.k8s.io/pause:3.9 (~1MB real footprint)
//   - spec.nodeName hard pin to spec.node (bare metal cannot migrate anyway)
//   - Guaranteed QoS: requests == limits, both dimensions (QoS class is
//     immutable, so every later resize patches both sides together)
//   - resizePolicy NotRequired for cpu + memory (in-place resize, no restart;
//     requires K8s 1.29+ InPlacePodVerticalScaling)
//   - PriorityClass symbiont-ballast so normal workloads cannot preempt it
//   - controller ownerReference so CR deletion GCs the phantom
func BuildPhantomPod(sw *symbiontv1alpha1.ShadowWorkload, scheme *runtime.Scheme, initial symbiontv1alpha1.ResourcePair) (*corev1.Pod, error) {
	requests := corev1.ResourceList{
		corev1.ResourceCPU:    initial.CPU,
		corev1.ResourceMemory: initial.Memory,
	}
	limits := requests.DeepCopy()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      PhantomName(sw.Name),
			Namespace: sw.Namespace,
			Labels: map[string]string{
				LabelManagedBy:      ManagedByValue,
				LabelShadowWorkload: sw.Name,
			},
			Annotations: map[string]string{
				AnnotationDisclaimer: DisclaimerText,
			},
		},
		Spec: corev1.PodSpec{
			NodeName:          sw.Spec.Node,
			PriorityClassName: BallastPriorityClassName,
			RestartPolicy:     corev1.RestartPolicyAlways,
			Containers: []corev1.Container{
				{
					Name:  PhantomContainerName,
					Image: PhantomImage,
					Resources: corev1.ResourceRequirements{
						Requests: requests,
						Limits:   limits,
					},
					ResizePolicy: []corev1.ContainerResizePolicy{
						{ResourceName: corev1.ResourceCPU, RestartPolicy: corev1.NotRequired},
						{ResourceName: corev1.ResourceMemory, RestartPolicy: corev1.NotRequired},
					},
				},
			},
		},
	}
	if err := controllerutil.SetControllerReference(sw, pod, scheme); err != nil {
		return nil, fmt.Errorf("failed to set owner reference on phantom pod: %w", err)
	}
	return pod, nil
}

// CurrentPairOf extracts the phantom's effective requested pair from its first
// container. Missing dimensions read as zero.
func CurrentPairOf(pod *corev1.Pod) symbiontv1alpha1.ResourcePair {
	pair := symbiontv1alpha1.ResourcePair{}
	if len(pod.Spec.Containers) == 0 {
		return pair
	}
	req := pod.Spec.Containers[0].Resources.Requests
	if cpu, ok := req[corev1.ResourceCPU]; ok {
		pair.CPU = cpu
	}
	if mem, ok := req[corev1.ResourceMemory]; ok {
		pair.Memory = mem
	}
	return pair
}
