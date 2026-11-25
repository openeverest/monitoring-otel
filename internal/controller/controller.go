// Copyright (C) 2026 The OpenEverest Contributors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package controller reconciles ExtensionManaged MonitoringBindings of an
// opentelemetry MonitoringClass: one OpenTelemetry Collector per
// MonitoringDestination, fed by one PodMonitor per component of every bound
// Instance through the collector's target allocator.
package controller

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	monitoringv1alpha1 "github.com/openeverest/openeverest/v2/api/monitoring/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/monitoring"
)

// FieldOwner is the server-side-apply field manager of everything this
// controller writes.
const FieldOwner = "openeverest.io/monitoring-otel"

// applyRetryInterval paces retries while PodMonitors or collectors cannot be
// written, for example until the operator CRDs are installed.
const applyRetryInterval = 30 * time.Second

// Options configures the controllers.
type Options struct {
	// ControllerName claims the MonitoringClasses whose controllerName it is.
	ControllerName string
	// TargetAllocatorClusterRole is the ClusterRole bound, in each
	// destination's namespace, to the target allocator of its collector.
	TargetAllocatorClusterRole string
	// CollectorImage overrides the OpenTelemetry Operator's collector image.
	CollectorImage string
	// TargetAllocatorMTLS enables mTLS between target allocator and collector.
	TargetAllocatorMTLS bool
}

// Setup registers the controllers serving every MonitoringClass whose
// controllerName is opts.ControllerName: one claims the classes, one runs a
// collector per destination and one renders the PodMonitors of every
// binding. Collectors and PodMonitors are only ever written, never watched,
// so the manager starts without the OpenTelemetry and Prometheus Operator
// CRDs.
func Setup(mgr ctrl.Manager, opts Options) error {
	cc := monitoring.ClassController{Client: mgr.GetClient(), FieldOwner: FieldOwner, ControllerName: opts.ControllerName}

	if err := ctrl.NewControllerManagedBy(mgr).
		Named("monitoring-otel-class").
		For(&monitoringv1alpha1.MonitoringClass{}, builder.WithPredicates(
			predicate.GenerationChangedPredicate{},
			predicate.NewPredicateFuncs(func(obj client.Object) bool {
				class, ok := obj.(*monitoringv1alpha1.MonitoringClass)
				return ok && cc.OwnsClass(class)
			}),
		)).
		Complete(&classReconciler{client: mgr.GetClient(), cc: cc}); err != nil {
		return fmt.Errorf("set up MonitoringClass controller: %w", err)
	}

	dr := &destinationReconciler{
		client:             mgr.GetClient(),
		cc:                 cc,
		targetAllocatorCRB: opts.TargetAllocatorClusterRole,
		collector:          collectorOptions{Image: opts.CollectorImage, TargetAllocatorMTLS: opts.TargetAllocatorMTLS},
		probe:              httpExportProbe(&http.Client{}),
	}
	if err := ctrl.NewControllerManagedBy(mgr).
		Named("monitoring-otel-destination").
		For(&monitoringv1alpha1.MonitoringDestination{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{}, acceptedChanged()))).
		Watches(&monitoringv1alpha1.MonitoringClass{}, handler.EnqueueRequestsFromMapFunc(dr.destinationsOfClass),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(dr); err != nil {
		return fmt.Errorf("set up MonitoringDestination controller: %w", err)
	}

	r := &bindingReconciler{client: mgr.GetClient(), cc: cc}
	if err := ctrl.NewControllerManagedBy(mgr).
		Named("monitoring-otel").
		For(&monitoringv1alpha1.MonitoringBinding{}, builder.WithPredicates(monitoring.ExtensionManagedBindingPredicate())).
		Watches(&corev1alpha1.Instance{}, handler.EnqueueRequestsFromMapFunc(r.bindingsForInstance),
			builder.WithPredicates(sourcesChanged())).
		Watches(&monitoringv1alpha1.MonitoringDestination{}, handler.EnqueueRequestsFromMapFunc(r.bindingsForDestination),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(r); err != nil {
		return fmt.Errorf("set up MonitoringBinding controller: %w", err)
	}
	return nil
}

// classReconciler claims the served classes and leaves any other unclaimed.
type classReconciler struct {
	client client.Client
	cc     monitoring.ClassController
}

func (r *classReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	class := &monitoringv1alpha1.MonitoringClass{}
	if err := r.client.Get(ctx, req.NamespacedName, class); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	return ctrl.Result{}, r.cc.ClaimClass(ctx, class)
}

// acceptedChanged passes destination updates that change core's Accepted
// condition: the collector is rendered only for accepted destinations.
func acceptedChanged() predicate.Funcs {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldDst, okOld := e.ObjectOld.(*monitoringv1alpha1.MonitoringDestination)
			newDst, okNew := e.ObjectNew.(*monitoringv1alpha1.MonitoringDestination)
			return okOld && okNew && destinationAccepted(oldDst) != destinationAccepted(newDst)
		},
	}
}

// sourcesChanged passes Instance updates that change
// status.monitoring.sources, the only Instance field PodMonitors derive from.
func sourcesChanged() predicate.Funcs {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return false },
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldIn, okOld := e.ObjectOld.(*corev1alpha1.Instance)
			newIn, okNew := e.ObjectNew.(*corev1alpha1.Instance)
			return okOld && okNew && !equality.Semantic.DeepEqual(sourcesOf(oldIn), sourcesOf(newIn))
		},
	}
}

func sourcesOf(in *corev1alpha1.Instance) *corev1alpha1.MonitoringSources {
	if in.Status.Monitoring == nil {
		return nil
	}
	return in.Status.Monitoring.Sources
}

func (r *bindingReconciler) bindingsForInstance(ctx context.Context, obj client.Object) []reconcile.Request {
	in, ok := obj.(*corev1alpha1.Instance)
	if !ok {
		return nil
	}
	bindings, err := monitoring.ListBindingsForInstance(ctx, r.client, in)
	if err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "listing bindings for Instance failed", "instance", in.Name)
		return nil
	}
	return r.served(ctx, bindings)
}

func (r *bindingReconciler) bindingsForDestination(ctx context.Context, obj client.Object) []reconcile.Request {
	list := &monitoringv1alpha1.MonitoringBindingList{}
	if err := r.client.List(ctx, list, client.InNamespace(obj.GetNamespace()), client.MatchingLabels{
		monitoringv1alpha1.BindingDestinationLabel: obj.GetName(),
	}); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "listing bindings for MonitoringDestination failed", "destination", obj.GetName())
		return nil
	}
	return r.served(ctx, list.Items)
}

// served returns a request for each binding this controller executes.
func (r *bindingReconciler) served(ctx context.Context, bindings []monitoringv1alpha1.MonitoringBinding) []reconcile.Request {
	var reqs []reconcile.Request
	for i := range bindings {
		executes, err := r.executes(ctx, &bindings[i])
		if err != nil {
			ctrl.LoggerFrom(ctx).Error(err, "resolving the class of a binding failed", "binding", bindings[i].Name)
			continue
		}
		if executes {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&bindings[i])})
		}
	}
	return reqs
}
