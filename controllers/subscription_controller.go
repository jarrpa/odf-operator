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
	"time"

	"github.com/go-logr/logr"
	operatorv1alpha1 "github.com/operator-framework/api/pkg/operators/v1alpha1"
	odfv1alpha1 "github.com/red-hat-data-services/odf-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// SubscriptionReconciler reconciles a Subscription object
type SubscriptionReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder *EventReporter
}

//+kubebuilder:rbac:groups=operators.coreos.com,resources=subscriptions,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=operators.coreos.com,resources=subscriptions/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=operators.coreos.com,resources=subscriptions/finalizers,verbs=update

// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.8.3/pkg/reconcile
func (r *SubscriptionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {

	logger := log.FromContext(ctx)

	instance := &operatorv1alpha1.Subscription{}
	err := r.Client.Get(context.TODO(), req.NamespacedName, instance)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("Subscription instance not found.")
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return ctrl.Result{}, err
	}

	err = r.ensureSubscriptions(logger)
	if err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *SubscriptionReconciler) ensureSubscriptions(logger logr.Logger) error {

	storageSystemReconciler := &StorageSystemReconciler{
		Client: r.Client,
		Scheme: r.Scheme,
	}

	storageSystemInstance := &odfv1alpha1.StorageSystem{
		ObjectMeta: metav1.ObjectMeta{},
		Spec: odfv1alpha1.StorageSystemSpec{
			Kind: StorageClusterKind,
		},
	}

	// create ocs subscriptions
	err := storageSystemReconciler.ensureSubscription(storageSystemInstance, logger)
	if err != nil {
		return err
	}

	storageSystemList := &odfv1alpha1.StorageSystemList{}
	err = r.Client.List(context.TODO(), storageSystemList)
	if err != nil {
		return err
	}

	// create IBM subscription only if IBM storageSystem exists
	for i, storageSystem := range storageSystemList.Items {
		if storageSystem.Spec.Kind == FlashSystemKind {
			err = storageSystemReconciler.ensureSubscription(&storageSystemList.Items[i], logger)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (r *SubscriptionReconciler) createSubscriptionsOnStartUp() error {
	// This is a hack to work around the absence of StorageCluster CRs.

	// At this point, r.Client (from the manager) is using a cache that is not
	// initialized, so we create a temporary client that skips the cache for
	// Subscriptions.

	tmpClient, cliErr := cluster.NewClientBuilder().
		WithUncached(&operatorv1alpha1.Subscription{}, &odfv1alpha1.StorageSystem{}).
		Build(nil, ctrl.GetConfigOrDie(), client.Options{
			Scheme: r.Client.Scheme(),
			Mapper: r.Client.RESTMapper(),
		})
	if cliErr != nil {
		return cliErr
	}

	mgrClient := r.Client
	r.Client = tmpClient
	defer func() { r.Client = mgrClient }()

	logger := ctrl.Log.WithName("controllers").WithName("Subscription").WithName("SetupWithManager")

	for {
		err := r.ensureSubscriptions(logger)
		if err == nil {
			break
		} else {
			logger.Error(err, "failed to create OCS subscriptions, will retry after 5 seconds")
			time.Sleep(5 * time.Second)
		}
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *SubscriptionReconciler) SetupWithManager(mgr ctrl.Manager) error {

	err := r.createSubscriptionsOnStartUp()
	if err != nil {
		return err
	}

	generationChangedPredicate := predicate.GenerationChangedPredicate{}

	ignoreCreatePredicate := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			// Ignore create events as owned subscriptions are created by us
			return false
		},
	}

	predicateFunc := func(obj runtime.Object) bool {
		instance, ok := obj.(*operatorv1alpha1.Subscription)
		if !ok {
			return false
		}

		// ignore if not a odf-operator subscription
		if instance.Spec.Package != "odf-operator" {
			return false
		}

		return true
	}

	subscriptionPredicate := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return predicateFunc(e.Object)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return false
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return predicateFunc(e.ObjectNew)
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return predicateFunc(e.Object)
		},
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&operatorv1alpha1.Subscription{},
			builder.WithPredicates(generationChangedPredicate, subscriptionPredicate)).
		Owns(&operatorv1alpha1.Subscription{},
			builder.WithPredicates(generationChangedPredicate, ignoreCreatePredicate)).
		Complete(r)
}
