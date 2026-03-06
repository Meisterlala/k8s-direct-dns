# k8s-direct-dns

`k8s-direct-dns` is a Kubernetes controller that computes DNS targets for `HTTPRoute` resources from real backend placement.

It is built for clusters where you want DNS to follow where the route backend is actually running (node-level awareness), while still letting external-dns do provider updates.

## What It Does

For each `gateway.networking.k8s.io/v1` `HTTPRoute`, the controller:

1. Reads route hostnames (`spec.hostnames` and `external-dns.alpha.kubernetes.io/hostname`)
2. Resolves backend `Service` references from route rules
3. Reads matching `EndpointSlice` objects and counts ready endpoints per node
4. Selects one node deterministically (highest ready endpoint count, then name tie-break)
5. Resolves a publishable target from that node (`ExternalIP`, fallback `InternalIP`, or explicit node annotation override)
6. Writes/updates an `externaldns.k8s.io/v1alpha1` `DNSEndpoint`

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
- `Node.status.addresses` (`ExternalIP`, `InternalIP`)
- Standard topology labels remain available for cluster operations (`topology.kubernetes.io/zone`, `topology.kubernetes.io/region`, `kubernetes.io/hostname`)

No custom topology API is introduced.

## Shared Public IP Limitation (Important)

If two nodes share the same public IP (for example, `odin` and `raspberry-pi`), DNS cannot uniquely pin traffic to one of those nodes using A/AAAA records alone.

Behavior:

- The controller still selects a single node deterministically
- If selected nodes resolve to the same publish target IP, resulting DNS records are effectively shared

Optional mitigation:

- Set explicit per-node publish target via node annotation:
  - `directdns.meisterlala.dev/target: <ip-or-hostname>`

## Supported Annotations

On `HTTPRoute`:

- `directdns.meisterlala.dev/enabled`: `"true"|"false"` (default: true)
- `directdns.meisterlala.dev/ttl`: integer seconds (default: 60)
- `external-dns.alpha.kubernetes.io/hostname`: comma-separated additional hostnames

On `Node`:

- `directdns.meisterlala.dev/target`: explicit DNS target override for that node

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

- `ghcr.io/meisterlala/k8s-direct-dns:latest` on `main`
- `ghcr.io/meisterlala/k8s-direct-dns:sha-<shortsha>` on `main`

After publishing, the workflow also updates `config/manager/manager.yaml` to pin the Deployment image to the pushed digest and commits that change back to `main`.

This lets Flux deploy directly from this repository (`./config/default`) while still automatically rolling out new controller builds.

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
