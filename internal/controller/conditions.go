package controller

import (
	"context"
	"fmt"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	capi "sigs.k8s.io/cluster-api/api/v1beta1"
	capiexp "sigs.k8s.io/cluster-api/exp/api/v1beta1"
	capiconditions "sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
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
	return cluster.Labels[ReleaseVersionLabel] != cluster.Annotations[LastKnownUpgradeVersionAnnotation]
}

// areAllWorkerNodesReady checks if all MachineDeployments and MachinePools are ready
func areAllWorkerNodesReady(ctx context.Context, c client.Client, cluster *capi.Cluster) (bool, error) {
	machineDeployments := &capi.MachineDeploymentList{}
	if err := c.List(ctx, machineDeployments, client.InNamespace(cluster.Namespace), client.MatchingLabels{
		capi.ClusterNameLabel: cluster.Name,
	}); err != nil {
		return false, err
	}

	var notReadyMDs []string
	var versionMismatchMDs []string
	for _, md := range machineDeployments.Items {
		ready := isClusterReady(&md, capi.ReadyCondition)
		versionMatch := md.Labels[ReleaseVersionLabel] == cluster.Labels[ReleaseVersionLabel]

		log := log.FromContext(ctx)
		log.V(1).Info("Checking MachineDeployment status",
			"name", md.Name,
			"ready", ready,
			"versionMatch", versionMatch,
			"mdVersion", md.Labels[ReleaseVersionLabel],
			"clusterVersion", cluster.Labels[ReleaseVersionLabel],
			"readyCondition", func() string {
				if condition := capiconditions.Get(&md, capi.ReadyCondition); condition != nil {
					return fmt.Sprintf("status=%s,reason=%s", condition.Status, condition.Reason)
				}
				return "missing"
			}())

		if !ready {
			notReadyMDs = append(notReadyMDs, md.Name)
		}

		if !versionMatch {
			versionMismatchMDs = append(versionMismatchMDs, fmt.Sprintf("%s(%s->%s)",
				md.Name,
				md.Labels[ReleaseVersionLabel],
				cluster.Labels[ReleaseVersionLabel]))
		}
	}

	machinePools := &capiexp.MachinePoolList{}
	if err := c.List(ctx, machinePools, client.InNamespace(cluster.Namespace), client.MatchingLabels{
		capi.ClusterNameLabel: cluster.Name,
	}); err != nil {
		return false, err
	}

	var notReadyMPs []string
	var versionMismatchMPs []string
	for _, mp := range machinePools.Items {
		ready := isClusterReady(&mp, capi.ReadyCondition)
		versionMatch := mp.Labels[ReleaseVersionLabel] == cluster.Labels[ReleaseVersionLabel]

		log := log.FromContext(ctx)
		log.V(1).Info("Checking MachinePool status",
			"name", mp.Name,
			"ready", ready,
			"versionMatch", versionMatch,
			"mpVersion", mp.Labels[ReleaseVersionLabel],
			"clusterVersion", cluster.Labels[ReleaseVersionLabel],
			"readyCondition", func() string {
				if condition := capiconditions.Get(&mp, capi.ReadyCondition); condition != nil {
					return fmt.Sprintf("status=%s,reason=%s", condition.Status, condition.Reason)
				}
				return "missing"
			}())

		if !ready {
			notReadyMPs = append(notReadyMPs, mp.Name)
		}

		if !versionMatch {
			versionMismatchMPs = append(versionMismatchMPs, fmt.Sprintf("%s(%s->%s)",
				mp.Name,
				mp.Labels[ReleaseVersionLabel],
				cluster.Labels[ReleaseVersionLabel]))
		}
	}

	allReady := len(notReadyMDs) == 0 && len(notReadyMPs) == 0 && len(versionMismatchMDs) == 0 && len(versionMismatchMPs) == 0

	if !allReady {
		log := log.FromContext(ctx)
		log.Info("Worker nodes not all ready",
			"totalMachineDeployments", len(machineDeployments.Items),
			"totalMachinePools", len(machinePools.Items),
			"notReadyMachineDeployments", notReadyMDs,
			"notReadyMachinePools", notReadyMPs,
			"versionMismatchMachineDeployments", versionMismatchMDs,
			"versionMismatchMachinePools", versionMismatchMPs)
	}

	return allReady, nil
}

// areAnyWorkerNodesUpgrading checks if any MachineDeployments or MachinePools are upgrading
func areAnyWorkerNodesUpgrading(ctx context.Context, c client.Client, cluster *capi.Cluster) (bool, error) {
	machineDeployments := &capi.MachineDeploymentList{}
	if err := c.List(ctx, machineDeployments, client.InNamespace(cluster.Namespace), client.MatchingLabels{
		capi.ClusterNameLabel: cluster.Name,
	}); err != nil {
		return false, err
	}

	var upgradingMDs []string
	for _, md := range machineDeployments.Items {
		if isClusterUpgrading(&md, capi.ReadyCondition) {
			upgradingMDs = append(upgradingMDs, md.Name)
		}
	}

	machinePools := &capiexp.MachinePoolList{}
	if err := c.List(ctx, machinePools, client.InNamespace(cluster.Namespace), client.MatchingLabels{
		capi.ClusterNameLabel: cluster.Name,
	}); err != nil {
		return false, err
	}

	var upgradingMPs []string
	for _, mp := range machinePools.Items {
		if isClusterUpgrading(&mp, capi.ReadyCondition) {
			upgradingMPs = append(upgradingMPs, mp.Name)
		}
	}

	anyUpgrading := len(upgradingMDs) > 0 || len(upgradingMPs) > 0

	if anyUpgrading {
		log := log.FromContext(ctx)
		log.Info("Worker nodes upgrading detected",
			"upgradingMachineDeployments", upgradingMDs,
			"upgradingMachinePools", upgradingMPs)
	}

	return anyUpgrading, nil
}
