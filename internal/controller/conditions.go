package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	capi "sigs.k8s.io/cluster-api/api/core/v1beta2"
	capiconditions "sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

func isClusterReady(object capiconditions.Getter, condition string) bool {
	return capiconditions.IsTrue(object, condition)
}

func isClusterUpgrading(object capiconditions.Getter) bool {
	return capiconditions.IsTrue(object, capi.RollingOutCondition)
}

func conditionTimeStampFromAvailableState(object capiconditions.Getter, condition string) (*metav1.Time, bool) {
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

// getWorkloadClusterClient creates a Kubernetes client for the workload cluster
func getWorkloadClusterClient(ctx context.Context, c client.Client, cluster *capi.Cluster) (kubernetes.Interface, error) {
	// Get the kubeconfig secret for the workload cluster
	secretName := fmt.Sprintf("%s-kubeconfig", cluster.Name)
	secret := &corev1.Secret{}

	if err := c.Get(ctx, types.NamespacedName{
		Name:      secretName,
		Namespace: cluster.Namespace,
	}, secret); err != nil {
		return nil, fmt.Errorf("failed to get kubeconfig secret %s: %w", secretName, err)
	}

	// Extract kubeconfig data
	kubeconfigData, ok := secret.Data["value"]
	if !ok {
		return nil, fmt.Errorf("kubeconfig secret %s missing 'value' key", secretName)
	}

	// Create client config from kubeconfig
	config, err := clientcmd.RESTConfigFromKubeConfig(kubeconfigData)
	if err != nil {
		return nil, fmt.Errorf("failed to create REST config from kubeconfig: %w", err)
	}

	// Check if this kubeconfig points to the same cluster we're running on (management cluster)
	// Compare the server URL from the kubeconfig with our current cluster
	// Get the current cluster configuration (where this controller is running)
	currentConfig := ctrl.GetConfigOrDie()
	if config.Host == currentConfig.Host {
		log := log.FromContext(ctx)
		log.V(1).Info("Detected management cluster self-reference, skipping workload cluster connection",
			"cluster", cluster.Name,
			"targetServer", config.Host,
			"currentServer", currentConfig.Host,
			"reason", "prevents controller from connecting to itself during MC upgrades")
		return nil, fmt.Errorf("management cluster self-reference detected - skipping to prevent self-connection")
	}

	// Create Kubernetes client
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create Kubernetes client: %w", err)
	}

	return clientset, nil
}

// checkMachinePoolNodeVersions connects to workload cluster and checks node versions for a MachinePool
func checkMachinePoolNodeVersions(ctx context.Context, workloadClient kubernetes.Interface, mp *capi.MachinePool, expectedVersion string) (bool, []string, error) {
	logger := log.FromContext(ctx)

	// Try primary Giant Swarm label first
	labelSelector := fmt.Sprintf("giantswarm.io/machine-pool=%s", mp.Name)

	// List nodes with minimal fields to reduce memory usage
	nodes, err := workloadClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
		// Limit fields to only what we need to reduce memory usage
		FieldSelector: "spec.unschedulable!=true",
		// Limit the number of nodes returned to prevent memory issues
		Limit: 100,
	})
	if err != nil {
		return false, nil, fmt.Errorf("failed to list nodes for MachinePool %s: %w", mp.Name, err)
	}

	// If no nodes found with primary label, try Karpenter label as fallback
	// This handles cases where Karpenter NodePools are not configured with giantswarm.io/machine-pool label
	if len(nodes.Items) == 0 {
		karpenterSelector := fmt.Sprintf("karpenter.sh/nodepool=%s", mp.Name)
		logger.Info("No nodes found with giantswarm label, trying Karpenter label as fallback",
			"machinePool", mp.Name,
			"primaryLabel", labelSelector,
			"fallbackLabel", karpenterSelector)

		nodes, err = workloadClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{
			LabelSelector: karpenterSelector,
			FieldSelector: "spec.unschedulable!=true",
			Limit:         100,
		})
		if err != nil {
			return false, nil, fmt.Errorf("failed to list nodes for MachinePool %s with Karpenter label: %w", mp.Name, err)
		}

		if len(nodes.Items) > 0 {
			labelSelector = karpenterSelector // Update for logging
			logger.Info("Found nodes using Karpenter label fallback",
				"machinePool", mp.Name,
				"nodeCount", len(nodes.Items),
				"labelSelector", karpenterSelector)
		}
	}

	// If no nodes found with either label, return true since there are no nodes to upgrade.
	// This handles the case where:
	// - Karpenter NodePool has 0 nodes (no workloads scheduled, scale-to-zero, etc.)
	// - Nodes are still being provisioned (will get new version from launch template)
	// For externally-managed pools (Karpenter), this is expected behavior - future nodes
	// will be provisioned with the new configuration.
	if len(nodes.Items) == 0 {
		logger.Info("No nodes found for MachinePool in workload cluster - considering ready (0 nodes = 0 nodes to upgrade)",
			"machinePool", mp.Name,
			"labelSelector", labelSelector,
			"expectedVersion", expectedVersion)
		return true, nil, nil
	}

	logger.V(1).Info("Checking node versions for MachinePool",
		"machinePool", mp.Name,
		"nodeCount", len(nodes.Items),
		"expectedVersion", expectedVersion,
		"labelSelector", labelSelector)

	var nodesWithWrongVersion []string
	allCorrectVersion := true
	checkedNodeCount := 0

	for _, node := range nodes.Items {
		// Skip nodes being drained/cordoned (unschedulable) - should be filtered by FieldSelector but double-check
		if node.Spec.Unschedulable {
			logger.V(1).Info("Skipping unschedulable node in MachinePool version check",
				"machinePool", mp.Name,
				"node", node.Name,
				"nodeVersion", node.Status.NodeInfo.KubeletVersion)
			continue
		}

		checkedNodeCount++
		nodeVersion := node.Status.NodeInfo.KubeletVersion

		// Check kubelet version
		if nodeVersion != expectedVersion {
			nodesWithWrongVersion = append(nodesWithWrongVersion,
				fmt.Sprintf("%s(%s!=%s)", node.Name, nodeVersion, expectedVersion))
			allCorrectVersion = false

			logger.Info("Node has wrong version",
				"machinePool", mp.Name,
				"node", node.Name,
				"nodeVersion", nodeVersion,
				"expectedVersion", expectedVersion)

			// Limit the number of wrong version nodes we track to prevent memory bloat
			if len(nodesWithWrongVersion) >= 10 {
				logger.V(1).Info("Truncating wrong version nodes list to prevent memory usage",
					"machinePool", mp.Name,
					"totalWrongVersionNodes", "10+")
				break
			}
		}
	}

	// If all nodes were skipped (unschedulable), be conservative
	if checkedNodeCount == 0 {
		logger.Info("All nodes for MachinePool are unschedulable, marking as not ready",
			"machinePool", mp.Name,
			"totalNodes", len(nodes.Items))
		return false, []string{"all nodes unschedulable"}, nil
	}

	logger.V(1).Info("MachinePool node version check complete",
		"machinePool", mp.Name,
		"checkedNodes", checkedNodeCount,
		"allCorrectVersion", allCorrectVersion,
		"nodesWithWrongVersion", nodesWithWrongVersion)

	return allCorrectVersion, nodesWithWrongVersion, nil
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
		// In v1beta2, MachineDeployments use Available condition instead of Ready
		// Fall back to Ready condition for v1beta1 compatibility
		ready := isClusterReady(&md, capi.AvailableCondition)
		if !ready {
			ready = isClusterReady(&md, capi.ReadyCondition)
		}
		// Check if all machines in the MachineDeployment are up-to-date (v1beta2)
		machinesUpToDate := isClusterReady(&md, capi.MachineDeploymentMachinesUpToDateCondition)
		// Check version match - MachineDeployment label should match cluster version
		// This indicates the MachineDeployment is configured for the right release
		versionMatch := md.Labels[ReleaseVersionLabel] == cluster.Labels[ReleaseVersionLabel]

		var desiredReplicas int32
		if md.Spec.Replicas != nil {
			desiredReplicas = *md.Spec.Replicas
		}

		// For MachineDeployments, check both basic status and the v1beta2 MachinesUpToDate condition
		basicRolloutComplete := desiredReplicas == *md.Status.Replicas &&
			desiredReplicas == *md.Status.ReadyReplicas &&
			desiredReplicas == *md.Status.AvailableReplicas

		// In v1beta2, use the MachinesUpToDate condition which is maintained by CAPI
		// This is more reliable than checking individual machines
		rolloutComplete := basicRolloutComplete && machinesUpToDate

		log := log.FromContext(ctx)
		log.V(1).Info("Checking MachineDeployment status",
			"name", md.Name,
			"ready", ready,
			"machinesUpToDate", machinesUpToDate,
			"basicRolloutComplete", basicRolloutComplete,
			"rolloutComplete", rolloutComplete,
			"versionMatch", versionMatch,
			"desiredReplicas", desiredReplicas,
			"replicas", md.Status.Replicas,
			"readyReplicas", *md.Status.ReadyReplicas,
			"availableReplicas", *md.Status.AvailableReplicas,
			"mdVersion", md.Labels[ReleaseVersionLabel],
			"clusterVersion", cluster.Labels[ReleaseVersionLabel],
			"availableCondition", func() string {
				if condition := capiconditions.Get(&md, capi.AvailableCondition); condition != nil {
					return fmt.Sprintf("status=%s,reason=%s", condition.Status, condition.Reason)
				}
				return "missing"
			}(),
			"machinesUpToDateCondition", func() string {
				if condition := capiconditions.Get(&md, capi.MachineDeploymentMachinesUpToDateCondition); condition != nil {
					return fmt.Sprintf("status=%s,reason=%s", condition.Status, condition.Reason)
				}
				return "missing"
			}())

		if !ready || !rolloutComplete {
			notReadyMDs = append(notReadyMDs, md.Name)
		}

		if !versionMatch {
			// MachineDeployment version mismatch - label doesn't match cluster
			versionMismatchMDs = append(versionMismatchMDs, fmt.Sprintf("%s(label:%s!=cluster:%s)",
				md.Name,
				md.Labels[ReleaseVersionLabel],
				cluster.Labels[ReleaseVersionLabel]))
		}
	}

	machinePools := &capi.MachinePoolList{}
	if err := c.List(ctx, machinePools, client.InNamespace(cluster.Namespace), client.MatchingLabels{
		capi.ClusterNameLabel: cluster.Name,
	}); err != nil {
		return false, err
	}

	// Get workload cluster client to check for non-CAPI managed nodes (e.g., Karpenter)
	// This is a supplementary check - the primary check is CAPI conditions
	var workloadClient kubernetes.Interface
	var workloadClientErr error
	if len(machinePools.Items) > 0 || cluster.Status.ControlPlane != nil {
		workloadClient, workloadClientErr = getWorkloadClusterClient(ctx, c, cluster)
		if workloadClientErr != nil {
			log := log.FromContext(ctx)
			log.V(1).Info("Failed to get workload cluster client, will rely on CAPI status only",
				"cluster", cluster.Name,
				"error", workloadClientErr)
			workloadClient = nil
		}
	}

	var notReadyMPs []string
	var versionMismatchMPs []string
	for _, mp := range machinePools.Items {
		logger := log.FromContext(ctx)

		// Check if this MachinePool is managed by an external autoscaler (Cluster Autoscaler, Karpenter, etc.)
		// See: https://cluster-api.sigs.k8s.io/developer/architecture/controllers/machine-pool#primitives
		isExternallyManaged := mp.Annotations["cluster.x-k8s.io/replicas-managed-by"] == "external-autoscaler"

		// In v1beta2, MachinePools don't have Available/Ready conditions in status.conditions
		// (they only have Paused). The old conditions are in status.deprecated.v1beta1.conditions.
		// Instead, we check status fields directly which are reliable:
		// - status.replicas, status.readyReplicas, status.availableReplicas
		// - status.phase indicates the overall MachinePool state
		phase := capi.MachinePoolPhase(mp.Status.Phase)
		ready := phase == capi.MachinePoolPhaseRunning ||
			phase == capi.MachinePoolPhaseScaling ||
			phase == capi.MachinePoolPhaseScalingUp ||
			phase == capi.MachinePoolPhaseScalingDown

		// Check version match - MachinePool label should match cluster version
		// Note: mp.Spec.Template.Spec.Version uses Kubernetes version format (v1.31.9)
		// while labels use release version format (31.0.0), so we can't compare them directly
		versionMatch := mp.Labels[ReleaseVersionLabel] == cluster.Labels[ReleaseVersionLabel]

		var desiredReplicas int32
		if mp.Spec.Replicas != nil {
			desiredReplicas = *mp.Spec.Replicas
		}

		// Check basic readiness - in v1beta2, upToDateReplicas should be available in status
		basicRolloutComplete := desiredReplicas == *mp.Status.ReadyReplicas &&
			desiredReplicas == *mp.Status.AvailableReplicas

		// For MachinePools, we MUST verify actual node versions in workload cluster
		// This is critical because:
		// 1. v1beta1 MachinePools don't have upToDateReplicas field
		// 2. Karpenter provisions nodes independently from CAPI
		// 3. Status fields might not reflect actual node versions
		var expectedVersion string
		if mp.Spec.Template.Spec.Version != "" {
			expectedVersion = mp.Spec.Template.Spec.Version
		} else {
			expectedVersion = cluster.Labels[ReleaseVersionLabel]
		}

		allNodesCorrectVersion := true
		var nodesWithWrongVersion []string
		nodeVersionCheckPerformed := false

		if workloadClient != nil {
			var err error
			allNodesCorrectVersion, nodesWithWrongVersion, err = checkMachinePoolNodeVersions(ctx, workloadClient, &mp, expectedVersion)
			if err != nil {
				log := log.FromContext(ctx)
				log.V(1).Info("Failed to check node versions in workload cluster for MachinePool",
					"machinePool", mp.Name,
					"error", err)
				nodeVersionCheckPerformed = false
			} else {
				nodeVersionCheckPerformed = true
			}
		}

		// If workload cluster check was not performed (client unavailable or error),
		// we CANNOT trust upToDateReplicas because it reports based on ASG/Launch Template
		// state, not actual node versions. Be conservative and mark as not ready.
		if !nodeVersionCheckPerformed {
			// CRITICAL: Don't trust upToDateReplicas - it has the same problem as CAPI conditions!
			// The ASG can report "up-to-date" before nodes are actually replaced.
			allNodesCorrectVersion = false
			log := log.FromContext(ctx)
			log.Info("Cannot verify MachinePool node versions - workload cluster inaccessible, marking as not ready",
				"machinePool", mp.Name,
				"workloadClientError", workloadClientErr,
				"upToDateReplicas", func() string {
					if mp.Status.UpToDateReplicas != nil {
						return fmt.Sprintf("%d", *mp.Status.UpToDateReplicas)
					}
					return "nil"
				}())
		}

		// For externally-managed MachinePools (Cluster Autoscaler, Karpenter, etc.),
		// ONLY use workload cluster check. CAPI status (replicas, phase) is unreliable
		// for pools managed by external autoscalers.
		// For regular MachinePools, use both CAPI status AND workload cluster check.
		var rolloutComplete bool
		if isExternallyManaged {
			// External autoscaler: ONLY trust actual workload cluster node versions
			rolloutComplete = allNodesCorrectVersion
			logger.Info("Externally-managed MachinePool - using workload cluster check only",
				"machinePool", mp.Name,
				"allNodesCorrectVersion", allNodesCorrectVersion,
				"nodeVersionCheckPerformed", nodeVersionCheckPerformed,
				"nodesWithWrongVersion", nodesWithWrongVersion)
		} else {
			// Regular: use both CAPI status AND workload cluster check
			rolloutComplete = basicRolloutComplete && allNodesCorrectVersion
		}

		logLevel := logger.V(1)

		logFields := []interface{}{
			"name", mp.Name,
			"isExternallyManaged", isExternallyManaged,
			"ready", ready,
			"allNodesCorrectVersion", allNodesCorrectVersion,
			"nodeVersionCheckPerformed", nodeVersionCheckPerformed,
			"basicRolloutComplete", basicRolloutComplete,
			"rolloutComplete", rolloutComplete,
			"versionMatch", versionMatch,
			"desiredReplicas", desiredReplicas,
			"readyReplicas", *mp.Status.ReadyReplicas,
			"availableReplicas", *mp.Status.AvailableReplicas,
			"expectedVersion", expectedVersion,
			"mpLabelVersion", mp.Labels[ReleaseVersionLabel],
			"clusterVersion", cluster.Labels[ReleaseVersionLabel],
			"availableCondition", func() string {
				if condition := capiconditions.Get(&mp, capi.AvailableCondition); condition != nil {
					return fmt.Sprintf("status=%s,reason=%s", condition.Status, condition.Reason)
				}
				return "missing"
			}(),
		}

		// Add upToDateReplicas info if available (v1beta2 only)
		if mp.Status.UpToDateReplicas != nil {
			logFields = append(logFields, "upToDateReplicas", *mp.Status.UpToDateReplicas)
		} else {
			logFields = append(logFields, "upToDateReplicas", "not available (v1beta1?)")
		}

		// Add workload cluster check info
		if nodeVersionCheckPerformed {
			logFields = append(logFields, "workloadClusterCheck", "successful")
			if len(nodesWithWrongVersion) > 0 {
				logFields = append(logFields, "nodesWithWrongVersion", nodesWithWrongVersion)
			}
		} else if workloadClient != nil {
			logFields = append(logFields, "workloadClusterCheck", "failed")
		} else {
			logFields = append(logFields, "workloadClusterCheck", "client not available")
		}

		logLevel.Info("Checking MachinePool status", logFields...)

		// For externally-managed pools: only check workload cluster result (skip CAPI ready check)
		// For regular pools: check both ready and rolloutComplete
		var isNotReady bool
		if isExternallyManaged {
			isNotReady = !rolloutComplete
		} else {
			isNotReady = !ready || !rolloutComplete
		}

		if isNotReady {
			notReadyMPs = append(notReadyMPs, mp.Name)
		}

		if !versionMatch {
			// MachinePool version mismatch - label doesn't match cluster
			versionMismatchMPs = append(versionMismatchMPs, fmt.Sprintf("%s(label:%s!=cluster:%s)",
				mp.Name,
				mp.Labels[ReleaseVersionLabel],
				cluster.Labels[ReleaseVersionLabel]))
		}
	}

	allReady := len(notReadyMDs) == 0 && len(notReadyMPs) == 0 && len(versionMismatchMDs) == 0 && len(versionMismatchMPs) == 0

	if !allReady {
		log := log.FromContext(ctx)
		logFields := []interface{}{
			"totalMachineDeployments", len(machineDeployments.Items),
			"totalMachinePools", len(machinePools.Items),
			"notReadyMachineDeployments", notReadyMDs,
			"notReadyMachinePools", notReadyMPs,
			"versionMismatchMachineDeployments", versionMismatchMDs,
			"versionMismatchMachinePools", versionMismatchMPs,
		}

		log.Info("Worker nodes not all ready", logFields...)
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
		if isClusterUpgrading(&md) {
			upgradingMDs = append(upgradingMDs, md.Name)
		}
	}

	machinePools := &capi.MachinePoolList{}
	if err := c.List(ctx, machinePools, client.InNamespace(cluster.Namespace), client.MatchingLabels{
		capi.ClusterNameLabel: cluster.Name,
	}); err != nil {
		return false, err
	}

	var upgradingMPs []string
	for _, mp := range machinePools.Items {
		if isClusterUpgrading(&mp) {
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
