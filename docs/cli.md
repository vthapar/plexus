# Plexus CLI Reference

Plexus ships as both a standalone CLI (`plexus`) and a kubectl plugin
(`kubectl plexus`). Both are built from the same binary — `kubectl-plexus`.

## Installation

```bash
make build
# Copies bin/kubectl-plexus and symlinks bin/plexus

# To use as a kubectl plugin, place kubectl-plexus on your PATH:
cp bin/kubectl-plexus /usr/local/bin/
```

## Global Flags

| Flag | Description |
|------|-------------|
| `--kubeconfig <path>` | Path to kubeconfig file (defaults to `$KUBECONFIG` or `~/.kube/config`) |
| `--context <name>` | Kubeconfig context to use |

## Commands

### `plexus create`

Create an AdministrativeNetworkDomain with its first subnet. Add more
subnets with `add-subnet`.

```bash
plexus create <name> <subnet-name> --cidr <cidr> [--type <type>]
```

**Examples:**

```bash
plexus create production web --cidr 10.0.1.0/24 --type Public
plexus create staging app --cidr 10.0.10.0/24 --context kind-staging
```

### `plexus delete`

Delete an AdministrativeNetworkDomain and all its associated resources.
Prompts for confirmation unless `--yes` is specified.

```bash
plexus delete <name> [--yes]
```

**Flags:**

| Flag | Short | Description |
|------|-------|-------------|
| `--yes` | `-y` | Skip confirmation prompt |

**Examples:**

```bash
plexus delete staging
plexus delete staging --yes
```

### `plexus describe`

Show a detailed view of an AdministrativeNetworkDomain including its
subnets, availability zones, and status conditions.

```bash
plexus describe <name>
```

**Example output:**

```
Name:         production
Created:      2026-06-27T09:00:00Z (5m ago)
Subnets:      3

  SUBNET    CIDRS                    TYPE      AVAILABILITY ZONE
  web       10.0.1.0/24              Public    cluster(env=prod)
  backend   10.0.2.0/24,fd00::2/64   Private   <none>
  db        10.0.3.0/24              Isolated  cluster(env=prod), node(topology.kubernetes.io/zone=rack-a)

Conditions:
  TYPE    STATUS  REASON      MESSAGE                                      LAST TRANSITION
  Ready   True    Reconciled  All 3 subnets reconciled by ovnkube backend  2m ago
```

### `plexus add-subnet`

Add a subnet to an existing AdministrativeNetworkDomain.

```bash
plexus add-subnet <network-domain> <subnet-name> --cidr <cidr> [--type <type>]
```

**Flags:**

| Flag | Description |
|------|-------------|
| `--cidr <cidr>` | CIDR for the subnet (required, repeatable for dual-stack) |
| `--type <type>` | Subnet type: `Public`, `Private`, `Isolated`, `VPNOnly` (default: `Private`) |

**Validation:**

- CIDRs are validated with `net.ParseCIDR`
- CIDRs must use the network address (for example, `10.0.1.0/24`)
- At most two CIDRs (one IPv4, one IPv6 for dual-stack)
- Two CIDRs of the same address family are rejected
- Subnet type must be one of the known enum values
- Duplicate subnet names within an AND are rejected
- Subnet names and combined AND/subnet names must be valid DNS labels of at most 63 characters
- An AND can contain at most 100 subnets

**Examples:**

```bash
plexus add-subnet production web --cidr 10.0.1.0/24 --type Public
plexus add-subnet production backend --cidr 10.0.2.0/24
plexus add-subnet production dual --cidr 10.0.3.0/24 --cidr fd00::3/64 --type Private
```

### `plexus delete-subnet`

Delete a subnet from an AdministrativeNetworkDomain. This triggers the
controller to clean up all backend resources associated with the subnet.
The last subnet cannot be removed; delete the AND instead.
Prompts for confirmation unless `--yes` is specified.

```bash
plexus delete-subnet <network-domain> <subnet-name> [--yes]
```

**Flags:**

| Flag | Short | Description |
|------|-------|-------------|
| `--yes` | `-y` | Skip confirmation prompt |

**Examples:**

```bash
plexus delete-subnet production web
plexus delete-subnet production web --yes
```

### `plexus version`

Print the CLI version and Git commit.

```bash
plexus version
```

**Example output:**

```
plexus version v0.1.0 (commit: a1b2c3d)
```

Version and commit are extracted from module information stored in the binary.

## Using with kubectl

When `kubectl-plexus` is on your `PATH`, all commands work as kubectl
subcommands:

```bash
kubectl plexus create production web --cidr 10.0.1.0/24
kubectl plexus add-subnet production app --cidr 10.0.2.0/24
kubectl plexus describe production
kubectl plexus delete production --yes
```

## Using kubectl directly

Since AdministrativeNetworkDomain is a standard Kubernetes CRD, you can
also manage it with kubectl:

```bash
# List ANDs (uses printcolumn markers for READY and AGE)
kubectl get and

# Full YAML output
kubectl get and production -o yaml

# Apply from file (for complex specs with availability zones, etc.)
kubectl apply -f my-and.yaml

# Delete
kubectl delete and production
```
