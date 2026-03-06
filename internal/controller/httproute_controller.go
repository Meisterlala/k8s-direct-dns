/*
Copyright 2026.

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

package controller

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	externaldnsv1alpha1 "sigs.k8s.io/external-dns/apis/v1alpha1"
	"sigs.k8s.io/external-dns/endpoint"
	gatewaynetworkingv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	annotationEnabled = "directdns.meisterlala.dev/enabled"
	annotationTTL     = "directdns.meisterlala.dev/ttl"
	nodeTargetAnn     = "directdns.meisterlala.dev/target"
	k3sExternalDNSAnn = "k3s.io/external-dns"
	k3sExternalIPAnn  = "k3s.io/external-ip"
	zoneLabel         = "topology.kubernetes.io/zone"
	ingressRoleLabel  = "node-role.kubernetes.io/ingress"
	legacyRoleLabel   = "kubernetes.io/role"
	defaultRecordTTL  = int64(1)
)

// HTTPRouteReconciler reconciles a HTTPRoute object
type HTTPRouteReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=externaldns.k8s.io,resources=dnsendpoints,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *HTTPRouteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	route := &gatewaynetworkingv1.HTTPRoute{}
	if err := r.Get(ctx, req.NamespacedName, route); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, r.deleteDNSEndpoint(ctx, req.Namespace, dnsEndpointName(req.NamespacedName))
		}

		return ctrl.Result{}, err
	}

	if !routeEnabled(route) {
		log.Info("Skipping HTTPRoute because direct DNS is disabled", "name", route.Name, "namespace", route.Namespace)
		return ctrl.Result{}, r.deleteDNSEndpoint(ctx, route.Namespace, dnsEndpointName(req.NamespacedName))
	}

	hostnames := routeHostnames(route)
	if len(hostnames) == 0 {
		log.Info("Skipping HTTPRoute because no hostnames were found", "name", route.Name, "namespace", route.Namespace)
		return ctrl.Result{}, r.deleteDNSEndpoint(ctx, route.Namespace, dnsEndpointName(req.NamespacedName))
	}

	serviceNames := backendServiceNames(route)
	if len(serviceNames) == 0 {
		log.Info("Skipping HTTPRoute because no in-namespace Service backends were found", "name", route.Name, "namespace", route.Namespace)
		return ctrl.Result{}, r.deleteDNSEndpoint(ctx, route.Namespace, dnsEndpointName(req.NamespacedName))
	}

	targets, ingressNodeNames, err := r.selectTargetsFromBackendZones(ctx, route.Namespace, serviceNames)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(targets) == 0 {
		log.Info("Skipping HTTPRoute because no ingress nodes with publishable addresses were found in backend zones", "name", route.Name, "namespace", route.Namespace)
		return ctrl.Result{}, r.deleteDNSEndpoint(ctx, route.Namespace, dnsEndpointName(req.NamespacedName))
	}

	if err := r.upsertDNSEndpoint(ctx, route, hostnames, targets, routeTTL(route)); err != nil {
		return ctrl.Result{}, err
	}

	log.Info("Reconciled HTTPRoute DNS targets", "name", route.Name, "namespace", route.Namespace, "nodes", ingressNodeNames, "targets", targets)

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *HTTPRouteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gatewaynetworkingv1.HTTPRoute{}).
		Watches(&discoveryv1.EndpointSlice{}, handler.EnqueueRequestsFromMapFunc(r.mapEndpointSliceToHTTPRoutes)).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.mapNodeToHTTPRoutes)).
		Named("httproute").
		Complete(r)
}

func (r *HTTPRouteReconciler) mapEndpointSliceToHTTPRoutes(ctx context.Context, obj client.Object) []reconcile.Request {
	endpointSlice, ok := obj.(*discoveryv1.EndpointSlice)
	if !ok {
		return nil
	}

	serviceName, ok := endpointSlice.Labels[discoveryv1.LabelServiceName]
	if !ok || serviceName == "" {
		return nil
	}

	routeList := &gatewaynetworkingv1.HTTPRouteList{}
	if err := r.List(ctx, routeList, client.InNamespace(endpointSlice.Namespace)); err != nil {
		return nil
	}

	requests := make([]reconcile.Request, 0, len(routeList.Items))
	for i := range routeList.Items {
		route := &routeList.Items[i]
		if slices.Contains(backendServiceNames(route), serviceName) {
			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: route.Namespace, Name: route.Name}})
		}
	}

	return requests
}

// mapNodeToHTTPRoutes enqueues every route when a Node changes.
//
// Node updates can affect DNS targets through role labels, target override
// annotations, and node address changes.
func (r *HTTPRouteReconciler) mapNodeToHTTPRoutes(ctx context.Context, _ client.Object) []reconcile.Request {
	routeList := &gatewaynetworkingv1.HTTPRouteList{}
	if err := r.List(ctx, routeList); err != nil {
		return nil
	}

	requests := make([]reconcile.Request, 0, len(routeList.Items))
	for i := range routeList.Items {
		route := routeList.Items[i]
		requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: route.Namespace, Name: route.Name}})
	}

	return requests
}

// selectTargetsFromBackendZones maps backend placement to publishable ingress targets.
// It collects ready backend nodes, derives their zones, then includes ingress nodes
// from those zones and resolves their publish targets.
func (r *HTTPRouteReconciler) selectTargetsFromBackendZones(ctx context.Context, namespace string, services []string) ([]string, []string, error) {
	backendNodeNames, err := r.readyBackendNodeNames(ctx, namespace, services)
	if err != nil {
		return nil, nil, err
	}
	if len(backendNodeNames) == 0 {
		return nil, nil, nil
	}

	backendZones, err := r.backendZones(ctx, backendNodeNames)
	if err != nil {
		return nil, nil, err
	}
	if len(backendZones) == 0 {
		return nil, nil, nil
	}

	return r.ingressTargetsForZones(ctx, backendZones)
}

// readyBackendNodeNames returns unique node names that host ready backend endpoints.
func (r *HTTPRouteReconciler) readyBackendNodeNames(ctx context.Context, namespace string, services []string) ([]string, error) {
	backendNodeSet := map[string]struct{}{}

	for _, serviceName := range services {
		endpointSlices := &discoveryv1.EndpointSliceList{}
		if err := r.List(ctx, endpointSlices, client.InNamespace(namespace), client.MatchingLabels{discoveryv1.LabelServiceName: serviceName}); err != nil {
			return nil, err
		}

		for i := range endpointSlices.Items {
			endpointSlice := &endpointSlices.Items[i]
			for _, ep := range endpointSlice.Endpoints {
				if ep.Conditions.Ready != nil && !*ep.Conditions.Ready {
					continue
				}
				if ep.NodeName == nil || *ep.NodeName == "" {
					continue
				}

				backendNodeSet[*ep.NodeName] = struct{}{}
			}
		}
	}

	backendNodeNames := make([]string, 0, len(backendNodeSet))
	for nodeName := range backendNodeSet {
		backendNodeNames = append(backendNodeNames, nodeName)
	}
	slices.Sort(backendNodeNames)

	return backendNodeNames, nil
}

// backendZones resolves a set of topology zones from node names.
// Nodes that do not exist anymore or have no zone label are skipped.
func (r *HTTPRouteReconciler) backendZones(ctx context.Context, nodeNames []string) (map[string]struct{}, error) {
	zones := map[string]struct{}{}

	for _, nodeName := range nodeNames {
		node := &corev1.Node{}
		if err := r.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}

			return nil, err
		}

		zone := strings.TrimSpace(node.Labels[zoneLabel])
		if zone == "" {
			continue
		}

		zones[zone] = struct{}{}
	}

	return zones, nil
}

// ingressTargetsForZones returns all unique publish targets and node names for
// ingress-role nodes that belong to any backend zone.
func (r *HTTPRouteReconciler) ingressTargetsForZones(ctx context.Context, zones map[string]struct{}) ([]string, []string, error) {
	nodeList := &corev1.NodeList{}
	if err := r.List(ctx, nodeList); err != nil {
		return nil, nil, err
	}

	targetSet := map[string]struct{}{}
	nodeSet := map[string]struct{}{}

	for i := range nodeList.Items {
		node := &nodeList.Items[i]
		if !isIngressNode(node) {
			continue
		}

		zone := strings.TrimSpace(node.Labels[zoneLabel])
		if zone == "" {
			continue
		}
		if _, ok := zones[zone]; !ok {
			continue
		}

		target := resolveNodeTargetFromNode(node)
		if target == "" {
			continue
		}

		targetSet[target] = struct{}{}
		nodeSet[node.Name] = struct{}{}
	}

	targets := make([]string, 0, len(targetSet))
	for target := range targetSet {
		targets = append(targets, target)
	}
	slices.Sort(targets)

	nodes := make([]string, 0, len(nodeSet))
	for nodeName := range nodeSet {
		nodes = append(nodes, nodeName)
	}
	slices.Sort(nodes)

	return targets, nodes, nil
}

// resolveNodeTargetFromNode returns the publish target for a node.
// Priority order: direct-dns annotation, k3s annotations/addresses for
// external DNS and IP, then InternalIP.
func resolveNodeTargetFromNode(node *corev1.Node) string {
	if target, ok := node.Annotations[nodeTargetAnn]; ok {
		trimmed := strings.TrimSpace(target)
		if trimmed != "" {
			return trimmed
		}
	}

	if target, ok := node.Annotations[k3sExternalDNSAnn]; ok {
		trimmed := strings.TrimSpace(target)
		if trimmed != "" {
			return trimmed
		}
	}

	for _, address := range node.Status.Addresses {
		if address.Type == corev1.NodeExternalDNS {
			trimmed := strings.TrimSpace(address.Address)
			if trimmed != "" {
				return trimmed
			}
		}
	}

	if target, ok := node.Annotations[k3sExternalIPAnn]; ok {
		trimmed := strings.TrimSpace(target)
		if trimmed != "" {
			return trimmed
		}
	}

	for _, address := range node.Status.Addresses {
		if address.Type == corev1.NodeExternalIP {
			trimmed := strings.TrimSpace(address.Address)
			if trimmed != "" {
				return trimmed
			}
		}
	}

	for _, address := range node.Status.Addresses {
		if address.Type == corev1.NodeInternalIP {
			trimmed := strings.TrimSpace(address.Address)
			if trimmed != "" {
				return trimmed
			}
		}
	}

	return ""
}

// upsertDNSEndpoint writes one endpoint per hostname and record type.
// Targets are grouped by record type so mixed IPv4/IPv6/CNAME values are emitted correctly.
func (r *HTTPRouteReconciler) upsertDNSEndpoint(
	ctx context.Context,
	route *gatewaynetworkingv1.HTTPRoute,
	hostnames []string,
	targets []string,
	recordTTL int64,
) error {
	dnsEndpoint := &externaldnsv1alpha1.DNSEndpoint{}
	dnsEndpoint.Namespace = route.Namespace
	dnsEndpoint.Name = dnsEndpointName(types.NamespacedName{Namespace: route.Namespace, Name: route.Name})

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, dnsEndpoint, func() error {
		targetsByRecordType := map[string][]string{}
		for _, target := range targets {
			recordType := recordTypeForTarget(target)
			targetsByRecordType[recordType] = append(targetsByRecordType[recordType], target)
		}

		recordTypes := make([]string, 0, len(targetsByRecordType))
		for recordType := range targetsByRecordType {
			recordTypes = append(recordTypes, recordType)
		}
		slices.Sort(recordTypes)

		records := make([]*endpoint.Endpoint, 0, len(hostnames)*len(recordTypes))
		for _, hostname := range hostnames {
			for _, recordType := range recordTypes {
				typeTargets := targetsByRecordType[recordType]
				slices.Sort(typeTargets)

				record := endpoint.NewEndpointWithTTL(
					hostname,
					recordType,
					endpoint.TTL(recordTTL),
					typeTargets...,
				)
				if record != nil {
					records = append(records, record)
				}
			}
		}

		dnsEndpoint.Spec.Endpoints = records
		dnsEndpoint.Labels = map[string]string{
			"app.kubernetes.io/managed-by": "k8s-direct-dns",
		}

		return controllerutil.SetControllerReference(route, dnsEndpoint, r.Scheme)
	})

	return err
}

// isIngressNode supports both modern and legacy role label conventions.
func isIngressNode(node *corev1.Node) bool {
	if _, ok := node.Labels[ingressRoleLabel]; ok {
		return true
	}

	return node.Labels[legacyRoleLabel] == "ingress"
}

func (r *HTTPRouteReconciler) deleteDNSEndpoint(ctx context.Context, namespace, name string) error {
	dnsEndpoint := &externaldnsv1alpha1.DNSEndpoint{}

	err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, dnsEndpoint)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}

		return err
	}

	return r.Delete(ctx, dnsEndpoint)
}

func routeEnabled(route *gatewaynetworkingv1.HTTPRoute) bool {
	raw, ok := route.Annotations[annotationEnabled]
	if !ok || raw == "" {
		return true
	}

	enabled, err := strconv.ParseBool(raw)
	if err != nil {
		return true
	}

	return enabled
}

func routeHostnames(route *gatewaynetworkingv1.HTTPRoute) []string {
	hostnameSet := map[string]struct{}{}

	for _, hostname := range route.Spec.Hostnames {
		value := strings.TrimSpace(string(hostname))
		if value == "" {
			continue
		}

		hostnameSet[value] = struct{}{}
	}

	if annotationHostnames, ok := route.Annotations["external-dns.alpha.kubernetes.io/hostname"]; ok {
		for hostname := range strings.SplitSeq(annotationHostnames, ",") {
			value := strings.TrimSpace(hostname)
			if value == "" {
				continue
			}

			hostnameSet[value] = struct{}{}
		}
	}

	hostnames := make([]string, 0, len(hostnameSet))
	for hostname := range hostnameSet {
		hostnames = append(hostnames, hostname)
	}
	slices.Sort(hostnames)

	return hostnames
}

func routeTTL(route *gatewaynetworkingv1.HTTPRoute) int64 {
	raw, ok := route.Annotations[annotationTTL]
	if !ok || raw == "" {
		return defaultRecordTTL
	}

	ttl, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || ttl <= 0 {
		return defaultRecordTTL
	}

	return ttl
}

func backendServiceNames(route *gatewaynetworkingv1.HTTPRoute) []string {
	serviceSet := map[string]struct{}{}

	for _, rule := range route.Spec.Rules {
		for _, backendRef := range rule.BackendRefs {
			if backendRef.Name == "" {
				continue
			}
			if backendRef.Group != nil && *backendRef.Group != "" {
				continue
			}
			if backendRef.Kind != nil && *backendRef.Kind != "Service" {
				continue
			}
			if backendRef.Namespace != nil && string(*backendRef.Namespace) != route.Namespace {
				continue
			}

			serviceSet[string(backendRef.Name)] = struct{}{}
		}
	}

	serviceNames := make([]string, 0, len(serviceSet))
	for serviceName := range serviceSet {
		serviceNames = append(serviceNames, serviceName)
	}
	slices.Sort(serviceNames)

	return serviceNames
}

func recordTypeForTarget(target string) string {
	ip := net.ParseIP(target)
	if ip == nil {
		return "CNAME"
	}
	if ip.To4() != nil {
		return "A"
	}

	return "AAAA"
}

func dnsEndpointName(name types.NamespacedName) string {
	hash := sha1.Sum([]byte(name.String()))
	hashSuffix := hex.EncodeToString(hash[:])[:8]

	maxRouteLen := 63 - len("httproute--") - len(hashSuffix)
	routeName := name.Name
	if len(routeName) > maxRouteLen {
		routeName = routeName[:maxRouteLen]
	}

	return fmt.Sprintf("httproute-%s-%s", routeName, hashSuffix)
}
