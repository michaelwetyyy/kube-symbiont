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

// NodeEligible reports whether the target node can host a newly created
// phantom pod; it belongs to the pure phantom mechanics in this package.
package shadow

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// Node eligibility reasons returned by NodeEligible. The controller maps them
// onto Degraded condition reasons verbatim.
const (
	// NodeReasonMissing marks a target that does not exist at all.
	NodeReasonMissing = "missing"
	// NodeReasonTerminating marks a node whose deletion has started; its
	// allocatable is draining and new bindings would strand.
	NodeReasonTerminating = "terminating"
	// NodeReasonNotReady covers Ready=False/Unknown and nodes that have not
	// reported any Ready condition yet (freshly registered or kubelet-down).
	NodeReasonNotReady = "not-ready"
)

// NodeEligible reports whether node can host a newly created phantom pod right
// now. A phantom is pinned via spec.nodeName, so it bypasses the scheduler
// entirely: if the node is absent, terminating or NotReady, nothing will ever
// run the pod's containers and it sits Pending forever. Eligibility therefore
// requires a live, Ready node. The returned string is "" when eligible and one
// of the NodeReason* constants otherwise.
func NodeEligible(node *corev1.Node) (bool, string) {
	if node == nil {
		return false, NodeReasonMissing
	}
	if node.DeletionTimestamp != nil {
		return false, NodeReasonTerminating
	}
	for i := range node.Status.Conditions {
		c := &node.Status.Conditions[i]
		if c.Type == corev1.NodeReady {
			if c.Status != corev1.ConditionTrue {
				return false, NodeReasonNotReady
			}
			return true, ""
		}
	}
	// No Ready condition reported yet: treat like NotReady rather than
	// optimistically creating a phantom that cannot be scheduled onto.
	return false, NodeReasonNotReady
}

// NodeEligibilityError renders an eligibility reason as a human-readable
// message for conditions and events.
func NodeEligibilityError(nodeName, reason string) string {
	switch reason {
	case NodeReasonMissing:
		return fmt.Sprintf("target node %q does not exist", nodeName)
	case NodeReasonTerminating:
		return fmt.Sprintf("target node %q is terminating; phantom creation deferred", nodeName)
	default:
		return fmt.Sprintf("target node %q is not Ready; phantom creation deferred", nodeName)
	}
}
