/*
Copyright 2024.

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

package controller

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/giantswarm/microerror"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	capi "sigs.k8s.io/cluster-api/api/v1beta1"
	capiexp "sigs.k8s.io/cluster-api/exp/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	// Labels
	ReleaseVersionLabel = "release.giantswarm.io/version"
	PipelineRunLabel    = "cicd.giantswarm.io/pipelinerun"

	// Annotations
	LastKnownUpgradeVersionAnnotation   = "giantswarm.io/last-known-cluster-upgrade-version"
	LastKnownUpgradeTimestampAnnotation = "giantswarm.io/last-known-cluster-upgrade-timestamp"
	ClusterUpgradingAnnotation          = "giantswarm.io/cluster-upgrading"
	EmittedEventsAnnotation             = "giantswarm.io/emitted-upgrade-events"
)

// ClusterReconciler reconciles a Cluster object
type ClusterReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machinedeployments,verbs=get;list;watch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machinedeployments/status,verbs=get
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machinepools,verbs=get;list;watch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machinepools/status,verbs=get
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machines,verbs=get;list;watch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machines/status,verbs=get
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machinesets,verbs=get;list;watch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machinesets/status,verbs=get
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO: Modify the Reconcile function to compare the state specified by
// the Cluster object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.18.4/pkg/reconcile
func (r *ClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	cluster := &capi.Cluster{}

	err := r.Get(ctx, req.NamespacedName, cluster)
	if apierrors.IsNotFound(err) {
		log.Info("Cluster no longer exists")
		return ctrl.Result{}, nil
	} else if err != nil {
		return ctrl.Result{}, microerror.Mask(err)
	}

	if _, ok := cluster.Labels[PipelineRunLabel]; ok {
		// ignore E2e tests
		return ctrl.Result{}, nil
	}

	if _, ok := cluster.Labels[ReleaseVersionLabel]; !ok {
		// ignore cluster which have no release version yet
		log.Info("Cluster has no release version yet")
		return ctrl.Result{}, nil
	}

	// load last known transition time from annotations
	var lastKnownTransitionTime time.Time
	if annotation, ok := cluster.Annotations[LastKnownUpgradeTimestampAnnotation]; ok {
		if t, err := time.Parse(time.RFC3339, annotation); err == nil {
			lastKnownTransitionTime = t
		}
	}

	// Check if we need to initialize any missing annotations and batch them into a single update
	needsAnnotationUpdate := false
	missingUpgradeVersion := false
	missingUpgrading := false

	if _, ok := cluster.Annotations[LastKnownUpgradeVersionAnnotation]; !ok {
		needsAnnotationUpdate = true
		missingUpgradeVersion = true
	}

	if _, ok := cluster.Annotations[ClusterUpgradingAnnotation]; !ok {
		needsAnnotationUpdate = true
		missingUpgrading = true
	}

	// Batch update missing annotations in a single call to reduce conflicts
	if needsAnnotationUpdate {
		// Check if this is a newly created cluster (creation time within last 5 minutes)
		isNewCluster := time.Since(cluster.CreationTimestamp.Time) < 5*time.Minute

		if isNewCluster {
			log.Info("Detected new cluster, initializing upgrade annotations",
				"age", time.Since(cluster.CreationTimestamp.Time),
				"missingAnnotations", []string{
					func() string {
						if missingUpgradeVersion {
							return "upgrade-version"
						} else {
							return ""
						}
					}(),
					func() string {
						if missingUpgrading {
							return "upgrading"
						} else {
							return ""
						}
					}(),
				})

			// For new clusters, add a small delay to let other controllers settle
			// and use more aggressive retry settings
			time.Sleep(100 * time.Millisecond)
		}

		err := updateClusterAnnotations(r.Client, cluster, func(c *capi.Cluster) {
			if missingUpgradeVersion {
				c.Annotations[LastKnownUpgradeVersionAnnotation] = c.Labels[ReleaseVersionLabel]
			}
			if missingUpgrading {
				c.Annotations[ClusterUpgradingAnnotation] = "false"
			}
		})
		if err != nil {
			if isNewCluster {
				log.Info("Conflict during new cluster annotation setup, will retry on next reconciliation", "error", err)
				// For new clusters, don't fail the reconciliation on conflicts - just retry later
				return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
			}
			return ctrl.Result{}, microerror.Mask(err)
		}
		// Refetch cluster after update to ensure we have the latest version
		if err := r.Get(ctx, req.NamespacedName, cluster); err != nil {
			return ctrl.Result{}, microerror.Mask(err)
		}
	}

	// cluster is upgrading and release version is different => send "Upgrading" event
	workerNodesUpgrading, err := areAnyWorkerNodesUpgrading(ctx, r.Client, cluster)
	if err != nil {
		log.Error(err, "Failed to check worker node upgrade status")
		return ctrl.Result{}, microerror.Mask(err)
	}

	controlPlaneUpgrading := isClusterUpgrading(cluster, capi.ReadyCondition)
	releaseVersionDifferent := isClusterReleaseVersionDifferent(cluster)

	if controlPlaneUpgrading || workerNodesUpgrading || releaseVersionDifferent {
		log.Info("Cluster upgrade status check",
			"controlPlaneUpgrading", controlPlaneUpgrading,
			"workerNodesUpgrading", workerNodesUpgrading,
			"releaseVersionDifferent", releaseVersionDifferent,
			"currentVersion", cluster.Labels[ReleaseVersionLabel],
			"lastKnownVersion", cluster.Annotations[LastKnownUpgradeVersionAnnotation],
			"clusterUpgrading", cluster.Annotations[ClusterUpgradingAnnotation])
	}

	sendUpgradingEvent := (controlPlaneUpgrading || workerNodesUpgrading) && releaseVersionDifferent
	if sendUpgradingEvent {
		log.Info("Cluster upgrade started",
			"fromVersion", cluster.Annotations[LastKnownUpgradeVersionAnnotation],
			"toVersion", cluster.Labels[ReleaseVersionLabel])
		r.Recorder.Event(cluster, "Normal", "Upgrading", fmt.Sprintf("from release %s to %s", cluster.Annotations[LastKnownUpgradeVersionAnnotation], cluster.Labels[ReleaseVersionLabel]))

		// Update both version and timestamp when starting upgrade
		err := updateClusterAnnotations(r.Client, cluster, func(c *capi.Cluster) {
			c.Annotations[LastKnownUpgradeVersionAnnotation] = c.Labels[ReleaseVersionLabel]
			c.Annotations[ClusterUpgradingAnnotation] = "true"
			// Set timestamp to current ready time to prevent it being reset
			if readyTransition, ok := conditionTimeStampFromReadyState(c, capi.ReadyCondition); ok {
				c.Annotations[LastKnownUpgradeTimestampAnnotation] = readyTransition.UTC().Format(time.RFC3339)
			}
			// Clear emitted events when starting a new upgrade
			delete(c.Annotations, EmittedEventsAnnotation)
		})
		if err != nil {
			return ctrl.Result{}, microerror.Mask(err)
		}
	}

	// wait for cluster is ready the first time
	readyTransition, ok := conditionTimeStampFromReadyState(cluster, capi.ReadyCondition)
	if !ok {
		log.V(1).Info("Cluster control plane not ready yet, waiting...")
		return ctrl.Result{}, nil
	}
	readyTransitionTime := readyTransition.UTC()

	// ensure last known transition time is set when annotation is missing and cluster is ready when created
	if lastKnownTransitionTime.IsZero() {
		log.Info("Initializing cluster upgrade timestamp", "readyTime", readyTransitionTime)
		// Preserve current upgrading state when initializing timestamp
		isUpgrading := cluster.Annotations[ClusterUpgradingAnnotation] == "true"
		err := updateLastKnownTransitionTime(r.Client, cluster, readyTransitionTime, isUpgrading)
		if err != nil {
			return ctrl.Result{}, microerror.Mask(err)
		}
	} else {
		isCurrentlyUpgrading := cluster.Annotations[ClusterUpgradingAnnotation] == "true"

		// Only check worker nodes if we're actually upgrading
		if isCurrentlyUpgrading {
			allWorkerNodesReady, err := areAllWorkerNodesReady(ctx, r.Client, cluster)
			if err != nil {
				log.Error(err, "Failed to check worker node ready status")
				return ctrl.Result{}, microerror.Mask(err)
			}

			controlPlaneReady := isClusterReady(cluster, capi.ReadyCondition)
			timeProgressed := readyTransitionTime.After(lastKnownTransitionTime)
			versionsMatch := !isClusterReleaseVersionDifferent(cluster)

			log.Info("Upgrade in progress - checking completion criteria",
				"controlPlaneReady", controlPlaneReady,
				"allWorkerNodesReady", allWorkerNodesReady,
				"timeProgressed", timeProgressed,
				"versionsMatch", versionsMatch,
				"lastTransitionTime", lastKnownTransitionTime,
				"currentReadyTime", readyTransitionTime)

			// Control plane completed event (only sent once)
			controlPlaneEventSent := cluster.Annotations[EmittedEventsAnnotation] == "UpgradedControlPlane"

			if controlPlaneReady && timeProgressed && versionsMatch && !controlPlaneEventSent {
				log.Info("Control plane upgraded")
				r.Recorder.Event(cluster, "Normal", "UpgradedControlPlane",
					fmt.Sprintf("to release %s", cluster.Labels[ReleaseVersionLabel]))

				// Mark event as sent
				err := updateClusterAnnotations(r.Client, cluster, func(c *capi.Cluster) {
					c.Annotations[EmittedEventsAnnotation] = "UpgradedControlPlane"
				})
				if err != nil {
					log.Error(err, "Failed to update control plane event annotation")
				}
			}

			// Only send upgrade complete event when both control plane AND worker nodes are ready
			sendUpgradeEvent := controlPlaneReady &&
				allWorkerNodesReady &&
				timeProgressed &&
				versionsMatch

			if sendUpgradeEvent {
				duration := readyTransitionTime.Sub(lastKnownTransitionTime).Round(time.Second)
				log.Info("Cluster upgrade completed successfully",
					"version", cluster.Labels[ReleaseVersionLabel],
					"duration", duration)
				r.Recorder.Event(cluster, "Normal", "Upgraded", fmt.Sprintf("to release %s in %s", cluster.Labels[ReleaseVersionLabel], duration))
				err := updateLastKnownTransitionTime(r.Client, cluster, readyTransitionTime, false)
				if err != nil {
					return ctrl.Result{}, microerror.Mask(err)
				}
			} else {
				// Log why upgrade is not yet complete
				reasons := []string{}
				if !controlPlaneReady {
					reasons = append(reasons, "control plane not ready")
				}
				if !allWorkerNodesReady {
					reasons = append(reasons, "worker nodes not ready")
				}
				if !timeProgressed {
					reasons = append(reasons, "time not progressed")
				}
				if !versionsMatch {
					reasons = append(reasons, "versions don't match")
				}
				log.Info("Upgrade still in progress", "waitingFor", reasons)
			}
		}
	}

	return ctrl.Result{}, nil
}

func updateClusterAnnotations(client client.Client, cluster *capi.Cluster, modifyFunc func(*capi.Cluster)) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latestCluster := &capi.Cluster{}
		if err := client.Get(context.Background(), types.NamespacedName{
			Name:      cluster.Name,
			Namespace: cluster.Namespace,
		}, latestCluster); err != nil {
			return err
		}

		if latestCluster.Annotations == nil {
			latestCluster.Annotations = make(map[string]string)
		}

		modifyFunc(latestCluster)

		if err := client.Update(context.Background(), latestCluster); err != nil {
			if !apierrors.IsConflict(err) {
				return err
			}
			return err
		}
		return nil
	})
}

func updateLastKnownTransitionTime(client client.Client, cluster *capi.Cluster, transitionTime time.Time, isUpgrading bool) error {
	return updateClusterAnnotations(client, cluster, func(c *capi.Cluster) {
		c.Annotations[LastKnownUpgradeTimestampAnnotation] = transitionTime.Format(time.RFC3339)
		c.Annotations[ClusterUpgradingAnnotation] = strconv.FormatBool(isUpgrading)

		// Clear emitted events when upgrade completes
		if !isUpgrading {
			delete(c.Annotations, EmittedEventsAnnotation)
		}
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&capi.Cluster{}).
		Watches(
			&capi.MachineDeployment{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
				md := obj.(*capi.MachineDeployment)
				if clusterName, ok := md.Labels[capi.ClusterNameLabel]; ok {
					return []reconcile.Request{
						{
							NamespacedName: types.NamespacedName{
								Name:      clusterName,
								Namespace: md.Namespace,
							},
						},
					}
				}
				return []reconcile.Request{}
			}),
		).
		Watches(
			&capiexp.MachinePool{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
				mp := obj.(*capiexp.MachinePool)
				if clusterName, ok := mp.Labels[capi.ClusterNameLabel]; ok {
					return []reconcile.Request{
						{
							NamespacedName: types.NamespacedName{
								Name:      clusterName,
								Namespace: mp.Namespace,
							},
						},
					}
				}
				return []reconcile.Request{}
			}),
		).
		Complete(r)
}
