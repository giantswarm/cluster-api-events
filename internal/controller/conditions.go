package controller

import (
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	capi "sigs.k8s.io/cluster-api/api/v1beta1"
	capiconditions "sigs.k8s.io/cluster-api/util/conditions"
)

func isClusterReady(object capiconditions.Getter, condition capi.ConditionType) bool {
	return capiconditions.IsTrue(object, condition)
}

func isClusterUpgrading(object capiconditions.Getter, condition capi.ConditionType) bool {
	return capiconditions.IsFalse(object, condition) && capiconditions.GetReason(object, condition) == "RollingUpdateInProgress"
}

func conditionTimeStamp(object capiconditions.Getter, condition capi.ConditionType) (*v1.Time, bool) {
	time := capiconditions.GetLastTransitionTime(object, condition)
	if time != nil {
		return time, true
	}
	return nil, false
}