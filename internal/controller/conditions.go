package controller

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
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

// checkMachinePoolNodeVersions connects to workload cluster and checks node versions for a MachinePool.
// When enforceCreationTime is true, additionally requires every schedulable node to have a
// CreationTimestamp strictly after upgradeStartTime. This is necessary for releases that
// don't bump the Kubernetes version (so the kubelet check alone can't distinguish old
// nodes from new ones) and for externally-managed pools like Karpenter where CAPI status
// is unreliable.
func checkMachinePoolNodeVersions(ctx context.Context, workloadClient kubernetes.Interface, mp *capi.MachinePool, expectedVersion string, upgradeStartTime time.Time, enforceCreationTime bool) (bool, []string, error) {
	logger := log.FromContext(ctx)

	// Try primary Giant Swarm label first
	labelSelector := fmt.Sprintf("giantswarm.io/machine-pool=%s", mp.Name)

	// List all nodes using pagination to handle large clusters
	var allNodes []corev1.Node
	continueToken := ""
	for {
		nodes, err := workloadClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{
			LabelSelector: labelSelector,
			FieldSelector: "spec.unschedulable!=true",
			Limit:         100,
			Continue:      continueToken,
		})
		if err != nil {
			return false, nil, fmt.Errorf("failed to list nodes for MachinePool %s: %w", mp.Name, err)
		}
		allNodes = append(allNodes, nodes.Items...)
		if nodes.Continue == "" {
			break
		}
		continueToken = nodes.Continue
	}

	// If no nodes found with primary label, try Karpenter label as fallback
	// This handles cases where Karpenter NodePools are not configured with giantswarm.io/machine-pool label
	if len(allNodes) == 0 {
		karpenterSelector := fmt.Sprintf("karpenter.sh/nodepool=%s", mp.Name)
		logger.Info("No nodes found with giantswarm label, trying Karpenter label as fallback",
			"machinePool", mp.Name,
			"primaryLabel", labelSelector,
			"fallbackLabel", karpenterSelector)

		continueToken = ""
		for {
			nodes, err := workloadClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{
				LabelSelector: karpenterSelector,
				FieldSelector: "spec.unschedulable!=true",
				Limit:         100,
				Continue:      continueToken,
			})
			if err != nil {
				return false, nil, fmt.Errorf("failed to list nodes for MachinePool %s with Karpenter label: %w", mp.Name, err)
			}
			allNodes = append(allNodes, nodes.Items...)
			if nodes.Continue == "" {
				break
			}
			continueToken = nodes.Continue
		}

		if len(allNodes) > 0 {
			labelSelector = karpenterSelector // Update for logging
			logger.Info("Found nodes using Karpenter label fallback",
				"machinePool", mp.Name,
				"nodeCount", len(allNodes),
				"labelSelector", karpenterSelector)
		}
	}

	// If no nodes found with either label, return true since there are no nodes to upgrade.
	// This handles the case where:
	// - Karpenter NodePool has 0 nodes (no workloads scheduled, scale-to-zero, etc.)
	// - Nodes are still being provisioned (will get new version from launch template)
	// For externally-managed pools (Karpenter), this is expected behavior - future nodes
	// will be provisioned with the new configuration.
	if len(allNodes) == 0 {
		logger.Info("No nodes found for MachinePool in workload cluster - considering ready (0 nodes = 0 nodes to upgrade)",
			"machinePool", mp.Name,
			"labelSelector", labelSelector,
			"expectedVersion", expectedVersion)
		return true, nil, nil
	}

	logger.V(1).Info("Checking node versions for MachinePool",
		"machinePool", mp.Name,
		"nodeCount", len(allNodes),
		"expectedVersion", expectedVersion,
		"labelSelector", labelSelector)

	var nodesWithWrongVersion []string
	allCorrectVersion := true
	checkedNodeCount := 0

	for _, node := range allNodes {
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
			continue
		}

		// Creation-time check: catches releases that don't bump kubelet version
		// (e.g. OS image-only releases) where the kubelet check above is useless.
		if enforceCreationTime && !node.CreationTimestamp.Time.After(upgradeStartTime) {
			nodesWithWrongVersion = append(nodesWithWrongVersion,
				fmt.Sprintf("%s(created %s, before upgrade start %s)",
					node.Name,
					node.CreationTimestamp.Time.UTC().Format(time.RFC3339),
					upgradeStartTime.UTC().Format(time.RFC3339)))
			allCorrectVersion = false

			logger.Info("Node predates upgrade start - still needs to be rolled",
				"machinePool", mp.Name,
				"node", node.Name,
				"nodeCreatedAt", node.CreationTimestamp.Time,
				"upgradeStartTime", upgradeStartTime)

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
			"totalNodes", len(allNodes))
		return false, []string{"all nodes unschedulable"}, nil
	}

	logger.V(1).Info("MachinePool node version check complete",
		"machinePool", mp.Name,
		"checkedNodes", checkedNodeCount,
		"allCorrectVersion", allCorrectVersion,
		"nodesWithWrongVersion", nodesWithWrongVersion)

	return allCorrectVersion, nodesWithWrongVersion, nil
}

// checkMachineDeploymentNodeVersions connects to workload cluster and checks node versions for a MachineDeployment.
// It lists Machine objects in the management cluster belonging to the MachineDeployment, then for each Machine
// with a NodeRef, gets the corresponding node from the workload cluster and checks its kubelet version.
// When enforceCreationTime is true, additionally requires every Machine to have a CreationTimestamp
// strictly after upgradeStartTime (catches releases that don't bump the Kubernetes version).
func checkMachineDeploymentNodeVersions(ctx context.Context, mgmtClient client.Client, workloadClient kubernetes.Interface, md *capi.MachineDeployment, expectedVersion string, upgradeStartTime time.Time, enforceCreationTime bool) (bool, []string, error) {
	logger := log.FromContext(ctx)

	// List Machine objects in the management cluster belonging to this MachineDeployment
	machines := &capi.MachineList{}
	if err := mgmtClient.List(ctx, machines,
		client.InNamespace(md.Namespace),
		client.MatchingLabels{
			capi.MachineDeploymentNameLabel: md.Name,
		},
	); err != nil {
		return false, nil, fmt.Errorf("failed to list Machines for MachineDeployment %s: %w", md.Name, err)
	}

	if len(machines.Items) == 0 {
		logger.Info("No Machines found for MachineDeployment - considering ready (0 machines = 0 nodes to check)",
			"machineDeployment", md.Name,
			"expectedVersion", expectedVersion)
		return true, nil, nil
	}

	logger.V(1).Info("Checking node versions for MachineDeployment",
		"machineDeployment", md.Name,
		"machineCount", len(machines.Items),
		"expectedVersion", expectedVersion)

	var nodesWithWrongVersion []string
	allCorrectVersion := true
	checkedNodeCount := 0
	provisioningCount := 0

	for _, machine := range machines.Items {
		// Skip machines without a NodeRef (still provisioning)
		if machine.Status.NodeRef.Name == "" {
			provisioningCount++
			logger.V(1).Info("Machine has no NodeRef yet (still provisioning)",
				"machineDeployment", md.Name,
				"machine", machine.Name)
			// Be conservative: a provisioning machine means rollout is not complete
			allCorrectVersion = false
			continue
		}

		// Get the node from the workload cluster
		node, err := workloadClient.CoreV1().Nodes().Get(ctx, machine.Status.NodeRef.Name, metav1.GetOptions{})
		if err != nil {
			logger.Info("Failed to get node from workload cluster for Machine",
				"machineDeployment", md.Name,
				"machine", machine.Name,
				"nodeName", machine.Status.NodeRef.Name,
				"error", err)
			// Be conservative: if we can't verify, mark as not correct
			allCorrectVersion = false
			continue
		}

		// Skip unschedulable nodes (being drained/cordoned)
		if node.Spec.Unschedulable {
			logger.V(1).Info("Skipping unschedulable node in MachineDeployment version check",
				"machineDeployment", md.Name,
				"machine", machine.Name,
				"node", node.Name,
				"nodeVersion", node.Status.NodeInfo.KubeletVersion)
			continue
		}

		checkedNodeCount++
		nodeVersion := node.Status.NodeInfo.KubeletVersion

		if nodeVersion != expectedVersion {
			nodesWithWrongVersion = append(nodesWithWrongVersion,
				fmt.Sprintf("%s(%s!=%s)", node.Name, nodeVersion, expectedVersion))
			allCorrectVersion = false

			logger.Info("Node has wrong version",
				"machineDeployment", md.Name,
				"machine", machine.Name,
				"node", node.Name,
				"nodeVersion", nodeVersion,
				"expectedVersion", expectedVersion)

			// Limit the number of wrong version nodes we track to prevent memory bloat
			if len(nodesWithWrongVersion) >= 10 {
				logger.V(1).Info("Truncating wrong version nodes list to prevent memory usage",
					"machineDeployment", md.Name,
					"totalWrongVersionNodes", "10+")
				break
			}
			continue
		}

		// Creation-time check on the Machine (the MachineSet rolls new Machines on upgrade).
		// Catches releases that don't bump kubelet version (e.g. OS image-only).
		if enforceCreationTime && !machine.CreationTimestamp.Time.After(upgradeStartTime) {
			nodesWithWrongVersion = append(nodesWithWrongVersion,
				fmt.Sprintf("%s(machine created %s, before upgrade start %s)",
					node.Name,
					machine.CreationTimestamp.Time.UTC().Format(time.RFC3339),
					upgradeStartTime.UTC().Format(time.RFC3339)))
			allCorrectVersion = false

			logger.Info("Machine predates upgrade start - still needs to be rolled",
				"machineDeployment", md.Name,
				"machine", machine.Name,
				"node", node.Name,
				"machineCreatedAt", machine.CreationTimestamp.Time,
				"upgradeStartTime", upgradeStartTime)

			if len(nodesWithWrongVersion) >= 10 {
				logger.V(1).Info("Truncating wrong version nodes list to prevent memory usage",
					"machineDeployment", md.Name,
					"totalWrongVersionNodes", "10+")
				break
			}
		}
	}

	logger.V(1).Info("MachineDeployment node version check complete",
		"machineDeployment", md.Name,
		"totalMachines", len(machines.Items),
		"checkedNodes", checkedNodeCount,
		"provisioningMachines", provisioningCount,
		"allCorrectVersion", allCorrectVersion,
		"nodesWithWrongVersion", nodesWithWrongVersion)

	return allCorrectVersion, nodesWithWrongVersion, nil
}

// checkControlPlaneNodeVersions connects to the workload cluster and checks that each control plane
// Machine's node has a kubelet version matching its Machine's Spec.Version.
// For non-patch upgrades it also requires that at least one CP Machine was created after
// upgradeStartTime, to ensure the rollout has actually begun (CAPI conditions and machine specs
// may not be updated immediately). Patch upgrades skip this precondition because the control
// plane is not expected to roll, so no new Machine will ever appear.
func checkControlPlaneNodeVersions(ctx context.Context, mgmtClient client.Client, workloadClient kubernetes.Interface, cluster *capi.Cluster, upgradeStartTime time.Time, isPatchUpgrade bool) (bool, []string, error) {
	logger := log.FromContext(ctx)

	machines := &capi.MachineList{}
	if err := mgmtClient.List(ctx, machines,
		client.InNamespace(cluster.Namespace),
		client.MatchingLabels{
			capi.ClusterNameLabel: cluster.Name,
		},
		client.HasLabels{capi.MachineControlPlaneLabel},
	); err != nil {
		return false, nil, fmt.Errorf("failed to list control plane Machines for cluster %s: %w", cluster.Name, err)
	}

	if len(machines.Items) == 0 {
		logger.Info("No control plane Machines found - considering ready (0 machines = 0 nodes to check)",
			"cluster", cluster.Name)
		return true, nil, nil
	}

	// For minor/major upgrades, require that at least one Machine was created after the upgrade
	// started. If not, KCP hasn't started rolling yet — all machines still have the old version.
	// Patch upgrades don't roll the control plane, so this precondition would never be met.
	if !isPatchUpgrade {
		hasNewMachine := false
		for _, machine := range machines.Items {
			if machine.CreationTimestamp.Time.After(upgradeStartTime) {
				hasNewMachine = true
				break
			}
		}

		if !hasNewMachine {
			logger.Info("No control plane Machine created after upgrade start, rollout has not begun yet",
				"cluster", cluster.Name,
				"upgradeStartTime", upgradeStartTime,
				"machineCount", len(machines.Items))
			return false, []string{"no new machine since upgrade start"}, nil
		}
	}

	logger.V(1).Info("Checking node versions for control plane",
		"cluster", cluster.Name,
		"machineCount", len(machines.Items))

	var nodesWithWrongVersion []string
	allCorrectVersion := true
	checkedNodeCount := 0
	provisioningCount := 0

	for _, machine := range machines.Items {
		expectedVersion := machine.Spec.Version

		// Skip machines without a NodeRef (still provisioning)
		if machine.Status.NodeRef.Name == "" {
			provisioningCount++
			logger.V(1).Info("Control plane Machine has no NodeRef yet (still provisioning)",
				"cluster", cluster.Name,
				"machine", machine.Name)
			allCorrectVersion = false
			continue
		}

		// Get the node from the workload cluster
		node, err := workloadClient.CoreV1().Nodes().Get(ctx, machine.Status.NodeRef.Name, metav1.GetOptions{})
		if err != nil {
			logger.Info("Failed to get node from workload cluster for control plane Machine",
				"cluster", cluster.Name,
				"machine", machine.Name,
				"nodeName", machine.Status.NodeRef.Name,
				"error", err)
			allCorrectVersion = false
			continue
		}

		// Skip unschedulable nodes (being drained/cordoned)
		if node.Spec.Unschedulable {
			logger.V(1).Info("Skipping unschedulable node in control plane version check",
				"cluster", cluster.Name,
				"machine", machine.Name,
				"node", node.Name,
				"nodeVersion", node.Status.NodeInfo.KubeletVersion)
			continue
		}

		checkedNodeCount++
		nodeVersion := node.Status.NodeInfo.KubeletVersion

		if nodeVersion != expectedVersion {
			nodesWithWrongVersion = append(nodesWithWrongVersion,
				fmt.Sprintf("%s(%s!=%s)", node.Name, nodeVersion, expectedVersion))
			allCorrectVersion = false

			logger.Info("Control plane node has wrong version",
				"cluster", cluster.Name,
				"machine", machine.Name,
				"node", node.Name,
				"nodeVersion", nodeVersion,
				"expectedVersion", expectedVersion)

			if len(nodesWithWrongVersion) >= 10 {
				logger.V(1).Info("Truncating wrong version nodes list to prevent memory usage",
					"cluster", cluster.Name,
					"totalWrongVersionNodes", "10+")
				break
			}
		}
	}

	logger.V(1).Info("Control plane node version check complete",
		"cluster", cluster.Name,
		"totalMachines", len(machines.Items),
		"checkedNodes", checkedNodeCount,
		"provisioningMachines", provisioningCount,
		"allCorrectVersion", allCorrectVersion,
		"nodesWithWrongVersion", nodesWithWrongVersion)

	return allCorrectVersion, nodesWithWrongVersion, nil
}

// areAllWorkerNodesReady checks if all MachineDeployments and MachinePools are ready.
// When enforceCreationTime is true, additionally requires worker nodes/Machines to have been
// (re)created after upgradeStartTime. Callers must only set this when upgradeStartTime is
// known to be trustworthy (i.e. stamped at the actual upgrade start, not synthesized).
func areAllWorkerNodesReady(ctx context.Context, c client.Client, cluster *capi.Cluster, upgradeStartTime time.Time, enforceCreationTime bool) (bool, error) {
	machineDeployments := &capi.MachineDeploymentList{}
	if err := c.List(ctx, machineDeployments, client.InNamespace(cluster.Namespace), client.MatchingLabels{
		capi.ClusterNameLabel: cluster.Name,
	}); err != nil {
		return false, err
	}

	machinePools := &capi.MachinePoolList{}
	if err := c.List(ctx, machinePools, client.InNamespace(cluster.Namespace), client.MatchingLabels{
		capi.ClusterNameLabel: cluster.Name,
	}); err != nil {
		return false, err
	}

	// Get workload cluster client to check actual node versions
	// Needed for both MachineDeployments (to verify kubelet versions) and MachinePools (for Karpenter etc.)
	var workloadClient kubernetes.Interface
	var workloadClientErr error
	if len(machineDeployments.Items) > 0 || len(machinePools.Items) > 0 || cluster.Status.ControlPlane != nil {
		workloadClient, workloadClientErr = getWorkloadClusterClient(ctx, c, cluster)
		if workloadClientErr != nil {
			log := log.FromContext(ctx)
			log.V(1).Info("Failed to get workload cluster client, will rely on CAPI status only",
				"cluster", cluster.Name,
				"error", workloadClientErr)
			workloadClient = nil
		}
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

		// Get expected Kubernetes version from MachineDeployment spec
		expectedVersion := md.Spec.Template.Spec.Version

		// Check actual node versions in the workload cluster
		allNodesCorrectVersion := true
		var nodesWithWrongVersion []string
		nodeVersionCheckPerformed := false

		if workloadClient != nil && expectedVersion != "" {
			var err error
			allNodesCorrectVersion, nodesWithWrongVersion, err = checkMachineDeploymentNodeVersions(ctx, c, workloadClient, &md, expectedVersion, upgradeStartTime, enforceCreationTime)
			if err != nil {
				logger := log.FromContext(ctx)
				logger.V(1).Info("Failed to check node versions in workload cluster for MachineDeployment",
					"machineDeployment", md.Name,
					"error", err)
				nodeVersionCheckPerformed = false
			} else {
				nodeVersionCheckPerformed = true
			}
		}

		// If workload cluster check was not performed, be conservative and mark as not ready
		if !nodeVersionCheckPerformed {
			allNodesCorrectVersion = false
		}

		// Combine CAPI conditions with actual node version checks
		rolloutComplete := basicRolloutComplete && machinesUpToDate && allNodesCorrectVersion

		logger := log.FromContext(ctx)
		logFields := []any{
			"name", md.Name,
			"ready", ready,
			"machinesUpToDate", machinesUpToDate,
			"allNodesCorrectVersion", allNodesCorrectVersion,
			"nodeVersionCheckPerformed", nodeVersionCheckPerformed,
			"basicRolloutComplete", basicRolloutComplete,
			"rolloutComplete", rolloutComplete,
			"versionMatch", versionMatch,
			"expectedVersion", expectedVersion,
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
			}(),
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

		logger.V(1).Info("Checking MachineDeployment status", logFields...)

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
			allNodesCorrectVersion, nodesWithWrongVersion, err = checkMachinePoolNodeVersions(ctx, workloadClient, &mp, expectedVersion, upgradeStartTime, enforceCreationTime)
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

// flatcarImageLookupFormatRegex matches imageLookupFormat strings rendered by
// the giantswarm/cluster-aws Helm chart, e.g.
//
//	flatcar-stable-4459.2.1-kube-1.33.6-tooling-1.26.2-gs
//	flatcar-stable-4459.2.1-kube-1.33.6-tooling-1.26.2-arm64-gs
//
// Capture groups: Flatcar OS version, Kubernetes version.
var flatcarImageLookupFormatRegex = regexp.MustCompile(`^flatcar-[a-z]+-([0-9]+\.[0-9]+\.[0-9]+)-kube-([0-9]+\.[0-9]+\.[0-9]+)-tooling-[0-9]+\.[0-9]+\.[0-9]+(?:-[a-z0-9]+)?-gs$`)

// parseFlatcarImageLookupFormat extracts the Flatcar OS version and Kubernetes
// version from a chart-rendered imageLookupFormat. Returns ok=false if the
// string doesn't match the expected pattern.
func parseFlatcarImageLookupFormat(s string) (flatcarVersion, kubeVersion string, ok bool) {
	m := flatcarImageLookupFormatRegex.FindStringSubmatch(s)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// willUpgradeRollWorkers reports whether the in-flight upgrade is expected to
// roll existing worker Nodes. It compares the expected Flatcar+kubelet
// versions (derived from each AWSMachinePool's launch template) against what
// the observed Nodes actually run. If any worker MachinePool has a node whose
// kubelet or OS image differs from expected, a roll is required.
//
// Returns true (conservative) when state is uncertain:
//   - workload cluster client unavailable
//   - non-CAPA infrastructure kind (e.g. KarpenterMachinePool)
//   - hard-set ami.id on the AWSMachinePool (expected OS not derivable here)
//   - missing/unparseable imageLookupFormat
//   - any error fetching nodes or AWSMachinePools
//
// This is called once at "Upgrading" emit time so the same decision drives both
// the customer-facing event reason and the completion-time creation-time check.
func willUpgradeRollWorkers(ctx context.Context, c client.Client, cluster *capi.Cluster) bool {
	logger := log.FromContext(ctx)

	machinePools := &capi.MachinePoolList{}
	if err := c.List(ctx, machinePools, client.InNamespace(cluster.Namespace), client.MatchingLabels{
		capi.ClusterNameLabel: cluster.Name,
	}); err != nil {
		logger.Info("willUpgradeRollWorkers: failed to list MachinePools, assuming node roll", "error", err)
		return true
	}

	machineDeployments := &capi.MachineDeploymentList{}
	if err := c.List(ctx, machineDeployments, client.InNamespace(cluster.Namespace), client.MatchingLabels{
		capi.ClusterNameLabel: cluster.Name,
	}); err != nil {
		logger.Info("willUpgradeRollWorkers: failed to list MachineDeployments, assuming node roll", "error", err)
		return true
	}

	// MachineDeployments are rolled by CAPI on any spec change, so we can't
	// reliably predict "no roll" for them from a snapshot. Be conservative.
	if len(machineDeployments.Items) > 0 {
		logger.V(1).Info("willUpgradeRollWorkers: MachineDeployments present, assuming node roll",
			"machineDeploymentCount", len(machineDeployments.Items))
		return true
	}

	// No workers at all: nothing to roll.
	if len(machinePools.Items) == 0 {
		logger.V(1).Info("willUpgradeRollWorkers: no worker MachinePools, no node roll required")
		return false
	}

	workloadClient, err := getWorkloadClusterClient(ctx, c, cluster)
	if err != nil {
		logger.Info("willUpgradeRollWorkers: workload client unavailable, assuming node roll", "error", err)
		return true
	}

	inspectedAny := false
	for _, mp := range machinePools.Items {
		infraRef := mp.Spec.Template.Spec.InfrastructureRef

		// Non-AWSMachinePool infra (e.g. KarpenterMachinePool) doesn't have a
		// CAPA launch template we can introspect uniformly. If the pool has no
		// nodes, there's nothing to roll for it — skip and check other pools.
		// If it does have nodes, fall back conservatively.
		if infraRef.Kind != "AWSMachinePool" {
			poolReplicas := int32(0)
			if mp.Status.Replicas != nil {
				poolReplicas = *mp.Status.Replicas
			}
			if poolReplicas == 0 {
				logger.V(1).Info("willUpgradeRollWorkers: non-AWSMachinePool pool is empty, skipping",
					"machinePool", mp.Name, "infraKind", infraRef.Kind)
				continue
			}
			logger.Info("willUpgradeRollWorkers: non-AWSMachinePool pool has nodes, assuming node roll",
				"machinePool", mp.Name, "infraKind", infraRef.Kind, "replicas", poolReplicas)
			return true
		}

		awsmp := &unstructured.Unstructured{}
		awsmp.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "infrastructure.cluster.x-k8s.io",
			Version: "v1beta2",
			Kind:    "AWSMachinePool",
		})
		if err := c.Get(ctx, types.NamespacedName{Namespace: mp.Namespace, Name: infraRef.Name}, awsmp); err != nil {
			logger.Info("willUpgradeRollWorkers: failed to get AWSMachinePool, assuming node roll",
				"machinePool", mp.Name, "awsmp", infraRef.Name, "error", err)
			return true
		}

		// Hard-set AMI: expected OS isn't derivable without an AWS API call.
		amiID, _, _ := unstructured.NestedString(awsmp.Object, "spec", "awsLaunchTemplate", "ami", "id")
		if amiID != "" {
			logger.Info("willUpgradeRollWorkers: AWSMachinePool uses hard-set ami.id, assuming node roll",
				"machinePool", mp.Name, "amiID", amiID)
			return true
		}

		imageLookupFormat, _, _ := unstructured.NestedString(awsmp.Object, "spec", "awsLaunchTemplate", "imageLookupFormat")
		expectedFlatcar, expectedKube, parseOK := parseFlatcarImageLookupFormat(imageLookupFormat)
		if !parseOK {
			logger.Info("willUpgradeRollWorkers: unparseable imageLookupFormat, assuming node roll",
				"machinePool", mp.Name, "imageLookupFormat", imageLookupFormat)
			return true
		}

		// Note: we intentionally do not compare Machine.Spec.Bootstrap.DataSecretName
		// here. For MachinePool-owned Machines CAPI doesn't populate that field
		// per-Machine (the secret reference lives on the MachinePool spec only),
		// so the comparison would always trigger a false positive.
		// If a chart bump changes the user-data hash, CAPA refreshes the ASG
		// instances; the in-flight roll is already caught by the cluster's
		// RollingOut condition (clusterNotRollingOut guard in the completion check).

		nodes, err := listNodesForMachinePool(ctx, workloadClient, mp.Name)
		if err != nil {
			logger.Info("willUpgradeRollWorkers: failed to list nodes, assuming node roll",
				"machinePool", mp.Name, "error", err)
			return true
		}
		if len(nodes) == 0 {
			logger.V(1).Info("willUpgradeRollWorkers: pool has no nodes, no roll needed for it",
				"machinePool", mp.Name)
			continue
		}

		inspectedAny = true
		for _, node := range nodes {
			observedKubelet := node.Status.NodeInfo.KubeletVersion
			observedOSImage := node.Status.NodeInfo.OSImage
			kubeletOK := normalizeKubeletVersion(observedKubelet) == "v"+expectedKube
			osOK := strings.Contains(observedOSImage, expectedFlatcar)
			if !kubeletOK || !osOK {
				logger.Info("willUpgradeRollWorkers: node would need to roll",
					"machinePool", mp.Name, "node", node.Name,
					"observedKubelet", observedKubelet, "expectedKubelet", "v"+expectedKube,
					"observedOSImage", observedOSImage, "expectedFlatcar", expectedFlatcar)
				return true
			}
		}
	}

	if !inspectedAny {
		logger.V(1).Info("willUpgradeRollWorkers: all worker pools empty, no node roll required")
		return false
	}

	logger.Info("willUpgradeRollWorkers: all worker nodes already at expected kubelet+Flatcar versions, no node roll required",
		"cluster", cluster.Name)
	return false
}

// normalizeKubeletVersion prepends "v" if missing so a node's kubeletVersion
// can be compared against an unprefixed semver parsed from imageLookupFormat.
func normalizeKubeletVersion(v string) string {
	if v == "" || strings.HasPrefix(v, "v") {
		return v
	}
	return "v" + v
}

// listNodesForMachinePool returns all nodes belonging to the given MachinePool.
// Tries the giantswarm machine-pool label first, then falls back to the
// Karpenter nodepool label. Schedulable-only filtering is intentionally omitted:
// we want to inspect every node that exists for the pool.
func listNodesForMachinePool(ctx context.Context, workloadClient kubernetes.Interface, mpName string) ([]corev1.Node, error) {
	primary := fmt.Sprintf("giantswarm.io/machine-pool=%s", mpName)
	nodes, err := workloadClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: primary})
	if err != nil {
		return nil, err
	}
	if len(nodes.Items) > 0 {
		return nodes.Items, nil
	}
	fallback := fmt.Sprintf("karpenter.sh/nodepool=%s", mpName)
	nodes, err = workloadClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: fallback})
	if err != nil {
		return nil, err
	}
	return nodes.Items, nil
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
