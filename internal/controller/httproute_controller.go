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
	"sync"
	"time"

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
	annotationEnabled    = "directdns.meisterlala.dev/enabled"
	annotationTTL        = "directdns.meisterlala.dev/ttl"
	annotationResolve    = "directdns.meisterlala.dev/resolve-target-hostnames"
	annotationDNSServer  = "directdns.meisterlala.dev/dns-server"
	annotationStaleSince = "directdns.meisterlala.dev/stale-since"
	nodeTargetAnn        = "directdns.meisterlala.dev/target"
	k3sExternalDNSAnn    = "k3s.io/external-dns"
	k3sExternalIPAnn     = "k3s.io/external-ip"
	zoneLabel            = "topology.kubernetes.io/zone"
	ingressRoleLabel     = "node-role.kubernetes.io/ingress"
	legacyRoleLabel      = "kubernetes.io/role"
	defaultRecordTTL     = int64(1)
	defaultDNSServer     = "1.1.1.1:53"
)

// HTTPRouteReconciler reconciles a HTTPRoute object
type HTTPRouteReconciler struct {
	client.Client
	Scheme                   *runtime.Scheme
	DNSServer                string
	ResolveInterval          time.Duration
	ResolveByDefault         bool
	EnableIPv4               bool
	EnableIPv6               bool
	StaleEndpointGracePeriod time.Duration

	resolutionMu    sync.RWMutex
	resolvedHostIPs map[string][]string
	now             func() time.Time
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
		log.Info("Skipping HTTPRoute because no hostnames were found", "name", route.Name, "namespace", route.Namespace, "warning", true)
		return ctrl.Result{}, r.deleteDNSEndpoint(ctx, route.Namespace, dnsEndpointName(req.NamespacedName))
	}

	serviceNames := backendServiceNames(route)
	if len(serviceNames) == 0 {
		log.Info("Skipping HTTPRoute because no in-namespace Service backends were found", "name", route.Name, "namespace", route.Namespace, "warning", true)
		return ctrl.Result{}, r.deleteDNSEndpoint(ctx, route.Namespace, dnsEndpointName(req.NamespacedName))
	}

	targets, ingressNodeNames, backendZonesKnown, err := r.selectTargetsFromBackendZones(ctx, route.Namespace, serviceNames)
	if err != nil {
		return ctrl.Result{}, err
	}

	targets = r.resolvedTargetsForRoute(ctx, route, targets)

	targets = filterTargetsByIPFamily(targets, r.EnableIPv4, r.EnableIPv6)

	if len(targets) == 0 {
		if !backendZonesKnown {
			log.Info("Skipping HTTPRoute because no ready backends with zone labels were found", "name", route.Name, "namespace", route.Namespace, "warning", true)
			return r.reconcileNoValidTargets(ctx, route)
		}

		fallbackTargets, fallbackIngressNodes, err := r.allIngressTargets(ctx)
		if err != nil {
			return ctrl.Result{}, err
		}

		fallbackTargets = r.resolvedTargetsForRoute(ctx, route, fallbackTargets)
		fallbackTargets = filterTargetsByIPFamily(fallbackTargets, r.EnableIPv4, r.EnableIPv6)
		if len(fallbackTargets) == 0 {
			log.Info("Skipping HTTPRoute because no ingress nodes with publishable addresses were found", "name", route.Name, "namespace", route.Namespace, "warning", true)
			return r.reconcileNoValidTargets(ctx, route)
		}

		log.Info(
			"Falling back to all ingress nodes because no publishable ingress nodes were found in backend zones",
			"name", route.Name,
			"namespace", route.Namespace,
			"services", serviceNames,
			"fallbackNodes", fallbackIngressNodes,
			"fallbackTargets", fallbackTargets,
			"warning", true,
		)

		targets = fallbackTargets
		ingressNodeNames = fallbackIngressNodes
	}

	if err := r.upsertDNSEndpoint(ctx, route, hostnames, targets, routeTTL(route)); err != nil {
		return ctrl.Result{}, err
	}

	log.Info("Reconciled HTTPRoute DNS targets", "name", route.Name, "namespace", route.Namespace, "nodes", ingressNodeNames, "targets", targets)

	return ctrl.Result{}, nil
}

func (r *HTTPRouteReconciler) reconcileNoValidTargets(ctx context.Context, route *gatewaynetworkingv1.HTTPRoute) (ctrl.Result, error) {
	if r.StaleEndpointGracePeriod <= 0 {
		return ctrl.Result{}, r.deleteDNSEndpoint(ctx, route.Namespace, dnsEndpointName(types.NamespacedName{Namespace: route.Namespace, Name: route.Name}))
	}

	now := r.currentTime()
	log := logf.FromContext(ctx)
	dnsEndpoint := &externaldnsv1alpha1.DNSEndpoint{}
	key := types.NamespacedName{Namespace: route.Namespace, Name: dnsEndpointName(types.NamespacedName{Namespace: route.Namespace, Name: route.Name})}
	if err := r.Get(ctx, key, dnsEndpoint); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, err
	}

	if len(dnsEndpoint.Spec.Endpoints) == 0 {
		return ctrl.Result{}, r.deleteDNSEndpoint(ctx, route.Namespace, dnsEndpointName(types.NamespacedName{Namespace: route.Namespace, Name: route.Name}))
	}

	staleSince := now
	staleSinceRaw := ""
	shouldPersistStaleSince := false
	if dnsEndpoint.Annotations != nil {
		staleSinceRaw = strings.TrimSpace(dnsEndpoint.Annotations[annotationStaleSince])
	}

	if staleSinceRaw == "" {
		shouldPersistStaleSince = true
	} else {
		parsed, err := time.Parse(time.RFC3339Nano, staleSinceRaw)
		if err == nil {
			staleSince = parsed
		} else {
			shouldPersistStaleSince = true
			log.Info(
				"Could not parse stale-since annotation on DNSEndpoint, resetting grace timer",
				"namespace", route.Namespace,
				"name", route.Name,
				"dnsEndpoint", dnsEndpoint.Name,
				"staleSince", staleSinceRaw,
				"warning", true,
			)
		}
	}

	expiresAt := staleSince.Add(r.StaleEndpointGracePeriod)
	if now.Before(expiresAt) {
		if shouldPersistStaleSince {
			if dnsEndpoint.Annotations == nil {
				dnsEndpoint.Annotations = map[string]string{}
			}
			dnsEndpoint.Annotations[annotationStaleSince] = staleSince.Format(time.RFC3339Nano)
			if err := r.Update(ctx, dnsEndpoint); err != nil {
				return ctrl.Result{}, err
			}
		}

		remaining := expiresAt.Sub(now)
		if remaining <= 0 {
			remaining = time.Second
		}

		log.Info(
			"Keeping previous DNS targets because no valid targets were found",
			"namespace", route.Namespace,
			"name", route.Name,
			"dnsEndpoint", dnsEndpoint.Name,
			"gracePeriod", r.StaleEndpointGracePeriod,
			"expiresAt", expiresAt.Format(time.RFC3339),
			"warning", true,
		)

		return ctrl.Result{RequeueAfter: remaining}, nil
	}

	log.Info(
		"Removed DNSEndpoint after stale grace period elapsed",
		"namespace", route.Namespace,
		"name", route.Name,
		"dnsEndpoint", dnsEndpoint.Name,
		"staleSince", staleSince.Format(time.RFC3339),
		"gracePeriod", r.StaleEndpointGracePeriod,
		"warning", true,
	)

	return ctrl.Result{}, r.deleteDNSEndpoint(ctx, route.Namespace, dnsEndpointName(types.NamespacedName{Namespace: route.Namespace, Name: route.Name}))
}

func (r *HTTPRouteReconciler) currentTime() time.Time {
	if r.now != nil {
		return r.now().UTC()
	}

	return time.Now().UTC()
}

func (r *HTTPRouteReconciler) resolvedTargetsForRoute(ctx context.Context, route *gatewaynetworkingv1.HTTPRoute, targets []string) []string {
	if !routeResolveTargets(route, r.ResolveByDefault) {
		return targets
	}

	dnsServer := routeDNSServer(route, r.DNSServer)
	if dnsServer == "" {
		dnsServer = defaultDNSServer
	}

	resolvedSet := map[string]struct{}{}
	unresolved := make([]string, 0)

	for _, target := range targets {
		trimmed := strings.TrimSpace(target)
		if trimmed == "" {
			continue
		}

		if ip := net.ParseIP(trimmed); ip != nil {
			resolvedSet[trimmed] = struct{}{}
			continue
		}

		cacheKey := resolutionCacheKey(dnsServer, trimmed)
		if cached := r.getCachedResolvedIPs(cacheKey); len(cached) > 0 {
			for _, ip := range cached {
				resolvedSet[ip] = struct{}{}
			}

			continue
		}

		ips, err := resolveHostTarget(ctx, dnsServer, trimmed)
		if err != nil {
			unresolved = append(unresolved, trimmed)
			continue
		}
		r.setCachedResolvedIPs(cacheKey, ips)

		for _, ip := range ips {
			resolvedSet[ip] = struct{}{}
		}
	}

	if len(resolvedSet) == 0 {
		fallback := append([]string(nil), targets...)
		slices.Sort(fallback)
		return fallback
	}

	resolved := make([]string, 0, len(resolvedSet))
	for target := range resolvedSet {
		resolved = append(resolved, target)
	}
	slices.Sort(resolved)

	if len(unresolved) > 0 {
		logf.FromContext(ctx).Info(
			"Could not resolve some hostname targets and skipped them",
			"namespace", route.Namespace,
			"name", route.Name,
			"dnsServer", dnsServer,
			"unresolvedTargets", unresolved,
			"warning", true,
		)
	}

	return resolved
}

func resolveHostTarget(ctx context.Context, dnsServer, host string) ([]string, error) {
	dnsServer = normalizeDNSServer(dnsServer)
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			dialer := &net.Dialer{}
			return dialer.DialContext(ctx, network, dnsServer)
		},
	}

	lookupHost := strings.TrimSuffix(strings.TrimSpace(host), ".")
	ips, err := resolver.LookupIPAddr(ctx, lookupHost)
	if err != nil {
		return nil, err
	}

	set := map[string]struct{}{}
	for _, ip := range ips {
		if ip.IP == nil {
			continue
		}

		set[ip.IP.String()] = struct{}{}
	}

	if len(set) == 0 {
		return nil, fmt.Errorf("no IP addresses found for hostname %q", host)
	}

	resolved := make([]string, 0, len(set))
	for ip := range set {
		resolved = append(resolved, ip)
	}
	slices.Sort(resolved)

	return resolved, nil
}

func routeResolveTargets(route *gatewaynetworkingv1.HTTPRoute, defaultEnabled bool) bool {
	raw, ok := route.Annotations[annotationResolve]
	if !ok || raw == "" {
		return defaultEnabled
	}

	enabled, err := strconv.ParseBool(raw)
	if err != nil {
		return defaultEnabled
	}

	return enabled
}

func routeDNSServer(route *gatewaynetworkingv1.HTTPRoute, fallback string) string {
	if raw, ok := route.Annotations[annotationDNSServer]; ok {
		trimmed := strings.TrimSpace(raw)
		if trimmed != "" {
			return normalizeDNSServer(trimmed)
		}
	}

	return normalizeDNSServer(fallback)
}

func normalizeDNSServer(server string) string {
	trimmed := strings.TrimSpace(server)
	if trimmed == "" {
		return ""
	}

	if strings.Contains(trimmed, ":") {
		return trimmed
	}

	return trimmed + ":53"
}

func filterTargetsByIPFamily(targets []string, enableIPv4, enableIPv6 bool) []string {
	filtered := make([]string, 0, len(targets))
	for _, target := range targets {
		recordType := recordTypeForTarget(target)
		switch recordType {
		case "A":
			if !enableIPv4 {
				continue
			}
		case "AAAA":
			if !enableIPv6 {
				continue
			}
		}

		filtered = append(filtered, target)
	}

	return filtered
}

func resolutionCacheKey(dnsServer, host string) string {
	return normalizeDNSServer(dnsServer) + "|" + strings.TrimSuffix(strings.TrimSpace(host), ".")
}

func (r *HTTPRouteReconciler) getCachedResolvedIPs(key string) []string {
	r.resolutionMu.RLock()
	defer r.resolutionMu.RUnlock()

	if r.resolvedHostIPs == nil {
		return nil
	}

	ips := r.resolvedHostIPs[key]
	if len(ips) == 0 {
		return nil
	}

	copyIPs := make([]string, len(ips))
	copy(copyIPs, ips)
	return copyIPs
}

func (r *HTTPRouteReconciler) setCachedResolvedIPs(key string, ips []string) {
	if len(ips) == 0 {
		return
	}

	copyIPs := make([]string, len(ips))
	copy(copyIPs, ips)
	slices.Sort(copyIPs)

	r.resolutionMu.Lock()
	defer r.resolutionMu.Unlock()

	if r.resolvedHostIPs == nil {
		r.resolvedHostIPs = map[string][]string{}
	}

	r.resolvedHostIPs[key] = copyIPs
}

func (r *HTTPRouteReconciler) compareAndSetCachedResolvedIPs(key string, ips []string) bool {
	copyIPs := make([]string, len(ips))
	copy(copyIPs, ips)
	slices.Sort(copyIPs)

	r.resolutionMu.Lock()
	defer r.resolutionMu.Unlock()

	if r.resolvedHostIPs == nil {
		r.resolvedHostIPs = map[string][]string{}
	}

	current := r.resolvedHostIPs[key]
	if slices.Equal(current, copyIPs) {
		return false
	}

	r.resolvedHostIPs[key] = copyIPs
	return true
}

// SetupWithManager sets up the controller with the Manager.
func (r *HTTPRouteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.resolvedHostIPs == nil {
		r.resolvedHostIPs = map[string][]string{}
	}

	if r.now == nil {
		r.now = time.Now
	}

	if err := ctrl.NewControllerManagedBy(mgr).
		For(&gatewaynetworkingv1.HTTPRoute{}).
		Watches(&discoveryv1.EndpointSlice{}, handler.EnqueueRequestsFromMapFunc(r.mapEndpointSliceToHTTPRoutes)).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.mapNodeToHTTPRoutes)).
		Named("httproute").
		Complete(r); err != nil {
		return err
	}

	return mgr.Add(r)
}

// NeedLeaderElection ensures only the leader runs periodic DNS checks.
func (r *HTTPRouteReconciler) NeedLeaderElection() bool {
	return true
}

// Start periodically refreshes resolved hostname IPs and reconciles affected routes.
func (r *HTTPRouteReconciler) Start(ctx context.Context) error {
	if r.ResolveInterval <= 0 {
		<-ctx.Done()
		return nil
	}

	ticker := time.NewTicker(r.ResolveInterval)
	defer ticker.Stop()

	log := logf.FromContext(ctx)
	log.Info("Starting periodic hostname resolution refresh", "interval", r.ResolveInterval)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := r.refreshResolvedHostCache(ctx); err != nil {
				log.Error(err, "Could not refresh hostname resolution cache")
			}
		}
	}
}

func (r *HTTPRouteReconciler) refreshResolvedHostCache(ctx context.Context) error {
	log := logf.FromContext(ctx)

	routeList := &gatewaynetworkingv1.HTTPRouteList{}
	if err := r.List(ctx, routeList); err != nil {
		return err
	}

	watchers := map[string][]types.NamespacedName{}
	for i := range routeList.Items {
		route := &routeList.Items[i]
		if !routeEnabled(route) || !routeResolveTargets(route, r.ResolveByDefault) {
			continue
		}

		serviceNames := backendServiceNames(route)
		if len(serviceNames) == 0 {
			continue
		}

		targets, _, _, err := r.selectTargetsFromBackendZones(ctx, route.Namespace, serviceNames)
		if err != nil {
			log.Error(err, "Could not collect targets for route during periodic resolution", "namespace", route.Namespace, "name", route.Name)
			continue
		}
		if len(targets) == 0 {
			fallbackTargets, _, fallbackErr := r.allIngressTargets(ctx)
			if fallbackErr != nil {
				log.Error(fallbackErr, "Could not collect fallback ingress targets for route during periodic resolution", "namespace", route.Namespace, "name", route.Name)
				continue
			}
			targets = fallbackTargets
		}

		dnsServer := routeDNSServer(route, r.DNSServer)
		if dnsServer == "" {
			dnsServer = defaultDNSServer
		}

		for _, target := range targets {
			trimmed := strings.TrimSpace(target)
			if trimmed == "" || net.ParseIP(trimmed) != nil {
				continue
			}

			cacheKey := resolutionCacheKey(dnsServer, trimmed)
			watchers[cacheKey] = append(watchers[cacheKey], types.NamespacedName{Namespace: route.Namespace, Name: route.Name})
		}
	}

	changedRoutes := map[types.NamespacedName]struct{}{}
	for cacheKey, routeKeys := range watchers {
		delimiter := strings.Index(cacheKey, "|")
		if delimiter <= 0 || delimiter >= len(cacheKey)-1 {
			continue
		}

		dnsServer := cacheKey[:delimiter]
		host := cacheKey[delimiter+1:]

		ips, err := resolveHostTarget(ctx, dnsServer, host)
		if err != nil {
			if len(r.getCachedResolvedIPs(cacheKey)) > 0 {
				log.Info("Could not resolve hostname target during periodic refresh, keeping last known good IPs", "host", host, "dnsServer", dnsServer, "warning", true)
			}
			continue
		}

		if !r.compareAndSetCachedResolvedIPs(cacheKey, ips) {
			continue
		}

		for _, routeKey := range routeKeys {
			changedRoutes[routeKey] = struct{}{}
		}
	}

	if len(changedRoutes) == 0 {
		return nil
	}

	log.Info("Detected changed hostname IPs, reconciling affected routes", "routeCount", len(changedRoutes))
	for routeKey := range changedRoutes {
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: routeKey})
		if err != nil {
			log.Error(err, "Could not reconcile route after hostname IP change", "namespace", routeKey.Namespace, "name", routeKey.Name)
		}
	}

	return nil
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
func (r *HTTPRouteReconciler) selectTargetsFromBackendZones(ctx context.Context, namespace string, services []string) ([]string, []string, bool, error) {
	backendNodeNames, err := r.readyBackendNodeNames(ctx, namespace, services)
	if err != nil {
		return nil, nil, false, err
	}
	if len(backendNodeNames) == 0 {
		return nil, nil, false, nil
	}

	backendZones, err := r.backendZones(ctx, backendNodeNames)
	if err != nil {
		return nil, nil, false, err
	}
	if len(backendZones) == 0 {
		return nil, nil, false, nil
	}

	targets, nodes, err := r.ingressTargetsForZones(ctx, backendZones)
	if err != nil {
		return nil, nil, true, err
	}

	return targets, nodes, true, nil
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

// allIngressTargets returns all unique publish targets and node names for ingress-role nodes.
func (r *HTTPRouteReconciler) allIngressTargets(ctx context.Context) ([]string, []string, error) {
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
		if dnsEndpoint.Annotations == nil {
			dnsEndpoint.Annotations = map[string]string{}
		}
		delete(dnsEndpoint.Annotations, annotationStaleSince)
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
