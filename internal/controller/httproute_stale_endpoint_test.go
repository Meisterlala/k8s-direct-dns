package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	externaldnsv1alpha1 "sigs.k8s.io/external-dns/apis/v1alpha1"
	"sigs.k8s.io/external-dns/endpoint"
	gatewaynetworkingv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestReconcileNoValidTargetsMarksStaleAndRequeues(t *testing.T) {
	now := time.Date(2026, time.March, 14, 12, 0, 0, 0, time.UTC)
	route := &gatewaynetworkingv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"}}
	dnsEndpoint := &externaldnsv1alpha1.DNSEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: dnsEndpointName(types.NamespacedName{Namespace: route.Namespace, Name: route.Name}), Namespace: route.Namespace},
		Spec: externaldnsv1alpha1.DNSEndpointSpec{Endpoints: []*endpoint.Endpoint{
			endpoint.NewEndpoint("app.example.com", "A", "203.0.113.10"),
		}},
	}

	reconciler := newStaleEndpointTestReconciler(t, now, 5*time.Minute, dnsEndpoint)

	result, err := reconciler.reconcileNoValidTargets(context.Background(), route)
	if err != nil {
		t.Fatalf("reconcileNoValidTargets() error = %v", err)
	}
	if result.RequeueAfter != 5*time.Minute {
		t.Fatalf("reconcileNoValidTargets() RequeueAfter = %v, want %v", result.RequeueAfter, 5*time.Minute)
	}

	updated := &externaldnsv1alpha1.DNSEndpoint{}
	err = reconciler.Get(context.Background(), types.NamespacedName{Namespace: route.Namespace, Name: dnsEndpoint.Name}, updated)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	gotStale := updated.Annotations[annotationStaleSince]
	wantStale := now.Format(time.RFC3339Nano)
	if gotStale != wantStale {
		t.Fatalf("stale-since annotation = %q, want %q", gotStale, wantStale)
	}
}

func TestReconcileNoValidTargetsUsesRemainingGrace(t *testing.T) {
	now := time.Date(2026, time.March, 14, 12, 0, 0, 0, time.UTC)
	staleSince := now.Add(-2 * time.Minute)
	route := &gatewaynetworkingv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"}}
	dnsEndpoint := &externaldnsv1alpha1.DNSEndpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dnsEndpointName(types.NamespacedName{Namespace: route.Namespace, Name: route.Name}),
			Namespace: route.Namespace,
			Annotations: map[string]string{
				annotationStaleSince: staleSince.Format(time.RFC3339Nano),
			},
		},
		Spec: externaldnsv1alpha1.DNSEndpointSpec{Endpoints: []*endpoint.Endpoint{
			endpoint.NewEndpoint("app.example.com", "A", "203.0.113.10"),
		}},
	}

	reconciler := newStaleEndpointTestReconciler(t, now, 5*time.Minute, dnsEndpoint)

	result, err := reconciler.reconcileNoValidTargets(context.Background(), route)
	if err != nil {
		t.Fatalf("reconcileNoValidTargets() error = %v", err)
	}
	if result.RequeueAfter != 3*time.Minute {
		t.Fatalf("reconcileNoValidTargets() RequeueAfter = %v, want %v", result.RequeueAfter, 3*time.Minute)
	}
}

func TestReconcileNoValidTargetsDeletesAfterGrace(t *testing.T) {
	now := time.Date(2026, time.March, 14, 12, 0, 0, 0, time.UTC)
	route := &gatewaynetworkingv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"}}
	dnsEndpoint := &externaldnsv1alpha1.DNSEndpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dnsEndpointName(types.NamespacedName{Namespace: route.Namespace, Name: route.Name}),
			Namespace: route.Namespace,
			Annotations: map[string]string{
				annotationStaleSince: now.Add(-6 * time.Minute).Format(time.RFC3339Nano),
			},
		},
		Spec: externaldnsv1alpha1.DNSEndpointSpec{Endpoints: []*endpoint.Endpoint{
			endpoint.NewEndpoint("app.example.com", "A", "203.0.113.10"),
		}},
	}

	reconciler := newStaleEndpointTestReconciler(t, now, 5*time.Minute, dnsEndpoint)

	result, err := reconciler.reconcileNoValidTargets(context.Background(), route)
	if err != nil {
		t.Fatalf("reconcileNoValidTargets() error = %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Fatalf("reconcileNoValidTargets() RequeueAfter = %v, want 0", result.RequeueAfter)
	}

	deleted := &externaldnsv1alpha1.DNSEndpoint{}
	err = reconciler.Get(context.Background(), client.ObjectKey{Namespace: route.Namespace, Name: dnsEndpoint.Name}, deleted)
	if err == nil {
		t.Fatalf("expected DNSEndpoint to be deleted")
	}
}

func TestReconcileNoValidTargetsDeletesImmediatelyWhenGraceDisabled(t *testing.T) {
	now := time.Date(2026, time.March, 14, 12, 0, 0, 0, time.UTC)
	route := &gatewaynetworkingv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"}}
	dnsEndpoint := &externaldnsv1alpha1.DNSEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: dnsEndpointName(types.NamespacedName{Namespace: route.Namespace, Name: route.Name}), Namespace: route.Namespace},
		Spec: externaldnsv1alpha1.DNSEndpointSpec{Endpoints: []*endpoint.Endpoint{
			endpoint.NewEndpoint("app.example.com", "A", "203.0.113.10"),
		}},
	}

	reconciler := newStaleEndpointTestReconciler(t, now, 0, dnsEndpoint)

	_, err := reconciler.reconcileNoValidTargets(context.Background(), route)
	if err != nil {
		t.Fatalf("reconcileNoValidTargets() error = %v", err)
	}

	deleted := &externaldnsv1alpha1.DNSEndpoint{}
	err = reconciler.Get(context.Background(), client.ObjectKey{Namespace: route.Namespace, Name: dnsEndpoint.Name}, deleted)
	if err == nil {
		t.Fatalf("expected DNSEndpoint to be deleted")
	}
}

func TestUpsertDNSEndpointRemovesStaleAnnotationWithoutEmptyAnnotationsMap(t *testing.T) {
	route := &gatewaynetworkingv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default", UID: types.UID("route-uid")}}
	dnsEndpoint := &externaldnsv1alpha1.DNSEndpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dnsEndpointName(types.NamespacedName{Namespace: route.Namespace, Name: route.Name}),
			Namespace: route.Namespace,
			Annotations: map[string]string{
				annotationStaleSince: time.Now().UTC().Format(time.RFC3339Nano),
			},
		},
	}

	reconciler := newStaleEndpointTestReconciler(t, time.Now().UTC(), 5*time.Minute, dnsEndpoint)

	err := reconciler.upsertDNSEndpoint(context.Background(), route, []string{"app.example.com"}, []string{"203.0.113.10"}, 300)
	if err != nil {
		t.Fatalf("upsertDNSEndpoint() error = %v", err)
	}

	updated := &externaldnsv1alpha1.DNSEndpoint{}
	err = reconciler.Get(context.Background(), types.NamespacedName{Namespace: route.Namespace, Name: dnsEndpoint.Name}, updated)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	if updated.Annotations != nil {
		t.Fatalf("annotations = %#v, want nil", updated.Annotations)
	}
}

func newStaleEndpointTestReconciler(t *testing.T, now time.Time, grace time.Duration, objects ...client.Object) *HTTPRouteReconciler {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := externaldnsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := gatewaynetworkingv1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()

	return &HTTPRouteReconciler{
		Client:                   cl,
		Scheme:                   scheme,
		StaleEndpointGracePeriod: grace,
		now: func() time.Time {
			return now
		},
	}
}
