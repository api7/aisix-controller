package controller

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/api7/aisix-controller/api/v1alpha1"
	"github.com/api7/aisix-controller/internal/render"
)

const (
	CondAccepted   = "Accepted"
	CondProgrammed = "Programmed"
)

// desiredStatus derives one resource's conditions. Accepted: its current
// spec is valid. Programmed: the gateway serves its current spec.
func desiredStatus(prev v1alpha1.ResourceStatus, generation int64, d render.Disposition, known bool, docErr string, written bool, gw Gateway) v1alpha1.ResourceStatus {
	st := *prev.DeepCopy()
	st.ObservedGeneration = generation
	set := func(t string, s metav1.ConditionStatus, reason, msg string) {
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{
			Type: t, Status: s, Reason: reason, Message: msg, ObservedGeneration: generation,
		})
	}

	switch {
	case !known:
		set(CondAccepted, metav1.ConditionUnknown, "Pending", "not reconciled yet")
		set(CondProgrammed, metav1.ConditionUnknown, "Pending", "not reconciled yet")
	case d.State == render.Retained && docErr == "":
		set(CondAccepted, metav1.ConditionFalse, "Invalid", d.Reason)
		set(CondProgrammed, metav1.ConditionFalse, "Invalid", "the previously accepted version keeps serving")
	case d.State != render.Applied:
		set(CondAccepted, metav1.ConditionFalse, "Invalid", d.Reason)
		set(CondProgrammed, metav1.ConditionFalse, "Invalid", "not served")
	case docErr != "":
		set(CondAccepted, metav1.ConditionUnknown, "Pending", "the resources file as a whole is invalid")
		set(CondProgrammed, metav1.ConditionFalse, "ConfigurationRejected", docErr)
	default:
		set(CondAccepted, metav1.ConditionTrue, "Accepted", "valid")
		switch {
		case !written:
			set(CondProgrammed, metav1.ConditionFalse, "Pending", "waiting for the resources file to be written")
		case gw.Ready:
			set(CondProgrammed, metav1.ConditionTrue, gw.Reason, gw.Message)
		default:
			set(CondProgrammed, metav1.ConditionFalse, gw.Reason, gw.Message)
		}
	}
	return st
}
