# Plexus

**Plexus** is a network orchestrator that provides VPC-like isolation on
Kubernetes via the **AdministrativeNetworkDomain** (AND) CRD.

It translates high-level network intent — subnets, routing, security groups,
gateways — into backend-specific resources. [OVN-Kubernetes](https://github.com/ovn-kubernetes/ovn-kubernetes)
is the reference backend.

> **Status:** Early PoC. The API is `v1beta1` and subject to change.

## Quick Start

```bash
# Build
make build

# Install the CLI (standalone)
cp bin/plexus /usr/local/bin/
# Or as a kubectl plugin
cp bin/kubectl-plexus /usr/local/bin/

# Install the CRD
kubectl apply -f config/crd/plexus.io_administrativenetworkdomains.yaml

# Create an AND with its first subnet, then add more subnets
plexus create production web --cidr 10.0.1.0/24 --type Public
plexus add-subnet production app --cidr 10.0.10.0/24 --type Private
plexus add-subnet production db  --cidr 10.0.20.0/24 --type Isolated

# List ANDs
kubectl get and

# Describe an AND
plexus describe production
```

The Helm chart selects an FRRConfiguration labeled
`plexus.io/frr-configuration: base`, which the KIND scripts create. Set
`ovnKubernetes.frrConfigurationSelector` in your chart values when using a
different FRRConfiguration.

## What It Does

When you create an AND with subnets, the Plexus controller (via the
OVN-Kubernetes backend):

1. Creates a **namespace** per subnet (`<and-name>-<subnet-name>`)
2. Creates a **UDN** (L2 EVPN) in each namespace
3. Creates **RouteAdvertisements** for Public subnets (BGP export)
4. Sets up **intra-domain routing** between non-Isolated subnets
5. Aggregates **status** from all subnets into the AND status

## Build

```bash
make build       # → bin/plexus-controller, bin/kubectl-plexus
make test        # Unit tests
make lint        # Format + vet + golangci-lint
make docker-build
```

## Documentation

- [CLI Reference](docs/cli.md) — full command reference for `plexus` / `kubectl plexus`

## Design

See [OKEP-6557](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6557)
for the full design document.

## License

Apache License 2.0. See [LICENSE](LICENSE).
