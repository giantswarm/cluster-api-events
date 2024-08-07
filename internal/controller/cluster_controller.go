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
	"sync"
	"time"

	"github.com/giantswarm/microerror"
	events "k8s.io/api/events/v1"
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

var (
	lastKnownTransitionTime = make(map[string]time.Time)
	mutex                   sync.Mutex
)

// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
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

	readyTransition, ok := conditionTimeStamp(cluster, capi.ReadyCondition)
	if !ok {
		return ctrl.Result{}, nil
	}
	readyTransitionTime := readyTransition.Time.UTC()

	// get all events for the cluster to check if the "Upgrading" event is present within the last 30 minutes
	// estimated time for the upgrade to finish
	thirtyMinutesAgo := time.Now().Add(-30 * time.Minute).UTC()
	upgradingEventIsPresent := false

	eventList := &events.EventList{}
	err = r.Client.List(ctx, eventList, client.InNamespace(req.Namespace))
	if err != nil {
		return ctrl.Result{}, microerror.Mask(err)
	}

	for _, event := range eventList.Items {
		eventTime := event.CreationTimestamp.Time
		if eventTime.After(thirtyMinutesAgo) && event.Reason == "Upgrading" && event.Regarding.Name == cluster.Name {
			upgradingEventIsPresent = true
		}
	}

	// if cluster is upgrading and no event was sent in the last 20 minutes => send "Upgrading" event
	sendUpgradingEvent := isClusterUpgrading(cluster, capi.ReadyCondition) && !upgradingEventIsPresent
	if sendUpgradingEvent {
		r.Recorder.Event(cluster, "Normal", "Upgrading", fmt.Sprintf("Cluster %s is Upgrading", cluster.Name))
	}

	// check last known "Ready" transition time for the cluster
	if !isLastKnownTransitionTimePresent(cluster) {
		updateLastKnownTransitionTime(cluster, readyTransitionTime)
	}

	// if cluster is "Ready" (marked as true) and lastTransitionTime of type "Ready" is newer than our stored lastKnownTransition time => send "Upgrade" event
	sendUpgradeEvent := isClusterReady(cluster, capi.ReadyCondition) && readyTransitionTime.After(lastKnownTransitionTime[fmt.Sprintf("%s/%s", cluster.Namespace, cluster.Name)])
	if sendUpgradeEvent {
		r.Recorder.Event(cluster, "Normal", "Upgraded", fmt.Sprintf("Cluster %s is Upgraded", cluster.Name))
		updateLastKnownTransitionTime(cluster, readyTransitionTime)
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		// Uncomment the following line adding a pointer to an instance of the controlled resource as an argument
		For(&capi.Cluster{}).
		Complete(r)
}

func isLastKnownTransitionTimePresent(cluster *capi.Cluster) bool {
	key := fmt.Sprintf("%s/%s", cluster.Namespace, cluster.Name)
	mutex.Lock()
	defer mutex.Unlock()
	last, exists := lastKnownTransitionTime[key]
	log.Log.Info("Last known transition time", "time", last)
	return exists
}

func updateLastKnownTransitionTime(cluster *capi.Cluster, readyTransitionTime time.Time) {
	key := fmt.Sprintf("%s/%s", cluster.Namespace, cluster.Name)
	mutex.Lock()
	defer mutex.Unlock()
	lastKnownTransitionTime[key] = readyTransitionTime
}
