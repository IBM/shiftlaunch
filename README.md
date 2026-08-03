# ShiftLaunch

ShiftLaunch is a turnkey orchestration agent for deploying Red Hat OpenShift clusters on IBM Power Systems (`ppc64le`). It bridges local infrastructure services (DNS, DHCP, HAProxy) with IBM's Hardware Management Console (HMC) REST API, providing a Docker-like CLI experience for the full cluster lifecycle: deploy, scale, and teardown.

## Table of Contents

- [Key Features](#key-features)
- [Installation](#installation)
- [Quick Start](#quick-start)
- [Command Reference](#command-reference)
- [Architecture Overview](#architecture-overview)
- [Prerequisites](#prerequisites)
- [Bring Your Own Infrastructure (BYOI)](#bring-your-own-infrastructure-byoi)
- [Disconnected and Airgapped Deployments](#disconnected-and-airgapped-deployments)
- [State Management and Idempotency](#state-management-and-idempotency)
- [Safe Teardown Lifecycle](#safe-teardown-lifecycle)
- [Example Configurations](#example-configurations)
- [Troubleshooting](#troubleshooting)
- [Authors](#authors)
- [License](#license)

---

## Key Features

- **Infrastructure as Code** — Automatically configures local `dnsmasq`, `haproxy`, `squid`, and `podman` registries from a single `config.yaml`.
- **Intelligent HMC Orchestration** — Interacts with the IBM HMC REST API to discover LPARs, mount Virtual Optical Media (ISO) via NFS to the VIOS, and automate boot sequences.
- **Airgap and Disconnected Support** — Built-in `oc-mirror` v2 integration to stand up local container registries and mirror OpenShift payloads for fully disconnected deployments.
- **Idempotent Auto-Resume** — Tracks deployment phases in `state.json`. If a crash or network drop occurs, ShiftLaunch resumes at the failed phase without repeating completed work.
- **Day-2 Scaling** — Append worker nodes to your config and run `shiftlaunch scale` to grow an existing cluster.
- **BYOI (Bring Your Own Infrastructure)** — Use existing enterprise load balancers, DNS, and DHCP, or let ShiftLaunch manage them locally.

---

## Installation

### Download a Pre-Built Binary

1. Go to the [releases page](https://github.com/IBM/shiftlaunch/releases).
2. Download the binary for your platform from the **Assets** section.
3. Install it:

```bash
# Make executable (Linux/macOS)
chmod +x shiftlaunch

# Move to PATH
sudo mv shiftlaunch /usr/local/bin/

# Verify
shiftlaunch --help
```

### Build from Source

Requires **Go 1.22+**. ShiftLaunch cross-compiles without CGO.

```bash
git clone https://github.com/IBM/shiftlaunch.git
cd shiftlaunch

make build
make install
```

> **Cross-compiling for IBM Power from macOS or Windows:**
> ```bash
> make build-ppc64le
> ```

---

## Quick Start

### 1. Generate a Configuration File

```bash
# Standard connected cluster using the Agent-based installer
shiftlaunch generate-config -t multi -b agent -o my-cluster.yaml

# Fully airgapped cluster using network boot
shiftlaunch generate-config -t multi -b netboot --disconnected -o my-cluster.yaml
```

### 2. Validate Your Infrastructure

Run pre-flight checks against your network, disk space, and HMC before deploying.

```bash
shiftlaunch validate --config my-cluster.yaml
```

### 3. Deploy the Cluster

```bash
shiftlaunch create --config my-cluster.yaml
```

If the deployment is interrupted, re-running this command resumes from the last failed phase.

### 4. Monitor Progress

```bash
# Stream live deployment logs
shiftlaunch logs -f --cluster my-cluster

# Check status and retrieve credentials
shiftlaunch status --cluster my-cluster
```

---

## Command Reference

### Core Commands

| Command | Description |
|---------|-------------|
| `create` | Run the cluster deployment pipeline; auto-resumes from the last failed phase |
| `scale` | Add new worker nodes to an existing cluster |
| `remove` / `delete` | Power off LPARs and remove all local service configurations |
| `list` | List all managed clusters in the workspace |
| `status` / `info` | Show cluster endpoints, credentials, and current deployment phase |
| `logs` | Fetch or stream (`-f`) deployment logs |
| `prune` | Reclaim disk space from clusters marked as deleted |

### Utility Commands

| Command | Description |
|---------|-------------|
| `validate` | Run pre-flight checks against `config.yaml` and HMC infrastructure |
| `generate-config` | Generate a starter `config.yaml` for a given topology |
| `service-configs` | Print external service configurations for network admins |
| `export kubeconfig` | Write the cluster `kubeconfig` to your local environment |
| `oc` | Run `oc` commands scoped to a specific managed cluster |

---

## Architecture Overview

### Boot Methods

ShiftLaunch supports two boot mechanisms. Agent-based is recommended for most deployments.

#### Agent-Based Installer (Recommended)

- Single ISO contains all installation artifacts — no separate PXE or DHCP server required.
- Static IPs are applied via NMState, removing dependency on network-side DHCP.
- ISO is served over NFS from the controller directly to the VIOS.
- All artifacts can be embedded in the ISO for fully disconnected deployments.
- ISO mappings and NFS exports are removed automatically after installation completes.

#### User Provisioned Infrastructure (UPI / Network Boot)

- Traditional PXE-based UPI workflow.
- Automated netboot triggered via the HMC REST API.
- Full network boot stack (DHCP, TFTP, HTTP) managed locally by ShiftLaunch.
- Per-node GRUB configurations generated automatically from MAC addresses.
- Requires one extra bootstrap LPAR that ShiftLaunch removes after the control plane is healthy.

### Network Isolation Topologies

| Mode | Key ID | Behavior |
|------|--------|----------|
| Connected | `connected` | Nodes pull directly from `quay.io` |
| Restricted Network | `restricted-network` | Nodes are isolated; traffic exits through a local Squid proxy or corporate gateway |
| Air-Gapped | `air-gapped` | Strict isolation. All proxy variables are scrubbed. A local Podman registry is created and seeded via `oc-mirror` v2 |

### Single VIP Architecture

ShiftLaunch uses one IP address per cluster instead of the traditional split API + Ingress VIP pair.

- HAProxy routes Layer 4 TCP traffic by port: `6443` (API), `22623` (Machine Config), `80`/`443` (Ingress).
- All DNS records (`api`, `api-int`, `*.apps`) resolve to the single VIP.
- The VIP is aliased to the controller's network interface and unbound cleanly on `delete`.

---

## Prerequisites

### Infrastructure

**1. Load Balancer VIP**

One dedicated IP address per cluster, in the same subnet as the cluster nodes. ShiftLaunch aliases this IP to the controller interface at deploy time.

**2. IBM Power LPARs**

| Deployment Type | Boot Method | Min LPARs | Node Layout |
|-----------------|-------------|-----------|-------------|
| Single Node OpenShift (SNO) | ISO or Netboot | 1 | 1 combined control-plane + worker |
| Multi-Node | Agent ISO | 5 | 3 masters + 2 workers |
| Multi-Node | Network Boot | 6 | 1 bootstrap + 3 masters + 2 workers |

> The bootstrap node used in Network Boot deployments is removed automatically once the control plane reaches a healthy state.

**3. Controller Node**

- OS: RHEL 9/10 or CentOS Stream 9/10
- Network access to HMC and all cluster nodes
- Disk: ~10 GB for connected deployments; ~60 GB or more for airgapped deployments (RHCOS images + agent ISO + oc-mirror output)

### Supported Component Versions

| Component | Supported Versions |
|-----------|--------------------|
| Controller OS | RHEL 9/10, CentOS Stream 9/10 |
| HMC | V11R2 (SP 1120, build 2604091530)<br>V11R1 (SP 1110, build 2502191030)<br>V10R3 M1063 |
| IBM Power Firmware | RB1120_fw1120.00<br>ML1060_fw1060.51 (148) |
| VIOS | 4.1.2.0, 4.1.1.10 |

---

## Bring Your Own Infrastructure (BYOI)

The `services` block in `config.yaml` controls which services ShiftLaunch manages locally. Three operational models are supported:

| Model | Description |
|-------|-------------|
| **Fully Managed** | ShiftLaunch installs and owns DNS, DHCP, PXE, HAProxy, and NFS on the controller |
| **Partially Managed** | Mix local and external services as needed |
| **BYOI** | All local services disabled; ShiftLaunch acts as a pure HMC orchestrator and installation monitor |

### Example: Fully Managed (Agent ISO Boot)

```yaml
services:
  dns:
    enabled: true
  dhcp:
    enabled: false        # Not needed for NMState-driven ISO boot
  pxe:
    enabled: false        # Not needed for ISO boot
  load_balancer:
    enabled: true
  nfs:
    enabled: true         # Required to host Agent ISOs on the VIOS
```

### Example: BYOI (Network Boot with Enterprise Services)

```yaml
services:
  dns:
    enabled: false        # Using enterprise InfoBlox
  dhcp:
    enabled: false        # Using enterprise DHCP
  pxe:
    enabled: false        # Using enterprise PXE server
  load_balancer:
    enabled: false        # Using enterprise F5 BIG-IP
  nfs:
    enabled: false
```

---

## Disconnected and Airgapped Deployments

ShiftLaunch can spin up a local Podman container registry and a Squid proxy on the controller to support environments with no or restricted outbound internet access.

| Architecture | Behavior | Typical Use Case |
|--------------|----------|-----------------|
| Standard | Pulls directly from `quay.io` | Datacenters with open internet access |
| Corp Proxy | Routes `quay.io` traffic through a local Squid proxy | Environments with enforced egress filtering |
| Strict Airgap | Local registry only; all proxy variables are scrubbed from nodes | Dark sites, classified environments |
| Soft Airgap | Local registry + local proxy | Sites that need local payloads but still reach external NTP, LDAP, or third-party operators |

> **CI / Nightly Builds:** Setting `release_type: "ci"` in your config causes ShiftLaunch to inject a `MachineConfig` that bypasses cryptographic signature validation. Use this only with trusted nightly payloads.

---

## State Management and Idempotency

All deployment state is persisted at `/opt/shiftlaunch/clusters/<name>/state.json`.

- **Locking** — A PID-aware file lock (`.lock`) uses `syscall.Signal(0)` to detect stale locks from crashed processes, preventing zombie lockouts.
- **Resume Logic** — Each phase (`downloads`, `services`, etc.) is marked complete in the state file when it succeeds. Re-running `shiftlaunch create` skips all completed phases and begins execution at the first incomplete or failed phase.
- **Self-Healing** — On load, the state manager validates the JSON schema and runs a `RecoverState` routine to remove duplicate events and repair corrupted phase histories.

---

## Safe Teardown Lifecycle

`shiftlaunch remove` (alias: `delete`) follows a strict ordered sequence to leave no orphaned infrastructure:

1. **LPAR Power Off** — Transitions all LPARs to a safe powered-off state before modifying storage.
2. **VIOS Cleanup** — Unmaps virtual optical media, deletes the ISO from the Media Repository, and removes the NFS mount.
3. **VIP Release** — Parses `ip` and `nmcli` output to cleanly unbind the cluster VIP from the controller interface without affecting other network configuration.
4. **Archival** — The cluster workspace is renamed to `.deleted` rather than immediately removed, preserving logs for post-mortem review. Run `shiftlaunch prune` when you are ready to reclaim disk space.

**Partial Failure Handling** — If a specific resource (LPAR, virtual disk, optical media) fails to delete, the failure is recorded in the state file. Re-running `delete` retries only the failed resources.

---

## Example Configurations

Pre-built examples are available in the [`example/`](./example/) directory:

| File | Description |
|------|-------------|
| [`config.yaml`](./example/config.yaml) | Basic multi-node cluster template |
| [`config-sno.yaml`](./example/config-sno.yaml) | Single-Node OpenShift (SNO) |
| [`config-disc-agent.yaml`](./example/config-disc-agent.yaml) | Disconnected deployment with Agent-based installer |
| [`config-multi-netboot.yaml`](./example/config-multi-netboot.yaml) | Multi-node cluster with traditional network boot |

---

## Troubleshooting

### "File 'config.yaml' already exists. Refusing to overwrite."

**Cause:** `generate-config` will not silently overwrite an existing file.

**Fix:** Specify a different output path or remove the existing file first.

```bash
shiftlaunch generate-config -t multi -b agent --config new-name.yaml
```

### "Cluster is already managed and fully deployed."

**Cause:** A `.managed` marker exists in the cluster workspace, indicating a healthy finished cluster. Running `create` on an already-managed cluster is blocked to prevent accidental data loss.

**Fix:** Explicitly delete the cluster first, then redeploy.

```bash
shiftlaunch delete --cluster my-cluster
shiftlaunch create --config my-cluster.yaml
```

---

## Authors

- **Sudeesh John** ([@sudeeshjohn](https://github.ibm.com/sudeeshjohn))

---

## License

This project is maintained under the `shiftlaunch` repository. See the [LICENSE](./LICENSE) file for details.
