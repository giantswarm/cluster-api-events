package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	capi "sigs.k8s.io/cluster-api/api/v1beta1"
	capiexp "sigs.k8s.io/cluster-api/exp/api/v1beta1"
	capiconditions "sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

func isClusterReady(object capiconditions.Getter, condition capi.ConditionType) bool {
	return capiconditions.IsTrue(object, condition)
}

func isClusterUpgrading(object capiconditions.Getter, condition capi.ConditionType) bool {
	return capiconditions.IsFalse(object, condition) && capiconditions.GetReason(object, condition) == "RollingUpdateInProgress"
}

func conditionTimeStampFromReadyState(object capiconditions.Getter, condition capi.ConditionType) (*metav1.Time, bool) {
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
func checkMachinePoolNodeVersions(ctx context.Context, workloadClient kubernetes.Interface, mp *capiexp.MachinePool, expectedVersion string) (bool, []string, error) {
	// List nodes with minimal fields to reduce memory usage
	nodes, err := workloadClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("giantswarm.io/machine-pool=%s", mp.Name),
		// Limit fields to only what we need to reduce memory usage
		FieldSelector: "spec.unschedulable!=true",
		// Limit the number of nodes returned to prevent memory issues
		Limit: 100,
	})
	if err != nil {
		return false, nil, fmt.Errorf("failed to list nodes for MachinePool %s: %w", mp.Name, err)
	}

	var nodesWithWrongVersion []string
	allCorrectVersion := true

	for _, node := range nodes.Items {
		// Skip nodes being drained/cordoned (unschedulable) - should be filtered by FieldSelector but double-check
		if node.Spec.Unschedulable {
			log := log.FromContext(ctx)
			log.V(1).Info("Skipping unschedulable node in MachinePool version check",
				"machinePool", mp.Name,
				"node", node.Name,
				"nodeVersion", node.Status.NodeInfo.KubeletVersion)
			continue
		}

		// Check kubelet version
		if node.Status.NodeInfo.KubeletVersion != expectedVersion {
			nodesWithWrongVersion = append(nodesWithWrongVersion,
				fmt.Sprintf("%s(%s!=%s)", node.Name, node.Status.NodeInfo.KubeletVersion, expectedVersion))
			allCorrectVersion = false

			// Limit the number of wrong version nodes we track to prevent memory bloat
			if len(nodesWithWrongVersion) >= 10 {
				log := log.FromContext(ctx)
				log.V(1).Info("Truncating wrong version nodes list to prevent memory usage",
					"machinePool", mp.Name,
					"totalWrongVersionNodes", "10+")
				break
			}
		}
	}

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
		ready := isClusterReady(&md, capi.ReadyCondition)
		// Check version match - MachineDeployment label should match cluster version
		// This indicates the MachineDeployment is configured for the right release
		versionMatch := md.Labels[ReleaseVersionLabel] == cluster.Labels[ReleaseVersionLabel]

		var desiredReplicas int32
		if md.Spec.Replicas != nil {
			desiredReplicas = *md.Spec.Replicas
		}

		// For MachineDeployments, check both basic status and individual machine versions
		basicRolloutComplete := desiredReplicas == md.Status.UpdatedReplicas &&
			desiredReplicas == md.Status.ReadyReplicas &&
			desiredReplicas == md.Status.AvailableReplicas

		// Check individual machines for version consistency (MachineDeployments create Machine resources)
		allMachinesCorrectVersion := true
		var expectedVersion string
		if md.Spec.Template.Spec.Version != nil {
			expectedVersion = *md.Spec.Template.Spec.Version
		} else {
			expectedVersion = cluster.Labels[ReleaseVersionLabel]
		}

		machines := &capi.MachineList{}
		if err := c.List(ctx, machines, client.InNamespace(cluster.Namespace), client.MatchingLabels{
			capi.ClusterNameLabel: cluster.Name,
		}); err != nil {
			log := log.FromContext(ctx)
			log.Error(err, "Failed to list machines for MachineDeployment", "machineDeployment", md.Name)
			allMachinesCorrectVersion = false
		} else {
			var machinesWithWrongVersion []string

			// Filter machines belonging to this MachineDeployment
			for _, machine := range machines.Items {
				// Check if machine belongs to this MachineDeployment via OwnerReferences
				belongsToMD := false
				for _, ownerRef := range machine.OwnerReferences {
					if ownerRef.Kind == "MachineSet" {
						// Get the MachineSet to check if it belongs to our MachineDeployment
						machineSet := &capi.MachineSet{}
						if err := c.Get(ctx, client.ObjectKey{
							Namespace: machine.Namespace,
							Name:      ownerRef.Name,
						}, machineSet); err == nil {
							for _, msOwnerRef := range machineSet.OwnerReferences {
								if msOwnerRef.Kind == "MachineDeployment" && msOwnerRef.Name == md.Name {
									belongsToMD = true
									break
								}
							}
						}
						break
					}
				}

				if !belongsToMD {
					continue
				}

				// Skip machines being deleted
				if !machine.DeletionTimestamp.IsZero() {
					continue
				}

				// Check machine version
				if machine.Spec.Version != nil && *machine.Spec.Version != expectedVersion {
					machinesWithWrongVersion = append(machinesWithWrongVersion,
						fmt.Sprintf("%s(%s!=%s)", machine.Name, *machine.Spec.Version, expectedVersion))
					allMachinesCorrectVersion = false
				}
			}

			if len(machinesWithWrongVersion) > 0 {
				log := log.FromContext(ctx)
				log.V(1).Info("MachineDeployment has machines with incorrect versions",
					"machineDeployment", md.Name,
					"expectedVersion", expectedVersion,
					"machinesWithWrongVersion", machinesWithWrongVersion)
			}
		}

		rolloutComplete := basicRolloutComplete && allMachinesCorrectVersion

		log := log.FromContext(ctx)
		log.V(1).Info("Checking MachineDeployment status",
			"name", md.Name,
			"ready", ready,
			"basicRolloutComplete", basicRolloutComplete,
			"allMachinesCorrectVersion", allMachinesCorrectVersion,
			"rolloutComplete", rolloutComplete,
			"versionMatch", versionMatch,
			"desiredReplicas", desiredReplicas,
			"updatedReplicas", md.Status.UpdatedReplicas,
			"readyReplicas", md.Status.ReadyReplicas,
			"availableReplicas", md.Status.AvailableReplicas,
			"expectedVersion", expectedVersion,
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
			// MachineDeployment version mismatch - label doesn't match cluster
			versionMismatchMDs = append(versionMismatchMDs, fmt.Sprintf("%s(label:%s!=cluster:%s)",
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

	// Get workload cluster client for MachinePool node version checking
	var workloadClient kubernetes.Interface
	var workloadClientErr error
	var isManagementCluster bool

	// Only try to connect if we have MachinePools to check
	if len(machinePools.Items) > 0 {
		workloadClient, workloadClientErr = getWorkloadClusterClient(ctx, c, cluster)
		if workloadClientErr != nil {
			// Check if this is a management cluster self-reference
			if workloadClientErr.Error() == "management cluster self-reference detected - skipping to prevent self-connection" {
				// For management cluster, create client from current config
				currentConfig := ctrl.GetConfigOrDie()
				clientset, err := kubernetes.NewForConfig(currentConfig)
				if err != nil {
					log := log.FromContext(ctx)
					log.V(1).Info("Failed to create management cluster client, falling back to basic MachinePool checking",
						"cluster", cluster.Name,
						"error", err)
					workloadClient = nil
				} else {
					workloadClient = clientset
					isManagementCluster = true
					log := log.FromContext(ctx)
					log.V(1).Info("Using management cluster client for node version checking",
						"cluster", cluster.Name)
				}
			} else {
				log := log.FromContext(ctx)
				log.V(1).Info("Failed to get workload cluster client, falling back to basic MachinePool checking",
					"cluster", cluster.Name,
					"error", workloadClientErr)
				// Set to nil to ensure fallback behavior
				workloadClient = nil
			}
		}
	}

	var notReadyMPs []string
	var versionMismatchMPs []string
	for _, mp := range machinePools.Items {
		ready := isClusterReady(&mp, capi.ReadyCondition)
		// Check version match - MachinePool label should match cluster version
		// Note: mp.Spec.Template.Spec.Version uses Kubernetes version format (v1.31.9)
		// while labels use release version format (31.0.0), so we can't compare them directly
		versionMatch := mp.Labels[ReleaseVersionLabel] == cluster.Labels[ReleaseVersionLabel]

		var desiredReplicas int32
		if mp.Spec.Replicas != nil {
			desiredReplicas = *mp.Spec.Replicas
		}

		// Check basic readiness first
		basicRolloutComplete := desiredReplicas == mp.Status.ReadyReplicas &&
			desiredReplicas == mp.Status.AvailableReplicas

		var expectedVersion string
		if mp.Spec.Template.Spec.Version != nil {
			expectedVersion = *mp.Spec.Template.Spec.Version
		} else {
			expectedVersion = cluster.Labels[ReleaseVersionLabel]
		}

		// Try to check actual node versions in workload cluster or management cluster
		allNodesCorrectVersion := true
		var nodesWithWrongVersion []string

		if workloadClient != nil {
			var err error
			allNodesCorrectVersion, nodesWithWrongVersion, err = checkMachinePoolNodeVersions(ctx, workloadClient, &mp, expectedVersion)
			if err != nil {
				log := log.FromContext(ctx)
				if isManagementCluster {
					log.V(1).Info("Failed to check node versions in management cluster for MachinePool",
						"machinePool", mp.Name,
						"error", err)
				} else {
					log.V(1).Info("Failed to check node versions in workload cluster for MachinePool",
						"machinePool", mp.Name,
						"error", err)
				}
				// Fall back to basic checking if node access fails
				allNodesCorrectVersion = true
			}
		}

		rolloutComplete := basicRolloutComplete && allNodesCorrectVersion

		log := log.FromContext(ctx)
		logLevel := log.V(1)

		logFields := []interface{}{
			"name", mp.Name,
			"ready", ready,
			"basicRolloutComplete", basicRolloutComplete,
			"rolloutComplete", rolloutComplete,
			"versionMatch", versionMatch,
			"desiredReplicas", desiredReplicas,
			"readyReplicas", mp.Status.ReadyReplicas,
			"availableReplicas", mp.Status.AvailableReplicas,
			"nodeRefsCount", len(mp.Status.NodeRefs),
			"expectedVersion", expectedVersion,
			"mpLabelVersion", mp.Labels[ReleaseVersionLabel],
			"clusterVersion", cluster.Labels[ReleaseVersionLabel],
			"readyCondition", func() string {
				if condition := capiconditions.Get(&mp, capi.ReadyCondition); condition != nil {
					return fmt.Sprintf("status=%s,reason=%s", condition.Status, condition.Reason)
				}
				return "missing"
			}(),
		}

		if workloadClient != nil {
			logFields = append(logFields,
				"allNodesCorrectVersion", allNodesCorrectVersion)
			if isManagementCluster {
				logFields = append(logFields, "clusterAccess", "management cluster (current)")
			} else {
				logFields = append(logFields, "workloadClusterAccess", "successful")
			}
			if len(nodesWithWrongVersion) > 0 {
				logFields = append(logFields, "nodesWithWrongVersion", nodesWithWrongVersion)
			}
		} else {
			if isManagementCluster {
				logFields = append(logFields,
					"clusterAccess", "management cluster failed",
					"fallbackMode", "basic status checking only")
			} else {
				logFields = append(logFields,
					"workloadClusterAccess", "failed",
					"fallbackMode", "basic status checking only")
			}
		}

		logLevel.Info("Checking MachinePool status", logFields...)

		if !ready || !rolloutComplete {
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

		if workloadClient != nil && workloadClientErr == nil {
			logFields = append(logFields, "machinePoolVersionChecking", "workload cluster node inspection")
		} else {
			logFields = append(logFields, "machinePoolVersionChecking", "basic status only (workload cluster access failed)")
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
