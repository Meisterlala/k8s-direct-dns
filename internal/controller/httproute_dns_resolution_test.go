package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewaynetworkingv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestRouteResolveTargets(t *testing.T) {
	tests := []struct {
		name  string
		route gatewaynetworkingv1.HTTPRoute
		want  bool
	}{
		{
			name:  "defaults to disabled",
			route: gatewaynetworkingv1.HTTPRoute{},
			want:  false,
		},
		{
			name: "enabled true",
			route: gatewaynetworkingv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				annotationResolve: "true",
			}}},
			want: true,
		},
		{
			name: "enabled false",
			route: gatewaynetworkingv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				annotationResolve: "false",
			}}},
			want: false,
		},
		{
			name: "invalid value defaults disabled",
			route: gatewaynetworkingv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				annotationResolve: "not-a-bool",
			}}},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := routeResolveTargets(&tt.route); got != tt.want {
				t.Fatalf("routeResolveTargets() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRouteDNSServer(t *testing.T) {
	tests := []struct {
		name     string
		route    gatewaynetworkingv1.HTTPRoute
		fallback string
		want     string
	}{
		{
			name:     "uses fallback with default port",
			route:    gatewaynetworkingv1.HTTPRoute{},
			fallback: "1.1.1.1",
			want:     "1.1.1.1:53",
		},
		{
			name: "uses annotation override",
			route: gatewaynetworkingv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				annotationDNSServer: "9.9.9.9",
			}}},
			fallback: "1.1.1.1:53",
			want:     "9.9.9.9:53",
		},
		{
			name: "keeps explicit port",
			route: gatewaynetworkingv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				annotationDNSServer: "8.8.8.8:5353",
			}}},
			fallback: "1.1.1.1:53",
			want:     "8.8.8.8:5353",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := routeDNSServer(&tt.route, tt.fallback); got != tt.want {
				t.Fatalf("routeDNSServer() = %q, want %q", got, tt.want)
			}
		})
	}
}
