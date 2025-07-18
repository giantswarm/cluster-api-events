package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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

		var desiredReplicas int32
		if md.Spec.Replicas != nil {
			desiredReplicas = *md.Spec.Replicas
		}

		rolloutComplete := desiredReplicas == md.Status.UpdatedReplicas &&
			desiredReplicas == md.Status.ReadyReplicas &&
			desiredReplicas == md.Status.AvailableReplicas

		log := log.FromContext(ctx)
		log.V(1).Info("Checking MachineDeployment status",
			"name", md.Name,
			"ready", ready,
			"rolloutComplete", rolloutComplete,
			"versionMatch", versionMatch,
			"desiredReplicas", desiredReplicas,
			"updatedReplicas", md.Status.UpdatedReplicas,
			"readyReplicas", md.Status.ReadyReplicas,
			"availableReplicas", md.Status.AvailableReplicas,
			"mdVersion", md.Labels[ReleaseVersionLabel],
			"clusterVersion", cluster.Labels[ReleaseVersionLabel],
			"readyCondition", func() string {
				if condition := capiconditions.Get(&md, capi.ReadyCondition); condition != nil {
					return fmt.Sprintf("status=%s,reason=%s", condition.Status, condition.Reason)
				}
				return "missing"
			}())

		if !ready || !rolloutComplete {
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

		var desiredReplicas int32
		if mp.Spec.Replicas != nil {
			desiredReplicas = *mp.Spec.Replicas
		}

		// For MachinePools, we need to ensure that:
		// 1. Ready/Available replicas match desired replicas exactly (no extra old nodes)
		// 2. All referenced nodes are running the expected version FOR THIS MACHINEPOOL
		//    (not necessarily the cluster version, to support staged upgrades)
		rolloutComplete := desiredReplicas == mp.Status.ReadyReplicas &&
			desiredReplicas == mp.Status.AvailableReplicas

		// Additional check: verify all node references are running the MachinePool's expected version
		// This supports staged upgrades where MachinePools may be pinned to older versions
		allNodesCorrectVersion := true
		if rolloutComplete && mp.Spec.Template.Spec.Version != nil && *mp.Spec.Template.Spec.Version != "" {
			expectedVersion := *mp.Spec.Template.Spec.Version

			// Get all nodes referenced by this MachinePool and check their versions
			for _, nodeRef := range mp.Status.NodeRefs {
				node := &corev1.Node{}
				if err := c.Get(ctx, types.NamespacedName{Name: nodeRef.Name}, node); err != nil {
					log := log.FromContext(ctx)
					log.V(1).Info("Failed to get node for version check",
						"machinePool", mp.Name,
						"node", nodeRef.Name,
						"error", err)
					allNodesCorrectVersion = false
					break
				}

				// Skip nodes that are being drained/terminated (SchedulingDisabled)
				// These nodes are in the process of being removed and shouldn't block upgrade completion
				if node.Spec.Unschedulable {
					log := log.FromContext(ctx)
					log.V(1).Info("Skipping unschedulable node in version check",
						"machinePool", mp.Name,
						"node", nodeRef.Name,
						"nodeVersion", node.Status.NodeInfo.KubeletVersion)
					continue
				}

				if node.Status.NodeInfo.KubeletVersion != expectedVersion {
					log := log.FromContext(ctx)
					log.V(1).Info("Node version mismatch detected",
						"machinePool", mp.Name,
						"node", nodeRef.Name,
						"nodeVersion", node.Status.NodeInfo.KubeletVersion,
						"expectedVersion", expectedVersion)
					allNodesCorrectVersion = false
					break
				}
			}
		}

		rolloutComplete = rolloutComplete && allNodesCorrectVersion

		log := log.FromContext(ctx)
		log.V(1).Info("Checking MachinePool status",
			"name", mp.Name,
			"ready", ready,
			"rolloutComplete", rolloutComplete,
			"allNodesCorrectVersion", allNodesCorrectVersion,
			"versionMatch", versionMatch,
			"desiredReplicas", desiredReplicas,
			"readyReplicas", mp.Status.ReadyReplicas,
			"availableReplicas", mp.Status.AvailableReplicas,
			"nodeRefsCount", len(mp.Status.NodeRefs),
			"machinePoolExpectedVersion", func() string {
				if mp.Spec.Template.Spec.Version != nil {
					return *mp.Spec.Template.Spec.Version
				}
				return ""
			}(),
			"mpLabelVersion", mp.Labels[ReleaseVersionLabel],
			"clusterVersion", cluster.Labels[ReleaseVersionLabel],
			"readyCondition", func() string {
				if condition := capiconditions.Get(&mp, capi.ReadyCondition); condition != nil {
					return fmt.Sprintf("status=%s,reason=%s", condition.Status, condition.Reason)
				}
				return "missing"
			}())

		if !ready || !rolloutComplete {
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
