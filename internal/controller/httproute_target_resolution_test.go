package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestResolveNodeTargetFromNode(t *testing.T) {
	tests := []struct {
		name string
		node corev1.Node
		want string
	}{
		{
			name: "uses directdns target annotation first",
			node: corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						nodeTargetAnn:     "custom.example.net",
						k3sExternalDNSAnn: "k3s.example.net",
						k3sExternalIPAnn:  "203.0.113.10",
					},
				},
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.10"}},
				},
			},
			want: "custom.example.net",
		},
		{
			name: "uses k3s external dns annotation before external ip",
			node: corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						k3sExternalDNSAnn: "home.example.net",
						k3sExternalIPAnn:  "203.0.113.11",
					},
				},
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.11"}},
				},
			},
			want: "home.example.net",
		},
		{
			name: "uses node external dns address when annotation missing",
			node: corev1.Node{
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{
						{Type: corev1.NodeExternalDNS, Address: "edge.example.net"},
						{Type: corev1.NodeExternalIP, Address: "203.0.113.12"},
					},
				},
			},
			want: "edge.example.net",
		},
		{
			name: "uses k3s external ip annotation before status external ip",
			node: corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{k3sExternalIPAnn: "203.0.113.13"},
				},
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{{Type: corev1.NodeExternalIP, Address: "203.0.113.14"}},
				},
			},
			want: "203.0.113.13",
		},
		{
			name: "falls back to status external ip",
			node: corev1.Node{
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{{Type: corev1.NodeExternalIP, Address: "203.0.113.15"}},
				},
			},
			want: "203.0.113.15",
		},
		{
			name: "falls back to internal ip last",
			node: corev1.Node{
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.16"}},
				},
			},
			want: "10.0.0.16",
		},
		{
			name: "returns empty when no target available",
			node: corev1.Node{},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveNodeTargetFromNode(&tt.node)
			if got != tt.want {
				t.Fatalf("resolveNodeTargetFromNode() = %q, want %q", got, tt.want)
			}
		})
	}
}
