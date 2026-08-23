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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func readyNode(status corev1.ConditionStatus) *corev1.Node {
	return &corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{
		Type:   corev1.NodeReady,
		Status: status,
	}}}}
}

func TestNodeEligible(t *testing.T) {
	cases := []struct {
		name       string
		node       *corev1.Node
		wantOK     bool
		wantReason string
	}{
		{"nil node reads as missing", nil, false, NodeReasonMissing},
		{"ready node is eligible", readyNode(corev1.ConditionTrue), true, ""},
		{"ready=false is not ready", readyNode(corev1.ConditionFalse), false, NodeReasonNotReady},
		{"ready=unknown is not ready", readyNode(corev1.ConditionUnknown), false, NodeReasonNotReady},
		{"no ready condition yet is not ready", &corev1.Node{}, false, NodeReasonNotReady},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, reason := NodeEligible(tc.node)
			if ok != tc.wantOK || reason != tc.wantReason {
				t.Fatalf("NodeEligible = (%v, %q), want (%v, %q)", ok, reason, tc.wantOK, tc.wantReason)
			}
		})
	}
}

func TestNodeEligibleTerminatingBeatsReady(t *testing.T) {
	node := readyNode(corev1.ConditionTrue)
	now := metav1.Now()
	node.DeletionTimestamp = &now

	ok, reason := NodeEligible(node)
	if ok || reason != NodeReasonTerminating {
		t.Fatalf("terminating node: got (%v, %q), want (false, %q)", ok, reason, NodeReasonTerminating)
	}
}

func TestNodeEligibleIgnoresNonReadyConditions(t *testing.T) {
	node := &corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
		{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionTrue},
	}}}
	if _, reason := NodeEligible(node); reason != NodeReasonNotReady {
		t.Fatalf("pressure-only node: got reason %q, want %q", reason, NodeReasonNotReady)
	}
}

func TestNodeEligibilityErrorMessages(t *testing.T) {
	cases := map[string]string{
		NodeReasonMissing:     "does not exist",
		NodeReasonTerminating: "terminating",
		NodeReasonNotReady:    "not Ready",
		"bogus":               "not Ready",
	}
	for reason, want := range cases {
		msg := NodeEligibilityError("lab", reason)
		if !strings.Contains(msg, want) {
			t.Errorf("NodeEligibilityError(lab, %q) = %q, want substring %q", reason, msg, want)
		}
	}
}
