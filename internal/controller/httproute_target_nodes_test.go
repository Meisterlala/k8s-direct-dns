package controller

import (
	"context"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewaynetworkingv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestRouteTargets_UsesTargetNodesOverride(t *testing.T) {
	route := &gatewaynetworkingv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "matrix",
			Namespace: "matrix",
			Annotations: map[string]string{
				annotationTargetNodes: "baldr",
			},
		},
	}

	reconciler := newTargetNodeTestReconciler(t,
		newIngressNode("baldr", "netcup", "193.31.25.101"),
		newIngressNode("odin", "home", "178.2.21.133"),
		newEndpointSlice("matrix", "conduwuit", "odin"),
	)

	targets, nodes, backendZonesKnown, err := reconciler.routeTargets(context.Background(), route, []string{"conduwuit"})
	if err != nil {
		t.Fatalf("routeTargets() error = %v", err)
	}

	if !backendZonesKnown {
		t.Fatalf("routeTargets() backendZonesKnown = %v, want true", backendZonesKnown)
	}

	if !reflect.DeepEqual(targets, []string{"193.31.25.101"}) {
		t.Fatalf("routeTargets() targets = %#v, want %#v", targets, []string{"193.31.25.101"})
	}

	if !reflect.DeepEqual(nodes, []string{"baldr"}) {
		t.Fatalf("routeTargets() nodes = %#v, want %#v", nodes, []string{"baldr"})
	}
}

func TestRouteTargets_UsesBackendZonesWhenNoOverride(t *testing.T) {
	route := &gatewaynetworkingv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "app",
			Namespace: "default",
		},
	}

	reconciler := newTargetNodeTestReconciler(t,
		newIngressNode("baldr", "netcup", "193.31.25.101"),
		newIngressNode("odin", "home", "178.2.21.133"),
		newEndpointSlice("default", "svc", "odin"),
	)

	targets, nodes, backendZonesKnown, err := reconciler.routeTargets(context.Background(), route, []string{"svc"})
	if err != nil {
		t.Fatalf("routeTargets() error = %v", err)
	}

	if !backendZonesKnown {
		t.Fatalf("routeTargets() backendZonesKnown = %v, want true", backendZonesKnown)
	}

	if !reflect.DeepEqual(targets, []string{"178.2.21.133"}) {
		t.Fatalf("routeTargets() targets = %#v, want %#v", targets, []string{"178.2.21.133"})
	}

	if !reflect.DeepEqual(nodes, []string{"odin"}) {
		t.Fatalf("routeTargets() nodes = %#v, want %#v", nodes, []string{"odin"})
	}
}

func newTargetNodeTestReconciler(t *testing.T, objects ...client.Object) *HTTPRouteReconciler {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme(corev1) error = %v", err)
	}
	if err := discoveryv1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme(discoveryv1) error = %v", err)
	}
	if err := gatewaynetworkingv1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme(gatewaynetworkingv1) error = %v", err)
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()

	return &HTTPRouteReconciler{Client: cl, Scheme: scheme}
}

func newIngressNode(name, zone, target string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				ingressRoleLabel: name,
				zoneLabel:        zone,
			},
			Annotations: map[string]string{
				nodeTargetAnn: target,
			},
		},
	}
}

func newEndpointSlice(namespace, serviceName, nodeName string) *discoveryv1.EndpointSlice {
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName + "-slice",
			Namespace: namespace,
			Labels: map[string]string{
				discoveryv1.LabelServiceName: serviceName,
			},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints: []discoveryv1.Endpoint{
			{
				NodeName: &nodeName,
				Conditions: discoveryv1.EndpointConditions{
					Ready: ptrTo(true),
				},
			},
		},
	}
}

func ptrTo[T any](value T) *T {
	return &value
}
