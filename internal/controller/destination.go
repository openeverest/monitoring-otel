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

package controller

import (
	"context"
	"fmt"
	"sync"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	monitoringv1alpha1 "github.com/openeverest/openeverest/v2/api/monitoring/v1alpha1"
)

const (
	// readyRecheckInterval paces the export probe of a running collector.
	readyRecheckInterval = time.Minute
	// startingRecheckInterval paces the checks of a collector that is not
	// running yet.
	startingRecheckInterval = 15 * time.Second
)

// destinationReconciler runs one collector per destination of our classes
// and reports the destination Ready from what the collector exports.
type destinationReconciler struct {
	client             client.Client
	cc                 classController
	targetAllocatorCRB string
	collector          collectorOptions
	probe              exportProbe

	mu      sync.Mutex
	samples map[types.NamespacedName]exportSample
}

// exportSample is the last exporter counters probed for a destination.
type exportSample struct {
	uid      types.UID
	counters exportCounters
}

// classController is the subset of monitoring.ClassController the
// destination reconciler uses.
type classController interface {
	OwnsClass(class *monitoringv1alpha1.MonitoringClass) bool
	OwnsClassNamed(ctx context.Context, name string) (bool, error)
	SetDestinationReady(ctx context.Context, dst *monitoringv1alpha1.MonitoringDestination, reason monitoringv1alpha1.ReadyReason, message, serverVersion string) error
}

func (r *destinationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	dst := &monitoringv1alpha1.MonitoringDestination{}
	if err := r.client.Get(ctx, req.NamespacedName, dst); err != nil {
		if client.IgnoreNotFound(err) == nil {
			r.forget(req.NamespacedName)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	owned, err := r.cc.OwnsClassNamed(ctx, dst.Spec.ClassRef.Name)
	if err != nil || !owned || !dst.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, err
	}
	params, err := decodeParameters(dst.Spec.Parameters)
	if err != nil {
		return ctrl.Result{}, r.cc.SetDestinationReady(ctx, dst, monitoringv1alpha1.ReasonReadyInvalidParameters, err.Error(), "")
	}
	if !destinationAccepted(dst) {
		return ctrl.Result{}, r.cc.SetDestinationReady(ctx, dst, monitoringv1alpha1.ReasonReadyPending,
			"waiting for OpenEverest to accept the destination", "")
	}

	collector, err := collectorFor(dst, params, r.collector)
	if err != nil {
		return ctrl.Result{}, r.cc.SetDestinationReady(ctx, dst, monitoringv1alpha1.ReasonReadyInvalidParameters, err.Error(), "")
	}
	if err := r.applyTargetAllocatorRBAC(ctx, dst); err != nil {
		return ctrl.Result{RequeueAfter: applyRetryInterval}, r.cc.SetDestinationReady(ctx, dst, monitoringv1alpha1.ReasonReadyPending,
			fmt.Sprintf("%v; retrying in %s", err, applyRetryInterval), "")
	}
	if err := r.client.Apply(ctx, client.ApplyConfigurationFromUnstructured(collector), client.FieldOwner(FieldOwner), client.ForceOwnership); err != nil {
		return ctrl.Result{RequeueAfter: applyRetryInterval}, r.cc.SetDestinationReady(ctx, dst, monitoringv1alpha1.ReasonReadyPending,
			fmt.Sprintf("apply OpenTelemetryCollector %s: %v; retrying in %s", collector.GetName(), err, applyRetryInterval), "")
	}

	reason, message, version, recheck, err := r.readiness(ctx, dst)
	if err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: recheck}, r.cc.SetDestinationReady(ctx, dst, reason, message, version)
}

// readiness derives the destination's Ready condition: Pending until the
// collector runs, then from the change of its exporter counters since the
// last probe.
func (r *destinationReconciler) readiness(ctx context.Context, dst *monitoringv1alpha1.MonitoringDestination) (monitoringv1alpha1.ReadyReason, string, string, time.Duration, error) {
	name := collectorName(dst.Name)
	live := &unstructured.Unstructured{}
	live.SetAPIVersion(collectorAPIVersion)
	live.SetKind(collectorKind)
	if err := r.client.Get(ctx, client.ObjectKey{Namespace: dst.Namespace, Name: name}, live); err != nil {
		return "", "", "", 0, fmt.Errorf("get OpenTelemetryCollector %s: %w", name, err)
	}
	version, _, _ := unstructured.NestedString(live.Object, "status", "version")
	replicas, _, _ := unstructured.NestedString(live.Object, "status", "scale", "statusReplicas")
	if replicas == "" || replicas[0] == '0' {
		return monitoringv1alpha1.ReasonReadyPending, fmt.Sprintf("collector %s is starting (%s ready)", name, orNone(replicas)),
			version, startingRecheckInterval, nil
	}

	now, err := r.probe(ctx, collectorTelemetryURL(dst))
	if err != nil {
		return monitoringv1alpha1.ReasonReadyPending, fmt.Sprintf("collector %s runs, but its telemetry could not be read: %v", name, err),
			version, startingRecheckInterval, nil
	}
	delta := r.record(client.ObjectKeyFromObject(dst), dst.UID, now)
	reason, message := readinessFromExports(delta)
	return reason, fmt.Sprintf("collector %s: %s", name, message), version, readyRecheckInterval, nil
}

// readinessFromExports maps what the collector exported since the last
// probe to a Ready reason. A collector with nothing to send yet is Ready:
// there is no failure to report.
func readinessFromExports(delta exportCounters) (monitoringv1alpha1.ReadyReason, string) {
	switch {
	case delta.Rejected > 0 && delta.Sent == 0:
		return monitoringv1alpha1.ReasonUnauthorized, fmt.Sprintf("the destination rejected the credentials for %.0f data points", delta.Rejected)
	case delta.Failed > 0 && delta.Sent == 0:
		return monitoringv1alpha1.ReasonUnreachable, fmt.Sprintf("exporting failed for %.0f data points and none was accepted; see the collector logs", delta.Failed)
	case delta.Failed > 0:
		return monitoringv1alpha1.ReasonReady, fmt.Sprintf("exported %.0f data points, %.0f failed", delta.Sent, delta.Failed)
	case delta.Sent > 0:
		return monitoringv1alpha1.ReasonReady, fmt.Sprintf("exported %.0f data points", delta.Sent)
	default:
		return monitoringv1alpha1.ReasonReady, "running, nothing exported yet"
	}
}

// record stores the latest counters of a destination and returns their
// increase since the previous probe; the first probe, a recreated
// destination or a restarted collector counts from zero.
func (r *destinationReconciler) record(key types.NamespacedName, uid types.UID, now exportCounters) exportCounters {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.samples == nil {
		r.samples = map[types.NamespacedName]exportSample{}
	}
	prev, ok := r.samples[key]
	r.samples[key] = exportSample{uid: uid, counters: now}
	if !ok || prev.uid != uid || now.Sent < prev.counters.Sent || now.Failed < prev.counters.Failed {
		return now
	}
	return exportCounters{
		Sent:     now.Sent - prev.counters.Sent,
		Failed:   now.Failed - prev.counters.Failed,
		Rejected: now.Rejected - prev.counters.Rejected,
	}
}

func (r *destinationReconciler) forget(key types.NamespacedName) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.samples, key)
}

// applyTargetAllocatorRBAC binds the target allocator of the destination's
// collector to the admin-provided ClusterRole within the destination's
// namespace only.
func (r *destinationReconciler) applyTargetAllocatorRBAC(ctx context.Context, dst *monitoringv1alpha1.MonitoringDestination) error {
	rb := &rbacv1.RoleBinding{
		TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{
			Name:            targetAllocatorServiceAccount(dst.Name),
			Namespace:       dst.Namespace,
			Labels:          map[string]string{monitoringv1alpha1.BindingDestinationLabel: dst.Name},
			OwnerReferences: []metav1.OwnerReference{destinationOwner(dst)},
		},
		RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: r.targetAllocatorCRB},
		Subjects: []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      targetAllocatorServiceAccount(dst.Name),
			Namespace: dst.Namespace,
		}},
	}
	u, err := toUnstructured(rb)
	if err != nil {
		return err
	}
	if err := r.client.Apply(ctx, client.ApplyConfigurationFromUnstructured(u), client.FieldOwner(FieldOwner), client.ForceOwnership); err != nil {
		return fmt.Errorf("apply RoleBinding %s: %w", rb.Name, err)
	}
	return nil
}

// destinationAccepted reports whether core accepted the current generation
// of the destination.
func destinationAccepted(dst *monitoringv1alpha1.MonitoringDestination) bool {
	c := meta.FindStatusCondition(dst.Status.Conditions, monitoringv1alpha1.MonitoringDestinationConditionAccepted)
	return c != nil && c.Status == metav1.ConditionTrue && c.ObservedGeneration == dst.Generation
}

// destinationsOfClass enqueues the destinations of a class this controller
// claims, so they are checked once the class appears or changes.
func (r *destinationReconciler) destinationsOfClass(ctx context.Context, obj client.Object) []reconcile.Request {
	class, ok := obj.(*monitoringv1alpha1.MonitoringClass)
	if !ok || !r.cc.OwnsClass(class) {
		return nil
	}
	list := &monitoringv1alpha1.MonitoringDestinationList{}
	if err := r.client.List(ctx, list); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "listing MonitoringDestinations failed", "class", class.Name)
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		if list.Items[i].Spec.ClassRef.Name == class.Name {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
		}
	}
	return reqs
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
