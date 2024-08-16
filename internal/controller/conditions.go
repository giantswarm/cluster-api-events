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

func conditionTimeStampFromReadyState(object capiconditions.Getter, condition capi.ConditionType) (*v1.Time, bool) {
	if isClusterReady(object, condition) {
		time := capiconditions.GetLastTransitionTime(object, condition)
		if time != nil {
			return time, true
		}
	}
	return nil, false
}

func isClusterReleaseVersionDifferent(cluster *capi.Cluster) bool {
	return cluster.Labels["release.giantswarm.io/version"] != cluster.Annotations["giantswarm.io/last-known-cluster-upgrade-version"]
}
