/*
Copyright 2021 Red Hat OpenShift Data Foundation.

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

package controllers

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/reference"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	objectreferencesv1 "github.com/openshift/custom-resource-status/objectreferences/v1"
	operatorv1alpha1 "github.com/operator-framework/api/pkg/operators/v1alpha1"
	odfv1alpha1 "github.com/red-hat-data-services/odf-operator/api/v1alpha1"
	"github.com/red-hat-data-services/odf-operator/metrics"
	"github.com/red-hat-data-services/odf-operator/pkg/util"
)

const (
	storageSystemFinalizer = "storagesystem.odf.openshift.io"
)

// StorageSystemReconciler reconciles a StorageSystem object
type StorageSystemReconciler struct {
	client.Client
	Log      logr.Logger
	Scheme   *runtime.Scheme
	Recorder *EventReporter
}

//+kubebuilder:rbac:groups=odf.openshift.io,resources=storagesystems,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=odf.openshift.io,resources=storagesystems/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=odf.openshift.io,resources=storagesystems/finalizers,verbs=update
//+kubebuilder:rbac:groups=ocs.openshift.io,resources=storageclusters,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=odf.ibm.com,resources=flashsystemclusters,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=operators.coreos.com,resources=catalogsources,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=operators.coreos.com,resources=clusterserviceversions,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=operators.coreos.com,resources=subscriptions,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=console.openshift.io,resources=consolequickstarts,verbs=*
//+kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch;create;update

// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.8.3/pkg/reconcile
func (r *StorageSystemReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := r.Log.WithValues("instance", req.NamespacedName)

	instance := &odfv1alpha1.StorageSystem{}
	err := r.Client.Get(context.TODO(), req.NamespacedName, instance)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("storagesystem instance not found")
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return ctrl.Result{}, err
	}

	logger.Info("storagesystem instance found")

	metrics.ReportODFSystemMapMetrics(instance.Name, instance.Spec.Name, instance.Spec.Namespace, string(instance.Spec.Kind))

	// Reconcile changes
	result, reconcileError := r.reconcile(instance, logger)

	// Apply status changes
	statusError := r.Client.Status().Update(context.TODO(), instance)
	if statusError != nil {
		logger.Error(statusError, "failed to update status")
	}

	// Reconcile errors have higher priority than status update errors
	if reconcileError != nil {
		r.Recorder.ReportIfNotPresent(instance, corev1.EventTypeWarning, EventReasonReconcileFailed, reconcileError.Error())
		return result, reconcileError
	} else if statusError != nil {
		return result, statusError
	} else {
		return result, nil
	}
}

func (r *StorageSystemReconciler) reconcile(instance *odfv1alpha1.StorageSystem, logger logr.Logger) (ctrl.Result, error) {

	var err error

	if instance.Status.Conditions == nil {
		SetReconcileInitConditions(&instance.Status.Conditions, "Init", "Initializing StorageSystem")
	} else {
		SetReconcileStartConditions(&instance.Status.Conditions, "Reconciling", "Reconcile is in progress")
	}

	if err = r.validateStorageSystemSpec(instance, logger); err != nil {
		logger.Error(err, "failed to validate storagesystem")
		return reconcile.Result{}, err
	}

	// add/remove finalizer
	if instance.GetDeletionTimestamp().IsZero() {
		if !util.FindInSlice(instance.GetFinalizers(), storageSystemFinalizer) {
			logger.Info("finalizer not found Add finalizer", "Finalizer", storageSystemFinalizer)
			instance.ObjectMeta.Finalizers = append(instance.ObjectMeta.Finalizers, storageSystemFinalizer)
			if err = r.Client.Update(context.TODO(), instance); err != nil {
				logger.Error(err, "failed to update storagesystem with finalizer", "Finalizer", storageSystemFinalizer)
				return ctrl.Result{}, err
			}
		}
	} else {
		// deletion phase

		if util.FindInSlice(instance.GetFinalizers(), storageSystemFinalizer) {
			SetDeletionInProgressConditions(&instance.Status.Conditions, "Deleting", "Deletion is in progress")

			err = r.deleteResources(instance, logger)
			if err != nil {
				return ctrl.Result{}, err
			}

			logger.Info("storagesystem is in deletion phase remove finalizer", "Finalizer", storageSystemFinalizer)
			instance.ObjectMeta.Finalizers = util.RemoveFromSlice(instance.ObjectMeta.Finalizers, storageSystemFinalizer)
			if err := r.Client.Update(context.TODO(), instance); err != nil {
				logger.Error(err, "failed to remove finalizer from storagesystem", "Finalizer", storageSystemFinalizer)
				return ctrl.Result{}, err
			}
		}
		logger.Info("storagesystem object is terminated, skipping reconciliation")
		return ctrl.Result{}, nil
	}

	err = r.ensureQuickStarts(logger)
	if err != nil {
		return ctrl.Result{}, err
	}

	err = r.ensureSubscription(instance, logger)
	if err != nil {
		return ctrl.Result{}, err
	}

	err = r.isVendorCsvReady(instance, logger)
	if err != nil {
		return ctrl.Result{}, err
	}

	err = r.isVendorSystemPresent(instance, logger)
	if err != nil {
		return ctrl.Result{}, err
	}

	SetReconcileCompleteConditions(&instance.Status.Conditions, "ReconcileCompleted", "Reconcile is completed successfully")

	return ctrl.Result{}, nil
}

func (r *StorageSystemReconciler) validateStorageSystemSpec(instance *odfv1alpha1.StorageSystem, logger logr.Logger) error {

	if instance.Spec.Kind != VendorStorageCluster() && instance.Spec.Kind != VendorFlashSystemCluster() {
		err := fmt.Errorf("unsupported kind %s", instance.Spec.Kind)
		r.Recorder.ReportIfNotPresent(instance, corev1.EventTypeWarning, EventReasonValidationFailed, err.Error())
		SetStorageSystemInvalidConditions(&instance.Status.Conditions, "NotValid", err.Error())
		return err
	} else {
		SetStorageSystemInvalidCondition(&instance.Status.Conditions, corev1.ConditionFalse, "Valid", "StorageSystem CR is valid")
	}

	return nil
}

func (r *StorageSystemReconciler) addReferenceToRelatedObjects(instance *odfv1alpha1.StorageSystem, logger logr.Logger, object runtime.Object) error {

	objectRef, err := reference.GetReference(r.Scheme, object)
	if err != nil {
		logger.Error(err, "Failed to get reference of object.")
		return err
	}

	err = objectreferencesv1.SetObjectReference(&instance.Status.RelatedObjects, *objectRef)
	if err != nil {
		logger.Error(err, "Failed to set reference of object into RelatedObjects.")
		return err
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *StorageSystemReconciler) SetupWithManager(mgr ctrl.Manager) error {

	generationChangedPredicate := predicate.GenerationChangedPredicate{}

	ignoreCreatePredicate := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			// Ignore create events as resource created by us
			return false
		},
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&odfv1alpha1.StorageSystem{}, builder.WithPredicates(generationChangedPredicate)).
		Owns(&operatorv1alpha1.Subscription{}, builder.WithPredicates(generationChangedPredicate, ignoreCreatePredicate)).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}
