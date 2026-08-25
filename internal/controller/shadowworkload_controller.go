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

	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	symbiontv1alpha1 "github.com/michaelwetyyy/kube-symbiont/api/v1alpha1"
	"github.com/michaelwetyyy/kube-symbiont/internal/metrics"
	"github.com/michaelwetyyy/kube-symbiont/internal/shadow"
	"github.com/michaelwetyyy/kube-symbiont/internal/sources"
)

const (
	conditionReady    = "Ready"
	conditionDegraded = "Degraded"

	reasonMetricsSynced         = "MetricsSynced"
	reasonWithinThreshold       = "WithinThreshold"
	reasonPhantomCreated        = "PhantomCreated"
	reasonPhantomRecreated      = "PhantomRecreated"
	reasonPhantomAdopted        = "PhantomAdopted"
	reasonResized               = "Resized"
	reasonNodeMissing           = "NodeMissing"
	reasonNodeNotReady          = "NodeNotReady"
	reasonNodeTerminating       = "NodeTerminating"
	reasonSourceInvalid         = "SourceInvalid"
	reasonPrometheusUnavailable = "PrometheusUnavailable"
	reasonPriorityClassMissing  = "PriorityClassMissing"
	reasonResizeRejected        = "ResizeRejected"
	reasonPhantomConflict       = "PhantomConflict"
	reasonAsExpected            = "AsExpected"

	defaultPollInterval = 30 * time.Second
	queryTimeout        = 10 * time.Second
)

// MetricsQuerier issues resolved query pairs against a metrics backend.
type MetricsQuerier interface {
	QueryPair(ctx context.Context, q sources.Queries) (cpuCores, memoryBytes float64, found bool, err error)
}

// ShadowWorkloadReconciler reconciles a ShadowWorkload object: it measures the
// declared bare-metal workload and keeps a phantom ballast pod's requests
// mirroring that footprint on the scheduler ledger.
type ShadowWorkloadReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	// APIReader performs get-only cluster-scoped lookups without making the
	// shared cache require list/watch permissions.
	APIReader client.Reader

	// QuerierFor builds a metrics querier for a Prometheus URL. Defaults to
	// sources.NewClient; overridable for tests.
	QuerierFor func(rawURL string) (MetricsQuerier, error)

	// Metrics instruments the measure → clamp → resize loop on the manager's
	// Prometheus registry. Nil-safe: every call is a no-op until
	// SetupWithManager installs a recorder.
	Metrics *metrics.Recorder
}

// +kubebuilder:rbac:groups=symbiont.tensorhost.com,resources=shadowworkloads,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=symbiont.tensorhost.com,resources=shadowworkloads/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=pods/resize,verbs=patch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get
// +kubebuilder:rbac:groups=scheduling.k8s.io,resources=priorityclasses,verbs=get
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile runs the measure → clamp → resize loop:
//
//  1. gate on target-node eligibility (nodes get): an absent, terminating
//     or not-Ready node defers all phantom management — a nodeName-pinned
//     pod created anyway could never run its containers,
//  2. resolve spec.source into a PromQL pair and measure it,
//  3. clamp the measurement to [floor, ceiling],
//  4. ensure the phantom pod exists (create if missing, recreate if its node
//     pin no longer matches), pinned via spec.nodeName with Guaranteed QoS,
//  5. when relative drift exceeds deltaThresholdPercent, PATCH the pod resize
//     subresource — requests AND limits together, preserving Guaranteed QoS,
//  6. update status and requeue after pollInterval (metric-driven loop).
func (r *ShadowWorkloadReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var sw symbiontv1alpha1.ShadowWorkload
	if err := r.Get(ctx, req.NamespacedName, &sw); err != nil {
		if apierrors.IsNotFound(err) {
			// The CR is gone: drop every series it owned so the registry
			// never accumulates stale namespaces.
			r.Metrics.RemoveShadowWorkload(req.Namespace, req.Name)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	log := logf.FromContext(ctx).WithValues("shadowWorkload", req.NamespacedName, "node", sw.Spec.Node)

	statusBase := sw.DeepCopy()
	poll := pollInterval(&sw)

	degradedReason := ""
	degradedMsg := ""
	readyReason := reasonWithinThreshold
	phantomObserved := false

	// Phantom identity/effective requests observed by this pass; only read
	// when phantomObserved is true (the eligible-management path).
	var (
		phantomPodName string
		observedPod    *corev1.Pod
	)

	setReady := func(reason string) { readyReason = reason }
	setDegraded := func(reason, format string, args ...any) {
		if degradedReason == "" { // first failure wins
			degradedReason = reason
			degradedMsg = fmt.Sprintf(format, args...)
		}
	}

	// Node gate: the phantom is pinned via nodeName and bypasses the
	// scheduler entirely, so creating one against an absent, terminating or
	// not-Ready node would strand it Pending forever. Until the node is
	// live again, no phantom is created or resized; the loop retries on the
	// poll clock and the phantom appears on the first eligible reconcile.
	var node corev1.Node
	apiReader := r.APIReader
	if apiReader == nil {
		// Tests and direct library users may not provide a separate reader. The
		// production manager always injects its uncached API reader so get-only
		// cluster-scoped checks never make the cache require list/watch privileges.
		apiReader = r.Client
	}
	nodeErr := apiReader.Get(ctx, client.ObjectKey{Name: sw.Spec.Node}, &node)
	nodeEligible := false
	switch {
	case nodeErr == nil:
		ok, why := shadow.NodeEligible(&node)
		nodeEligible = ok
		if !ok {
			reason := nodeConditionReason(why)
			msg := shadow.NodeEligibilityError(sw.Spec.Node, why)
			setDegraded(reason, "%s", msg)
			r.warnOnDegradedTransition(&sw, reason, msg)
		}
	case apierrors.IsNotFound(nodeErr):
		msg := shadow.NodeEligibilityError(sw.Spec.Node, shadow.NodeReasonMissing)
		setDegraded(reasonNodeMissing, "%s", msg)
		r.warnOnDegradedTransition(&sw, reasonNodeMissing, msg)
	default:
		return ctrl.Result{}, fmt.Errorf("get target node %q: %w", sw.Spec.Node, nodeErr)
	}

	prerequisitesReady := nodeEligible
	if nodeEligible {
		var priorityClass schedulingv1.PriorityClass
		priorityErr := apiReader.Get(ctx, client.ObjectKey{Name: shadow.BallastPriorityClassName}, &priorityClass)
		switch {
		case priorityErr == nil:
		case apierrors.IsNotFound(priorityErr):
			prerequisitesReady = false
			msg := fmt.Sprintf("required PriorityClass %q is missing; keeping the last accepted reservation", shadow.BallastPriorityClassName)
			setDegraded(reasonPriorityClassMissing, "%s", msg)
			r.warnOnDegradedTransition(&sw, reasonPriorityClassMissing, msg)
		default:
			return ctrl.Result{}, fmt.Errorf("get required PriorityClass %q: %w", shadow.BallastPriorityClassName, priorityErr)
		}
	}

	measuredCPU, measuredMem := 0.0, 0.0
	if prerequisitesReady {
		r.measure(ctx, &sw, setDegraded, &measuredCPU, &measuredMem)
	}

	floor := sw.Spec.Update.Floor
	ceiling := sw.Spec.Update.Ceiling
	desired := shadow.Clamp(shadow.PairOf(measuredCPU, measuredMem), floor, ceiling)

	if prerequisitesReady {
		// Ensure the phantom exists and matches the current node pin. Skipped
		// entirely while the node or prerequisite is ineligible: no create, no
		// resize, and any pre-existing phantom keeps reporting its last requests
		// as last truth.
		name := shadow.PhantomName(sw.Name)
		key := client.ObjectKey{Namespace: sw.Namespace, Name: name}
		var pod corev1.Pod
		err := r.Get(ctx, key, &pod)
		switch {
		case apierrors.IsNotFound(err):
			fresh, berr := shadow.BuildPhantomPod(&sw, r.Scheme, desired)
			if berr != nil {
				return ctrl.Result{}, fmt.Errorf("build phantom pod: %w", berr)
			}
			if cerr := r.Create(ctx, fresh); cerr != nil {
				if !apierrors.IsAlreadyExists(cerr) {
					return ctrl.Result{}, fmt.Errorf("create phantom pod %s: %w", name, cerr)
				}
				// The scoped Pod informer only tracks pods carrying the
				// managed-by label, so a same-name pod invisible to the cache
				// exists at the API server. Verify through the uncached reader
				// before deciding: foreign pods are reported as conflicts, and
				// our own label-stripped phantoms are relabelled and adopted
				// instead of crash-looping on Create.
				var live corev1.Pod
				gerr := r.APIReader.Get(ctx, key, &live)
				if gerr != nil && !apierrors.IsNotFound(gerr) {
					return ctrl.Result{}, fmt.Errorf("read existing phantom pod %s: %w", name, gerr)
				}
				if apierrors.IsNotFound(gerr) {
					// The conflicting object vanished between Create and Get;
					// retry cleanly on the next poll.
					return ctrl.Result{RequeueAfter: poll}, nil
				}
				if ownedBySw(&live, &sw) {
					if live.Labels == nil {
						live.Labels = map[string]string{}
					}
					live.Labels[shadow.LabelManagedBy] = shadow.ManagedByValue
					live.Labels[shadow.LabelShadowWorkload] = sw.Name
					if uerr := r.Update(ctx, &live); uerr != nil {
						if apierrors.IsConflict(uerr) {
							return ctrl.Result{RequeueAfter: poll}, nil
						}
						return ctrl.Result{}, fmt.Errorf("relabel phantom pod %s: %w", name, uerr)
					}
					log.Info("Relabelled managed phantom Pod", "pod", name)
					r.Recorder.Eventf(&sw, corev1.EventTypeNormal, reasonPhantomAdopted,
						"Relabelled phantom Pod %s so the scoped cache tracks it again", name)
					pod = live
					phantomObserved = true
					setReady(reasonPhantomAdopted)
				} else {
					msg := fmt.Sprintf("pod %s exists without the managed-by label or controller owner reference; refusing to manage it", name)
					setDegraded(reasonPhantomConflict, "%s", msg)
					r.warnOnDegradedTransition(&sw, reasonPhantomConflict, msg)
				}
				break
			}
			log.Info("Created phantom Pod", "pod", name, "node", sw.Spec.Node,
				"cpu", desired.CPU.String(), "memory", desired.Memory.String())
			r.Recorder.Eventf(&sw, corev1.EventTypeNormal, reasonPhantomCreated,
				"Created phantom Pod %s on node %q (cpu=%s memory=%s)", name, sw.Spec.Node, desired.CPU.String(), desired.Memory.String())
			r.Metrics.ReservationUpdated(sw.Namespace, sw.Name)
			pod = *fresh
			phantomObserved = true
			setReady(reasonPhantomCreated)
		case err != nil:
			return ctrl.Result{}, fmt.Errorf("get phantom pod %s: %w", name, err)
		case pod.Spec.NodeName != sw.Spec.Node:
			// Target node changed: the pin must follow. Delete and recreate next pass.
			if derr := r.Delete(ctx, &pod); derr != nil && !apierrors.IsNotFound(derr) {
				return ctrl.Result{}, fmt.Errorf("delete stale phantom pod %s: %w", name, derr)
			}
			log.Info("Deleted phantom Pod for node re-pin", "pod", name, "oldNode", pod.Spec.NodeName, "newNode", sw.Spec.Node)
			r.Recorder.Eventf(&sw, corev1.EventTypeNormal, reasonPhantomRecreated,
				"Deleted phantom Pod %s; re-pinning from node %q to %q", name, pod.Spec.NodeName, sw.Spec.Node)
			setReady(reasonPhantomRecreated)
		case !ownedBySw(&pod, &sw):
			setDegraded(reasonPhantomConflict,
				"pod %s exists but is not owned by this ShadowWorkload; refusing to manage it", name)
			r.Recorder.Eventf(&sw, corev1.EventTypeWarning, reasonPhantomConflict,
				"Pod %s already exists without controller ownerRef to this ShadowWorkload", name)
		default:
			phantomObserved = true
			current := shadow.CurrentPairOf(&pod)
			if degradedReason == "" && shadow.DriftExceeds(current, desired, sw.Spec.Update.DeltaThresholdPercent) {
				before := current
				if rerr := r.resizePhantom(ctx, &pod, desired); rerr != nil {
					r.Metrics.MeasurementFailed(sw.Namespace, sw.Name, metrics.FailureResizeRejected)
					setDegraded(reasonResizeRejected, "in-place resize of %s rejected: %v", name, rerr)
					log.Error(rerr, "In-place resize rejected", "pod", name)
					r.Recorder.Eventf(&sw, corev1.EventTypeWarning, reasonResizeRejected,
						"In-place resize of Pod %s rejected (%s -> cpu=%s memory=%s); keeping previous requests",
						name, rerr, desired.CPU.String(), desired.Memory.String())
				} else {
					log.Info("Resized phantom Pod", "pod", name,
						"cpu", before.CPU.String()+"->"+desired.CPU.String(),
						"memory", before.Memory.String()+"->"+desired.Memory.String())
					r.Recorder.Eventf(&sw, corev1.EventTypeNormal, reasonResized,
						"Resized Pod %s: cpu %s -> %s, memory %s -> %s",
						name, before.CPU.String(), desired.CPU.String(), before.Memory.String(), desired.Memory.String())
					now := metav1.Now()
					sw.Status.LastResize = now
					sw.Status.CurrentCPU = desired.CPU
					sw.Status.CurrentMemory = desired.Memory
					r.Metrics.ReservationUpdated(sw.Namespace, sw.Name)
					setReady(reasonResized)
				}
			} else if degradedReason == "" {
				setReady(reasonWithinThreshold)
			}
		}
		phantomPodName = name
		observedPod = &pod
	}

	// Status currents are only refreshed when this pass actually observed or
	// managed the phantom; during outages (node missing, Prometheus down,
	// resize rejected, foreign-pod conflict) they keep reporting last truth.
	// LastResize is stamped only by an accepted resize.
	if phantomObserved {
		sw.Status.PhantomPod = phantomPodName
		effective := shadow.CurrentPairOf(observedPod)
		sw.Status.CurrentCPU = effective.CPU
		sw.Status.CurrentMemory = effective.Memory
		r.Metrics.SetReserved(sw.Namespace, sw.Name,
			effective.CPU.AsApproximateFloat64(), effective.Memory.AsApproximateFloat64())
	}

	if degradedReason != "" {
		meta.SetStatusCondition(&sw.Status.Conditions, metav1.Condition{
			Type: conditionReady, Status: metav1.ConditionFalse, Reason: degradedReason,
			Message: degradedMsg,
		})
		meta.SetStatusCondition(&sw.Status.Conditions, metav1.Condition{
			Type: conditionDegraded, Status: metav1.ConditionTrue, Reason: degradedReason,
			Message: degradedMsg,
		})
	} else {
		meta.SetStatusCondition(&sw.Status.Conditions, metav1.Condition{
			Type: conditionReady, Status: metav1.ConditionTrue, Reason: readyReason,
			Message: fmt.Sprintf("tracking node %q at cpu=%s memory=%s",
				sw.Spec.Node, sw.Status.CurrentCPU.String(), sw.Status.CurrentMemory.String()),
		})
		meta.SetStatusCondition(&sw.Status.Conditions, metav1.Condition{
			Type: conditionDegraded, Status: metav1.ConditionFalse, Reason: reasonAsExpected,
			Message: "phantom tracking nominal",
		})
	}

	if perr := r.Status().Patch(ctx, &sw, client.MergeFrom(statusBase)); perr != nil {
		return ctrl.Result{}, fmt.Errorf("patch ShadowWorkload status: %w", perr)
	}

	log.Info("Reconciled", "ready", degradedReason == "", "requeueAfter", poll.String())
	return ctrl.Result{RequeueAfter: poll}, nil
}

// measure resolves the source to queries and fills in the measured pair.
// Prometheus problems are soft failures: the phantom keeps its last requests
// and the loop retries after pollInterval.
func (r *ShadowWorkloadReconciler) measure(
	ctx context.Context,
	sw *symbiontv1alpha1.ShadowWorkload,
	setDegraded func(reason, format string, args ...any),
	cpuOut, memOut *float64,
) {
	log := logf.FromContext(ctx)
	queries, err := sources.Resolve(&sw.Spec)
	if err != nil {
		r.Metrics.MeasurementFailed(sw.Namespace, sw.Name, metrics.FailureSourceMissing)
		setDegraded(reasonSourceInvalid, "%v", err)
		return
	}
	querier, err := r.QuerierFor(sw.Spec.Metrics.PrometheusURL)
	if err != nil {
		r.Metrics.MeasurementFailed(sw.Namespace, sw.Name, metrics.FailureSourceMissing)
		setDegraded(reasonSourceInvalid, "metrics backend config: %v", err)
		return
	}
	qctx, cancel := context.WithTimeout(ctx, queryTimeout+5*time.Second)
	defer cancel()
	cpu, mem, found, qerr := querier.QueryPair(qctx, queries)
	if qerr != nil {
		reason := metrics.FailurePrometheusUnavailable
		if errors.Is(qerr, sources.ErrMultiSample) {
			reason = metrics.FailureMultiSample
		}
		r.Metrics.MeasurementFailed(sw.Namespace, sw.Name, reason)
		setDegraded(reasonPrometheusUnavailable, "%v", qerr)
		log.Error(qerr, "Failed to query metrics backend", "prometheusURL", sw.Spec.Metrics.PrometheusURL)
		r.Recorder.Eventf(sw, corev1.EventTypeWarning, reasonPrometheusUnavailable,
			"Prometheus query failed: %v", qerr)
		return
	}
	r.Metrics.ObservedMeasurement(sw.Namespace, sw.Name, cpu, mem)
	r.Metrics.MeasurementSucceeded(sw.Namespace, sw.Name, time.Now())
	log.Info("Measured bare-metal footprint", "cpuCores", cpu, "memoryBytes", mem, "seriesFound", found)
	*cpuOut, *memOut = cpu, mem
}

// resizePhantom patches the pod resize subresource. Requests and limits are
// always written together: QoS class is immutable, so changing only one side
// would be rejected (or worse, change QoS).
func (r *ShadowWorkloadReconciler) resizePhantom(ctx context.Context, pod *corev1.Pod, desired symbiontv1alpha1.ResourcePair) error {
	base := pod.DeepCopy()
	resized := pod.DeepCopy()
	resources := corev1.ResourceList{
		corev1.ResourceCPU:    desired.CPU,
		corev1.ResourceMemory: desired.Memory,
	}
	for i := range resized.Spec.Containers {
		resized.Spec.Containers[i].Resources.Requests = resources.DeepCopy()
		resized.Spec.Containers[i].Resources.Limits = resources.DeepCopy()
	}
	if err := r.SubResource("resize").Patch(ctx, resized, client.StrategicMergeFrom(base)); err != nil {
		return err
	}
	// Keep the caller's observation aligned with the accepted API mutation so
	// status reflects the new requests. On rejection, pod remains untouched and
	// status continues to report the last accepted requests.
	*pod = *resized
	return nil
}

// nodeConditionReason maps a shadow.NodeEligible reason onto the Degraded
// condition reason reported in the ShadowWorkload status.
func nodeConditionReason(eligibilityReason string) string {
	switch eligibilityReason {
	case shadow.NodeReasonTerminating:
		return reasonNodeTerminating
	case shadow.NodeReasonNotReady:
		return reasonNodeNotReady
	default:
		return reasonNodeMissing
	}
}

// warnOnDegradedTransition emits a warning event only when the previously
// recorded Degraded condition carries a different reason. A steady poll clock
// (30s by default) would otherwise spam the event stream while a node stays
// down; identical consecutive reasons are already visible via the status.
func (r *ShadowWorkloadReconciler) warnOnDegradedTransition(sw *symbiontv1alpha1.ShadowWorkload, reason, msg string) {
	if cur := meta.FindStatusCondition(sw.Status.Conditions, conditionDegraded); cur != nil &&
		cur.Reason == reason && cur.Status == metav1.ConditionTrue {
		return
	}
	r.Recorder.Eventf(sw, corev1.EventTypeWarning, reason, "%s", msg)
}

func ownedBySw(pod *corev1.Pod, sw *symbiontv1alpha1.ShadowWorkload) bool {
	ref := metav1.GetControllerOf(pod)
	return ref != nil && ref.UID == sw.UID &&
		ref.Kind == "ShadowWorkload" && ref.APIVersion == symbiontv1alpha1.GroupVersion.String()
}

func pollInterval(sw *symbiontv1alpha1.ShadowWorkload) time.Duration {
	d, err := time.ParseDuration(sw.Spec.Update.PollInterval)
	if err != nil || d <= 0 {
		return defaultPollInterval
	}
	return d
}

// SetupWithManager sets up the controller with the Manager. Owning phantom
// pods means external deletion or modification triggers an immediate
// reconcile; the poll clock keeps the metric-driven cadence regardless.
func (r *ShadowWorkloadReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Recorder == nil {
		// GetEventRecorder returns the newer events recorder, which does not
		// implement the record.EventRecorder interface used by the fake recorder
		// in controller tests. Keep the compatible recorder until that interface
		// migration can be made deliberately across production and tests.
		r.Recorder = mgr.GetEventRecorderFor("kube-symbiont") //nolint:staticcheck
	}
	if r.QuerierFor == nil {
		r.QuerierFor = func(rawURL string) (MetricsQuerier, error) {
			return sources.NewClient(rawURL, queryTimeout)
		}
	}
	if r.Metrics == nil {
		r.Metrics = metrics.New()
	}
	// Serve the symbiont series from controller-runtime's metrics.Registry —
	// the same registry the manager's metrics endpoint exposes — alongside,
	// never duplicating, controller-runtime's own reconcile metrics.
	if err := r.Metrics.Register(crmetrics.Registry); err != nil {
		return fmt.Errorf("register shadow workload metrics: %w", err)
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&symbiontv1alpha1.ShadowWorkload{}).
		Owns(&corev1.Pod{}).
		Named("shadowworkload").
		Complete(r)
}
