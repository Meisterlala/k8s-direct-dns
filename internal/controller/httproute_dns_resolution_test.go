package controller

import (
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewaynetworkingv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestRouteResolveTargets(t *testing.T) {
	tests := []struct {
		name  string
		route gatewaynetworkingv1.HTTPRoute
		def   bool
		want  bool
	}{
		{
			name:  "defaults to disabled",
			route: gatewaynetworkingv1.HTTPRoute{},
			def:   false,
			want:  false,
		},
		{
			name:  "uses global default when enabled",
			route: gatewaynetworkingv1.HTTPRoute{},
			def:   true,
			want:  true,
		},
		{
			name: "enabled true",
			route: gatewaynetworkingv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				annotationResolve: "true",
			}}},
			def:  false,
			want: true,
		},
		{
			name: "enabled false",
			route: gatewaynetworkingv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				annotationResolve: "false",
			}}},
			def:  true,
			want: false,
		},
		{
			name: "invalid value falls back to global default",
			route: gatewaynetworkingv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				annotationResolve: "not-a-bool",
			}}},
			def:  true,
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := routeResolveTargets(&tt.route, tt.def); got != tt.want {
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

func TestFilterTargetsByIPFamily(t *testing.T) {
	tests := []struct {
		name       string
		targets    []string
		enableIPv4 bool
		enableIPv6 bool
		want       []string
	}{
		{
			name:       "ipv4 only default",
			targets:    []string{"203.0.113.1", "2001:db8::1", "edge.example.net"},
			enableIPv4: true,
			enableIPv6: false,
			want:       []string{"203.0.113.1", "edge.example.net"},
		},
		{
			name:       "ipv6 only",
			targets:    []string{"203.0.113.1", "2001:db8::1", "edge.example.net"},
			enableIPv4: false,
			enableIPv6: true,
			want:       []string{"2001:db8::1", "edge.example.net"},
		},
		{
			name:       "dual stack enabled",
			targets:    []string{"203.0.113.1", "2001:db8::1", "edge.example.net"},
			enableIPv4: true,
			enableIPv6: true,
			want:       []string{"203.0.113.1", "2001:db8::1", "edge.example.net"},
		},
		{
			name:       "both disabled keeps cname only",
			targets:    []string{"203.0.113.1", "2001:db8::1", "edge.example.net"},
			enableIPv4: false,
			enableIPv6: false,
			want:       []string{"edge.example.net"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filterTargetsByIPFamily(tt.targets, tt.enableIPv4, tt.enableIPv6)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("filterTargetsByIPFamily() = %#v, want %#v", got, tt.want)
			}
		})
	}
}
