package controller

import (
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/api7/aisix-controller/api/v1alpha1"
	"github.com/api7/aisix-controller/internal/render"
)

func deployment(replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{Spec: appsv1.DeploymentSpec{Replicas: ptr.To(replicas)}}
}

func pod(name string, ready bool) corev1.Pod {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			PodIP:      "10.0.0.1",
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}},
		},
	}
}

func TestEvaluate(t *testing.T) {
	want := "abc"
	pods := []corev1.Pod{pod("a", true), pod("b", true), pod("c", false)}
	ready := readyPods(pods)
	if len(ready) != 2 {
		t.Fatalf("ready = %d", len(ready))
	}

	cases := []struct {
		name     string
		dep      *appsv1.Deployment
		statuses map[string]PodStatus
		errs     map[string]error
		ready    bool
		reason   string
		substr   string
	}{
		{"missing deployment", nil, nil, nil, false, "GatewayNotFound", "does not exist"},
		{"scaled to zero", deployment(0), nil, nil, false, "Pending", "scaled to zero"},
		{"all serve", deployment(2), map[string]PodStatus{"a": {ConfigHash: want}, "b": {ConfigHash: want}}, nil, true, "Programmed", "2 gateway pod"},
		{"one stale", deployment(2), map[string]PodStatus{"a": {ConfigHash: want}, "b": {ConfigHash: "old"}}, nil, false, "Pending", "1/2"},
		{"stale with failure", deployment(2), map[string]PodStatus{"a": {ConfigHash: want}, "b": {ConfigHash: "old", LastFailure: "boom"}}, nil, false, "Pending", "pod b: boom"},
		{"unreachable", deployment(2), map[string]PodStatus{"a": {ConfigHash: want}}, map[string]error{"b": errors.New("refused")}, false, "Pending", "pod b: refused"},
		{"too few ready", deployment(3), map[string]PodStatus{"a": {ConfigHash: want}, "b": {ConfigHash: want}}, nil, false, "Pending", "2/3"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := ready
			if c.dep == nil {
				r = nil
			}
			gw := evaluate(c.dep, r, c.statuses, c.errs, want)
			if gw.Ready != c.ready || gw.Reason != c.reason || !strings.Contains(gw.Message, c.substr) {
				t.Errorf("got %+v", gw)
			}
		})
	}
}

func cond(st v1alpha1.ResourceStatus, t string) *metav1.Condition {
	return meta.FindStatusCondition(st.Conditions, t)
}

func TestDesiredStatus(t *testing.T) {
	applied := render.Disposition{State: render.Applied}
	ready := Gateway{Ready: true, Reason: "Programmed", Message: "served"}
	pending := Gateway{Reason: "Pending", Message: "1/2"}

	cases := []struct {
		name                string
		d                   render.Disposition
		known               bool
		docErr              string
		written             bool
		gw                  Gateway
		accepted, program   metav1.ConditionStatus
		acceptedR, programR string
	}{
		{"applied and served", applied, true, "", true, ready, "True", "True", "Accepted", "Programmed"},
		{"applied while rolling", applied, true, "", true, pending, "True", "False", "Accepted", "Pending"},
		{"applied, not written", applied, true, "", false, ready, "True", "False", "Accepted", "Pending"},
		{"retained", render.Disposition{State: render.Retained, Reason: "bad"}, true, "", true, ready, "False", "False", "Invalid", "Invalid"},
		{"rejected", render.Disposition{State: render.Rejected, Reason: "bad"}, true, "", true, ready, "False", "False", "Invalid", "Invalid"},
		{"document invalid", applied, true, "boom", false, ready, "Unknown", "False", "Pending", "ConfigurationRejected"},
		{"unknown", render.Disposition{}, false, "", true, ready, "Unknown", "Unknown", "Pending", "Pending"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := desiredStatus(v1alpha1.ResourceStatus{}, 3, c.d, c.known, c.docErr, c.written, c.gw)
			a, p := cond(st, CondAccepted), cond(st, CondProgrammed)
			if a.Status != c.accepted || a.Reason != c.acceptedR || p.Status != c.program || p.Reason != c.programR {
				t.Errorf("accepted=%s/%s programmed=%s/%s", a.Status, a.Reason, p.Status, p.Reason)
			}
			if st.ObservedGeneration != 3 || a.ObservedGeneration != 3 || p.ObservedGeneration != 3 {
				t.Error("observedGeneration not set")
			}
		})
	}
}

func TestTransitionTimeIsKeptWhenStatusIsUnchanged(t *testing.T) {
	applied := render.Disposition{State: render.Applied}
	gw := Gateway{Ready: true, Reason: "Programmed", Message: "served"}
	first := desiredStatus(v1alpha1.ResourceStatus{}, 1, applied, true, "", true, gw)
	old := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
	for i := range first.Conditions {
		first.Conditions[i].LastTransitionTime = old
	}
	second := desiredStatus(first, 2, applied, true, "", true, gw)
	if !cond(second, CondProgrammed).LastTransitionTime.Equal(&old) {
		t.Error("transition time moved without a status change")
	}
	third := desiredStatus(second, 2, render.Disposition{State: render.Rejected, Reason: "x"}, true, "", true, gw)
	if cond(third, CondAccepted).LastTransitionTime.Equal(&old) {
		t.Error("transition time kept across a status change")
	}
}
