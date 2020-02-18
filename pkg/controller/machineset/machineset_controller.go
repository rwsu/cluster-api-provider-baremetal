/*
Copyright 2019 The Kubernetes Authors.

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

package machineset

import (
	"context"

	bmh "github.com/metal3-io/baremetal-operator/pkg/apis/metal3/v1alpha1"
	actuator "github.com/metal3-io/cluster-api-provider-baremetal/pkg/cloud/baremetal/actuators/machine"
	machinev1beta1 "github.com/openshift/cluster-api/pkg/apis/machine/v1beta1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	logf "sigs.k8s.io/controller-runtime/pkg/runtime/log"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

var log = logf.Log.WithName("machineset-controller")

// Add creates a new MachineSet Controller and adds it to the Manager with default RBAC. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	return add(mgr, newReconciler(mgr))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) reconcile.Reconciler {
	return &ReconcileMachineSet{Client: mgr.GetClient(), scheme: mgr.GetScheme()}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New("machineset-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to MachineSet
	err = c.Watch(&source.Kind{Type: &machinev1beta1.MachineSet{}},
		&handler.EnqueueRequestForObject{}, predicate.ResourceVersionChangedPredicate{})
	if err != nil {
		return err
	}

	mapper := msmapper{client: mgr.GetClient()}
	err = c.Watch(&source.Kind{Type: &bmh.BareMetalHost{}},
		&handler.EnqueueRequestsFromMapFunc{ToRequests: &mapper}, predicate.ResourceVersionChangedPredicate{})
	if err != nil {
		return err
	}

	return nil
}

var _ reconcile.Reconciler = &ReconcileMachineSet{}

// ReconcileMachineSet reconciles a MachineSet object
type ReconcileMachineSet struct {
	client.Client
	scheme *runtime.Scheme
}

// Reconcile reads that state of the cluster for a MachineSet object and makes changes based on the state read
// and what is in the MachineSet.Spec
// Automatically generate RBAC rules to allow the Controller to read and write Deployments
// +kubebuilder:rbac:groups=cluster.k8s.io,resources=machinesets,verbs=get;list;watch;update;patch
func (r *ReconcileMachineSet) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	log := log.WithValues("MachineSet", request.NamespacedName.String())
	ctx := context.TODO()
	// Fetch the MachineSet instance
	instance := &machinev1beta1.MachineSet{}
	err := r.Get(ctx, request.NamespacedName, instance)
	if err != nil {
		if errors.IsNotFound(err) {
			// Object not found, return.  Created objects are automatically garbage collected.
			// For additional cleanup logic use finalizers.
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	// Don't take action if the annotation is not present
	annotations := instance.ObjectMeta.GetAnnotations()
	if annotations == nil {
		return reconcile.Result{}, nil
	}
	_, present := annotations[AutoScaleAnnotation]
	if !present {
		return reconcile.Result{}, nil
	}

	// Make sure the MachineSet has a non-empty selector.
	msselector, err := metav1.LabelSelectorAsSelector(&instance.Spec.Selector)
	if err != nil {
		return reconcile.Result{}, err
	}
	if msselector.Empty() {
		// The cluster-api machinesetcontroller expects every MachineSet to have
		// its Selector set.
		log.Info("MachineSet has empty selector, which is unexpected. Will not attempt scaling.")
		return reconcile.Result{}, nil
	}

	hostselector, err := actuator.SelectorFromProviderSpec(&instance.Spec.Template.Spec.ProviderSpec)
	if err != nil {
		return reconcile.Result{}, err
	}

	hosts := &bmh.BareMetalHostList{}
	opts := &client.ListOptions{
		Namespace: instance.Namespace,
	}

	err = r.List(ctx, hosts, client.UseListOptions(opts))
	if err != nil {
		return reconcile.Result{}, err
	}

	var count int32
	for _, host := range hosts.Items {
		consumer := host.Spec.ConsumerRef
		if consumer == nil {
			if hostselector.Matches(labels.Set(host.ObjectMeta.Labels)) {
				count++
				continue
			}
		} else {
			// We will only count this host if it is consumed by a Machine that
			// is part of the current MachineSet.
			machine := &machinev1beta1.Machine{}
			if consumer.Kind != machine.Kind || consumer.APIVersion != machine.APIVersion {
				// this host is being consumed by something else; don't count it
				continue
			}

			// host is being consumed. Let's get the Machine and see if it
			// matches the current MachineSet.
			nn := types.NamespacedName{
				Name:      consumer.Name,
				Namespace: consumer.Namespace,
			}
			err := r.Get(ctx, nn, machine)
			if err != nil {
				if errors.IsNotFound(err) {
					log.Info("Will not scale while BareMetalHost's consuming Machine is not found", "Machine.Name", nn.Name)
					return reconcile.Result{}, nil
				}
				// Error reading the object - requeue the request.
				return reconcile.Result{}, err
			}
			if msselector.Matches(labels.Set(machine.ObjectMeta.Labels)) {
				count++
			}
		}
	}

	if instance.Spec.Replicas == nil || count != *instance.Spec.Replicas {
		log.Info("Scaling MachineSet", "new_replicas", count, "old_replicas", instance.Spec.Replicas)
		new := instance.DeepCopy()
		new.Spec.Replicas = &count
		err = r.Update(ctx, new)
		if err != nil {
			return reconcile.Result{}, err
		}
	}

	return reconcile.Result{}, nil
}
