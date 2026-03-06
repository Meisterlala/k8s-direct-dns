# k8s-direct-dns

`k8s-direct-dns` is a Kubernetes controller that computes DNS targets for `HTTPRoute` resources from real backend placement.

It is built for clusters where you want DNS to follow where the route backend is actually running (node-level awareness), while still letting external-dns do provider updates.

## What It Does

For each `gateway.networking.k8s.io/v1` `HTTPRoute`, the controller:

1. Reads route hostnames (`spec.hostnames` and `external-dns.alpha.kubernetes.io/hostname`)
2. Resolves backend `Service` references from route rules
3. Reads matching `EndpointSlice` objects and collects ready backend node placement
4. Resolves backend zones from backend node labels (`topology.kubernetes.io/zone`)
5. Selects all nodes with role `ingress` in those backend zones
6. Resolves publishable targets for those ingress nodes (`ExternalIP`, fallback `InternalIP`, or explicit node annotation override)
7. Writes one or more DNS targets per hostname (multi-value A/AAAA/CNAME records as needed)
8. Writes/updates an `externaldns.k8s.io/v1alpha1` `DNSEndpoint`

external-dns then reads those `DNSEndpoint` objects and syncs records to your DNS provider.

## Why DNSEndpoint Instead of HTTPRoute Target Annotation

external-dns Gateway sources only read `external-dns.alpha.kubernetes.io/target` from the `Gateway` object, not per `HTTPRoute` target values.

To support per-route, per-backend-node DNS targets, this controller writes `DNSEndpoint` CRs and external-dns consumes them via `--source=crd`.

## Architecture

Data flow:

`HTTPRoute -> Service backend refs -> EndpointSlice endpoints -> Node -> DNSEndpoint -> external-dns -> DNS provider`

Reconcile triggers:

- `HTTPRoute` changes
- `EndpointSlice` changes (reconcile affected routes)
- `Node` changes (reconcile all routes)

## Topology Usage

The controller relies on existing Kubernetes topology and endpoint data:

- `EndpointSlice.endpoints[].nodeName`
- `EndpointSlice.endpoints[].conditions.ready`
- `Node.labels[topology.kubernetes.io/zone]`
- Ingress role labels on nodes (`node-role.kubernetes.io/ingress` or legacy `kubernetes.io/role=ingress`)
- `Node.status.addresses` (`ExternalDNS`, `ExternalIP`, `InternalIP`)
- Standard topology labels remain available for cluster operations (`topology.kubernetes.io/zone`, `topology.kubernetes.io/region`, `kubernetes.io/hostname`)

No custom topology API is introduced.

## Shared Public IP Behavior (Important)

If two selected ingress nodes resolve to the same publish IP (for example, behind the same NAT), DNS records will contain that target only once.

Behavior:

- The controller selects all ingress nodes in backend zones
- Duplicate targets are de-duplicated before writing DNS records

Optional mitigation:

- Set explicit per-node publish target via node annotation:
  - `directdns.meisterlala.dev/target: <ip-or-hostname>`

## Supported Annotations

On `HTTPRoute`:

- `directdns.meisterlala.dev/enabled`: `"true"|"false"` (default: true)
- `directdns.meisterlala.dev/ttl`: integer seconds (default: 1)
- `external-dns.alpha.kubernetes.io/hostname`: comma-separated additional hostnames

On `Node`:

- `directdns.meisterlala.dev/target`: explicit DNS target override for that node

Node target resolution order:

1. `directdns.meisterlala.dev/target`
2. `k3s.io/external-dns`
3. `Node.status.addresses[type=ExternalDNS]`
4. `k3s.io/external-ip`
5. `Node.status.addresses[type=ExternalIP]`
6. `Node.status.addresses[type=InternalIP]`

## external-dns Configuration

Enable CRD source in external-dns and point it to `DNSEndpoint`:

```sh
external-dns \
  --source=crd \
  --crd-source-apiversion=externaldns.k8s.io/v1alpha1 \
  --crd-source-kind=DNSEndpoint \
  --provider=<your-provider>
```

Ensure external-dns has RBAC permissions for `dnsendpoints.externaldns.k8s.io`.

## Getting Started

### Prerequisites

- Go `v1.24+`
- Docker `17.03+`
- kubectl `v1.11.3+`
- Access to a Kubernetes cluster
- Gateway API CRDs installed
- external-dns installed
- `DNSEndpoint` CRD installed (from external-dns)

### Build and Deploy Controller

```sh
make docker-build docker-push IMG=<some-registry>/k8s-direct-dns:tag
make deploy IMG=<some-registry>/k8s-direct-dns:tag
```

Examples:

```sh
make docker-build docker-push IMG=ghcr.io/<owner>/k8s-direct-dns:latest
make deploy IMG=ghcr.io/<owner>/k8s-direct-dns:latest
```

### Uninstall

```sh
make undeploy
```

## Distribution

### YAML Bundle

```sh
make build-installer IMG=<some-registry>/k8s-direct-dns:tag
```

This generates `dist/install.yaml`.

### Helm Chart

```sh
kubebuilder edit --plugins=helm/v2-alpha
```

## GitHub Image Automation

The repository includes a GitHub Actions workflow at `.github/workflows/image.yml` that builds and pushes a multi-arch image (`linux/amd64`, `linux/arm64`) to GHCR:

- `ghcr.io/<owner>/k8s-direct-dns:latest` on `master`
- `ghcr.io/<owner>/k8s-direct-dns:branch-<branch-name>` on branch pushes
- `ghcr.io/<owner>/k8s-direct-dns:sha-<shortsha>` on all builds

After publishing, the workflow also updates `config/manager/manager.yaml` to pin the Deployment image to the pushed digest and commits that change back to `master`.

This lets Flux deploy directly from this repository (`./config/default`) while still automatically rolling out new controller builds, regardless of cluster name or environment.

## Troubleshooting

- No `DNSEndpoint` created:
  - Route has no hostnames
  - Route has no in-namespace `Service` backend refs
  - Backend endpoints are not ready
  - Endpoints do not carry `nodeName`
- DNS target not what you expect:
  - Check selected node addresses (`ExternalIP` preferred, then `InternalIP`)
  - Check node override annotation `directdns.meisterlala.dev/target`
- Route should be ignored:
  - Set `directdns.meisterlala.dev/enabled: "false"`

## Development

```sh
make manifests generate
make lint-fix
make test
```

## License

Copyright 2026.

Licensed under the Apache License, Version 2.0.
