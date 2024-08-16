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
	"time"

	"github.com/giantswarm/microerror"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	capi "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
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

	err := r.Client.Get(ctx, req.NamespacedName, cluster)
	if apierrors.IsNotFound(err) {
		log.Info("Cluster no longer exists")
		return ctrl.Result{}, nil
	} else if err != nil {
		return ctrl.Result{}, microerror.Mask(err)
	}

	if _, ok := cluster.Labels["cicd.giantswarm.io/pipelinerun"]; ok {
		// ignore E2e tests
		return ctrl.Result{}, nil
	}

	// load last known transition time from annotations
	var lastKnownTransitionTime time.Time
	if annotation, ok := cluster.Annotations["giantswarm.io/last-known-cluster-upgrade-timestamp"]; ok {
		if t, err := time.Parse(time.RFC3339, annotation); err == nil {
			lastKnownTransitionTime = t
		}
	}

	// load last known upgrade release version from annotations otherwise update it
	if _, ok := cluster.Annotations["giantswarm.io/last-known-cluster-upgrade-version"]; ok {
		// last known upgrade version is set
	} else {
		err := updateLastKnownReleaseVersion(r.Client, cluster)
		if err != nil {
			return ctrl.Result{}, microerror.Mask(err)
		}
	}

	// cluster is upgrading and release version is different => send "Upgrading" event
	sendUpgradingEvent := isClusterUpgrading(cluster, capi.ReadyCondition) && isClusterReleaseVersionDifferent(cluster)
	if sendUpgradingEvent {
		r.Recorder.Event(cluster, "Normal", "Upgrading", fmt.Sprintf("Cluster %s is Upgrading from release version %s to %s", cluster.Name, cluster.Annotations["giantswarm.io/last-known-cluster-upgrade-version"], cluster.Labels["release.giantswarm.io/version"]))
		err := updateLastKnownReleaseVersion(r.Client, cluster)
		if err != nil {
			return ctrl.Result{}, microerror.Mask(err)
		}
	}

	// wait for cluster is ready the first time
	readyTransition, ok := conditionTimeStampFromReadyState(cluster, capi.ReadyCondition)
	if !ok {
		return ctrl.Result{}, nil
	}
	readyTransitionTime := readyTransition.Time.UTC()

	// ensure last known transition time is set when annotation is missing and cluster is ready when created
	if lastKnownTransitionTime.IsZero() {
		err := updateLastKnownTransitionTime(r.Client, cluster, readyTransitionTime)
		if err != nil {
			return ctrl.Result{}, microerror.Mask(err)
		}
	} else {
		sendUpgradeEvent := isClusterReady(cluster, capi.ReadyCondition) && readyTransitionTime.After(lastKnownTransitionTime) && !isClusterReleaseVersionDifferent(cluster)
		if sendUpgradeEvent {
			r.Recorder.Event(cluster, "Normal", "Upgraded", fmt.Sprintf("Cluster %s is Upgraded to release %s", cluster.Name, cluster.Labels["release.giantswarm.io/version"]))
			err := updateLastKnownTransitionTime(r.Client, cluster, readyTransitionTime)
			if err != nil {
				return ctrl.Result{}, microerror.Mask(err)
			}
		}
	}

	return ctrl.Result{}, nil
}

func updateLastKnownTransitionTime(client client.Client, cluster *capi.Cluster, transitionTime time.Time) error {
	// Update the annotation on the cluster object
	if cluster.Annotations == nil {
		cluster.Annotations = make(map[string]string)
	}
	cluster.Annotations["giantswarm.io/last-known-cluster-upgrade-timestamp"] = transitionTime.Format(time.RFC3339)

	// Update the cluster object in Kubernetes
	return client.Update(context.Background(), cluster)
}

func updateLastKnownReleaseVersion(client client.Client, cluster *capi.Cluster) error {
	// Update the annotation on the cluster object
	if cluster.Annotations == nil {
		cluster.Annotations = make(map[string]string)
	}
	cluster.Annotations["giantswarm.io/last-known-cluster-upgrade-version"] = cluster.Labels["release.giantswarm.io/version"]

	// Update the cluster object in Kubernetes
	return client.Update(context.Background(), cluster)
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		// Uncomment the following line adding a pointer to an instance of the controlled resource as an argument
		For(&capi.Cluster{}).
		Complete(r)
}
