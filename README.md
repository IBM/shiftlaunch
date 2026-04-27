# ShiftLaunch - OpenShift Deployer for IBM Power Systems

A comprehensive Go-based tool for deploying OpenShift clusters (SNO or Multi-Node) on IBM Power Systems using the User-Provisioned Infrastructure (UPI) method with **full automation**—from starter configuration generation to cluster ready state.

## Overview

ShiftLaunch provides **end-to-end automation** for OpenShift deployment on IBM Power Systems with support for two distinct boot methods:

### 🎯 Boot Methods

#### 1. Agent ISO Boot (Recommended for Production)
- ✅ **Simplified Deployment**: Single ISO contains all installation artifacts.
- ✅ **No DHCP/PXE Required**: Eliminates network boot complexity (uses static IPs via NMState).
- ✅ **NFS-Based**: ISO served via NFS from the controller directly to the VIOS.
- ✅ **Unified Installation**: Bootstrap and installation combined into a single phase.
- ✅ **Better for Disconnected**: All artifacts can be embedded in the ISO.
- ✅ **Automatic Cleanup**: ISO mappings and NFS exports are removed after installation.

#### 2. Network Boot (PXE/Netboot)
- ✅ **Traditional UPI**: Standard PXE boot workflow.
- ✅ **HMC REST API**: Automated netboot using IBM's Hardware Management Console.
- ✅ **DHCP/TFTP/HTTP**: Full network boot stack managed locally.
- ✅ **MAC-Based Config**: Automated, per-node GRUB configurations.
- ✅ **Flexible**: Supports custom kernel boot parameters.

### 🚀 Core Features
- ✅ **Bring Your Own Infrastructure (BYOI)**: Use existing DNS/DHCP/PXE/LoadBalancers or let ShiftLaunch manage them locally.
- ✅ **Smart Configuration Generator**: Interactive, dynamic template generation that adapts to your chosen topology and boot method.
- ✅ **Installation Monitoring**: Automated monitoring with `openshift-install wait-for` commands.
- ✅ **Single VIP Architecture**: One IP per cluster via port-based routing (50% IP savings).
- ✅ **Resume Functionality**: Safely resume failed deployments from the last completed phase.
- ✅ **State Management**: Tracks deployment progress with isolated JSON state files.
- ✅ **Enhanced Status Display**: Instantly view cluster nodes, IPs, endpoints, and credentials without SSH.

---

## Architecture

### Single VIP Architecture
- **One IP per Cluster**: Replaces the traditional dual VIP (API + Ingress) approach.
- **Port-Based Routing**: HAProxy routes traffic via Layer 4 TCP based strictly on ports (6443 for API, 22623 for Machine Config, 80/443 for Ingress).
- **Simplified DNS**: All DNS records (api, api-int, *.apps) point to a single VIP.

### Multi-Cluster Support
- **IP Aliasing**: Each cluster gets a dedicated VIP aliased to the controller's physical interface.
- **Workspace Isolation**: Separate directories, configs, and service instances located at `/opt/shiftlaunch/clusters/<cluster-name>/`.
- **HTTP Directory Structure**: Isolated `/var/www/html/<cluster-name>/` paths for hosting ignition and RHCOS payloads.

---

## Usage

### Quick Start

```bash
# 1. Generate a smart configuration template for an Agent ISO deployment
./shiftlaunch generate-config -type sno -boot iso -config my-sno.yaml

# 2. Edit the generated configuration with your infrastructure details
vi my-sno.yaml

# 3. Validate the configuration against the HMC and local controller
./shiftlaunch validate -config my-sno.yaml

# 4. Deploy the cluster
./shiftlaunch create -config my-sno.yaml

# 5. Check status (shows nodes, IPs, endpoints, and kubeadmin credentials)
./shiftlaunch status -cluster my-sno
```

### Command Reference

#### Generate Configuration Template
Create a highly-documented configuration template tailored exactly to your topology and boot method. The generator is "smart"—it automatically omits the `bootstrap` node and `rhcos_images` URLs if you select ISO boot, and toggles the required managed services (like NFS vs PXE) accordingly.

```bash
# Generate SNO configuration with Agent ISO boot (recommended)
./shiftlaunch generate-config -type sno -boot iso -config sno-cluster.yaml

# Generate SNO configuration with traditional Network Boot
./shiftlaunch generate-config -type sno -boot netboot -config sno-netboot.yaml

# Generate Multi-Node configuration with Agent ISO boot
./shiftlaunch generate-config -type multi -boot iso -config prod-cluster.yaml

# Generate Multi-Node configuration with traditional Network Boot
./shiftlaunch generate-config -type multi -boot netboot -config prod-netboot.yaml
```

#### Validate Configuration
Validate your YAML configuration, verify local controller disk space, check for VIP conflicts, and validate LPAR/Storage existence on the HMC:

```bash
./shiftlaunch validate -config my-cluster.yaml
```

#### Deploy Cluster
Deploy a new OpenShift cluster. If a previous deployment failed, running this command again will automatically detect the `state.json` file and safely resume from the last completed phase.

```bash
./shiftlaunch create -config my-cluster.yaml
```

#### Check Status
View real-time cluster status, node assignments, and connection credentials:

```bash
./shiftlaunch status -cluster my-cluster
```

**Status Output Includes:**
- Cluster deployment status and phase history.
- Cluster nodes with hostnames, roles, and IP addresses.
- Service endpoints (API, Console, OAuth, Prometheus, Grafana).
- A **Single-line `/etc/hosts` entry** for easy copy-pasting.
- Local path to your `kubeconfig` and `kubeadmin` password.

#### Delete Cluster
Safely tear down a deployed cluster. 

ShiftLaunch uses **Intelligent Partial Failure Handling**. It tracks exactly which resources (LPARs, Virtual Disks, Optical Media) fail to delete. If a deletion fails (e.g., a disk is locked), it preserves the failure in the state file. Re-running the delete command will safely retry *only* the failed resources, ensuring zero orphaned infrastructure.

```bash
./shiftlaunch delete -cluster my-cluster
```

#### Dump Configuration Requirements
If you opt to use external, unmanaged services (BYOI mode), generate the exact DNS, DHCP, and Load Balancer rules you need to hand off to your network administrators:

```bash
./shiftlaunch dump-config -config my-cluster.yaml
```

---

## Configuration

### Bring Your Own Infrastructure (BYOI)

ShiftLaunch supports flexible infrastructure management through the `managed_services` configuration block. You can choose to:

1. **Fully Managed** - ShiftLaunch manages all services locally on the controller (DNS, DHCP, PXE, HAProxy, NFS).
2. **Partially Managed** - Mix managed and external services.
3. **BYOI Mode** - Use all external services; ShiftLaunch only handles HMC LPAR provisioning, Ignition generation, and installation monitoring.

**Example: Fully Managed (ISO Boot)**
```yaml
managed_services:
  dns: true            
  dhcp: false          # Not required for NMState-driven ISO boot
  pxe: false           # Not required for ISO boot
  load_balancer: true  
  nfs: true            # Required to host Agent ISOs to the VIOS
```

**Example: BYOI Mode (Netboot)**
```yaml
managed_services:
  dns: false           # Using enterprise InfoBlox
  dhcp: false          # Using enterprise DHCP
  pxe: false           # Using enterprise PXE server
  load_balancer: false # Using enterprise F5 Big-IP
  nfs: false           
```

---

## Prerequisites

1. **Controller Node (Bastion)**:
   - RHEL 8/9 or compatible Linux.
   - Root SSH access.
   - Network routing to the Power Systems HMC and the target OpenShift subnets.
   - Sufficient disk space for RHCOS images and ISO generation (~10GB per cluster).

2. **HMC (Hardware Management Console)**:
   - REST API enabled.
   - User credentials with LPAR and VIOS management permissions.

3. **IBM Power Systems**:
   - Managed by the target HMC.
   - Sufficient Compute and Memory resources.
   - Virtual switches (`vswitch`) pre-configured.

4. **Storage**:
   - VIOS with an active Volume Group (excluding `rootvg`) with sufficient free space.

---

## Troubleshooting

### Common Issues

1. **"File 'config.yaml' already exists. Refusing to overwrite"**
   - **Cause**: You are running `generate-config` but the target file already exists.
   - **Solution**: Delete the existing file or specify a different output path using `-config new-name.yaml`.

2. **Network Boot "No Network Adapters Found" Error**
   - **Cause**: The LPAR exists but has no network adapter attached (usually due to a partial failure in a previous run).
   - **Solution**: Run the `delete` command to clean up the orphaned LPAR, then retry the `create` command.

3. **Network Boot Location Code Errors (`can not find physical location...`)**
   - **Cause**: HMC API returns location codes without port suffixes (`-T0` or `-T1`), but netboot strictly requires them.
   - **Solution**: ShiftLaunch automatically attempts `-T0` and falls back to `-T1`. If this fails, use the LPAR SMS menu (`Boot Options` → `Network`) to manually verify the adapter's location code suffix.

4. **"Cluster is already managed and fully deployed"**
   - **Cause**: You attempted to run `create` on a cluster directory that contains a `.managed` marker (indicating a healthy, finished cluster).
   - **Solution**: To redeploy, you must explicitly `delete` the cluster first to prevent accidental data loss.

---

## Authors

- **Sudeesh John** (@sudeeshjohn)

## License

This project is maintained under the `shiftlaunch` repository. See the LICENSE file for details.