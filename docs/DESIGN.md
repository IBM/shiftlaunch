# 🎯 ShiftLaunch: Software Design Document

**Version:** 3.0 (Greenfield Architecture)
**System:** IBM Power Systems (ppc64le)
**Component:** OpenShift UPI (User-Provisioned Infrastructure) Deployer

---

## 1. Executive Summary

ShiftLaunch is an end-to-end, stateful Go-based orchestration engine designed to automate the deployment of OpenShift Container Platform (OCP) clusters on IBM Power Systems. It bridges the gap between OpenShift's x86-centric tooling and the strict infrastructure requirements of IBM Power Hardware Management Consoles (HMC) and Virtual I/O Servers (VIOS).

The tool supports deploying both Single-Node OpenShift (SNO) and highly-available Multi-Node clusters. It dynamically provisions required network services (DNS, DHCP, PXE, NFS, HTTP, HAProxy) on a local controller node and handles infrastructure signaling (Power-On, Virtual Media Mapping, Netboot) directly via HMC REST APIs.

### Key CLI Capabilities
- **`generate-config`**: Uses a native Go `text/template` engine to dynamically generate a well-documented starter YAML based on topology (`sno`/`multi`) and boot method (`iso`/`netboot`), automatically toggling required services.
- **`create`**: Executes the idempotent deployment state machine.
- **`delete`**: Safely tears down a cluster with Intelligent Partial Failure tracking.
- **`status`**: Displays credentials, endpoints, and `/etc/hosts` entries directly from the local workspace.
- **`list`**: Provides a tabular view of all managed clusters.

---

## 2. Core Architecture

ShiftLaunch is built on four foundational architectural pillars:

### A. Single VIP Architecture
Instead of requiring separate VIPs for the Kubernetes API and Ingress, ShiftLaunch provisions a single IP alias on the controller node. HAProxy routes traffic via Layer 4 TCP based strictly on port mapping:
- `6443` -> API Server
- `22623` -> Machine Config Server
- `80/443` -> Ingress Routers

*Impact:* Cuts required IP allocations by 50% and simplifies DNS records.

### B. Workspace Isolation & Credential Caching
Every cluster is assigned an isolated directory at `/opt/shiftlaunch/clusters/<cluster-name>/`.
This directory serves as the single source of truth for the cluster, storing:
- The `config.yaml` used to deploy it.
- The runtime state (`state.json`).
- Automatically downloaded `kubeconfig` and `kubeadmin-password` files.
- Internal `.managed`, `.failed`, or `.deleted` marker files.

### C. Greenfield Node-Level Configuration
Legacy cluster-level storage and hardware blocks have been entirely replaced. All hardware, network (vSwitch/VLAN), and storage (VIOS/SVC) parameters are now defined strictly at the **node level**. This creates a single source of truth and prevents ambiguity in multi-system deployments.

### D. Idempotent State Machine & Intelligent Deletion
The orchestrator tracks its progression in `state.json`. 
- **Resuming:** If a deployment fails (e.g., network timeout), running `shiftlaunch create` parses the completed phases and safely resumes exactly where it left off.
- **Partial Failure Deletion:** If cluster teardown encounters locked resources (e.g., a disk in use), it deletes what it can, records the specific failures in the state file, and allows the user to re-run `delete` to safely retry *only* the failed resources. No orphaned infrastructure is left behind.

---

## 3. Supported Boot Mechanisms

ShiftLaunch supports two distinct provisioning mechanisms, toggleable via the `boot_method` configuration parameter.

### A. Agent ISO Boot (`boot_method: "iso"`)
The modern, network-isolated OpenShift Agent workflow. Highly recommended for production and disconnected environments.
1. **Payload Generation:** The Orchestrator builds an `agent-config.yaml` using static MAC-to-IP bindings (via NMState) and generates a self-contained OpenShift Agent ISO.
2. **NFS Hosting:** A local NFS server is configured to export the ISO directory.
3. **Virtual Media Mapping (LBYL):** ShiftLaunch mounts the NFS share to the IBM VIOS, registers the ISO as a Virtual Optical Media device, maps it to the target LPAR, and powers the LPAR on in standard `norm` boot mode.
4. **Service Pruning:** Because the ISO contains static IP instructions, ShiftLaunch automatically disables its local DHCP and PXE services to prevent network clutter.

### B. Network Boot (`boot_method: "netboot"`)
The traditional OpenShift UPI workflow utilizing PXE over the network.
1. **Services Setup:** ShiftLaunch configures granular `dnsmasq` phases for DHCP and TFTP, alongside Apache (`httpd`) for hosting Ignition payloads and RHCOS images.
2. **Location Code Fallback Strategy:** The Orchestrator power-cycles the LPAR to Open Firmware to register network adapters. It dynamically fetches the base location code and intelligently tries the `-T0` suffix, falling back to `-T1` if the HMC rejects it, ensuring robust physical adapter targeting.
3. **Payload Delivery:** The LPAR receives a DHCP lease, pulls `core.elf` via TFTP, loads the MAC-specific GRUB config, and fetches the RHCOS rootfs and Ignition configs via HTTP.

---

## 4. Orchestration State Machine (The Phases)

The core logic lives in `orchestrator.go` and executes linearly, saving state after each step.

* **Phase 0: Validation:** Evaluates YAML syntax, verifies local controller disk space and prerequisites, checks VIP conflicts, and tests HMC connectivity/LPAR availability.
* **Phase 1: Discovery:** Queries the HMC to fetch System UUIDs, LPAR UUIDs, Profile UUIDs, and network adapter MAC Addresses.
* **Phase 2: Downloads:** Downloads `openshift-install` and `oc` binaries. If `netboot`, it also fetches the RHCOS Kernel, Initramfs, and Rootfs.
* **Phase 3: Managed Services:** Installs required Linux packages and opens firewall ports. Splits `dnsmasq` setup into highly granular `setup_dns`, `setup_dhcp`, and `setup_pxe` functions to ensure deterministic load ordering.
* **Phase 4: Ignition Generation:** Uses `openshift-install` to create either standard Ignition files or an Agent ISO.
* **Phase 5: Boot:** Iterates through all nodes and executes the HMC API sequence to power them on. 
* **Phase 6: Wait:** Wraps `openshift-install wait-for` commands. **Crucially, this phase forcefully isolates the installer's stdout/stderr into memory buffers** to completely suppress terminal spam while cleanly passing the logs to the background deployment log file.
* **Phase 7: Post-Install Cleanup:** *(ISO Only)* Detaches Virtual Optical Media from LPARs, deletes the ISO from the VIOS, unmounts the VIOS NFS share, and removes the local NFS export from the controller.

---

## 5. Teardown and Cleanup (Intelligent Deletion)

The `Teardown` flow is engineered to be fully idempotent (safe to run repeatedly against partial failures). 

When executing a cluster deletion, it follows a strict 4-step sequence:
1. **LPAR Power-off:** Gracefully drops virtual terminal SSH sessions, then sends `Immediate` power-off signals via the HMC REST API.
2. **Unmap Storage:** Uses batch unmapping to cleanly detach all Client/Server virtual disks and optical media.
3. **Delete Volumes:** Wipes the underlying logical volumes from the VIOS (or SVC).
4. **Delete LPARs:** Removes the LPAR configuration from the HMC entirely.

**Failure Tracking:** If Step 3 fails because a disk is locked, the Orchestrator skips Step 4 for that node, saves the locked disk into `state.json`, and exits cleanly. The user can simply re-run the `delete` command later to retry wiping the remaining artifacts.

---

## 6. Deployment Flow Diagram

```mermaid
flowchart TD
    Start(["Start Deployment: shiftlaunch create"]) --> P0["Phase 0: Validation"]
    
    P0 --> P0_Check{"Boot Method?"}
    P0_Check -- "iso" --> P0_ISO["Validate MACs\nSkip RHCOS URL checks"]
    P0_Check -- "netboot" --> P0_Net["Validate RHCOS URLs"]
    
    P0_ISO --> P1["Phase 1: Pre-Flight & HMC Discovery"]
    P0_Net --> P1
    
    P1 --> P2["Phase 2: Downloading OpenShift Artifacts"]

    %% Phase 2
    P2 --> P2_Check{"Boot Method?"}
    P2_Check -- "iso" --> P2_ISO["Skip RHCOS Image Downloads\nDownload OpenShift Tools only"]
    P2_Check -- "netboot" --> P2_Net["Download RHCOS Kernel/Initramfs/Rootfs\nDownload OpenShift Tools"]

    P2_ISO --> P3["Phase 3: Managed Infrastructure Services"]
    P2_Net --> P3

    %% Phase 3
    P3 --> P3_Check{"Boot Method?"}
    P3_Check -- "iso" --> P3_ISO["Install nfs-utils & nmstate\nOpen NFS Firewall Ports\nSkip DHCP/PXE Setup"]
    P3_Check -- "netboot" --> P3_Net["Install httpd & tftp-server\nConfigure Granular DHCP & TFTP/PXE Service"]

    P3_ISO --> P4["Phase 4: Ignition Generation"]
    P3_Net --> P4

    %% Phase 4
    P4 --> P4_Check{"Boot Method?"}
    P4_Check -- "iso" --> P4_ISO["Generate install-config.yaml & agent-config.yaml\nRun 'agent create image'\nSetup Local NFS Server"]
    P4_Check -- "netboot" --> P4_Net["Generate install-config.yaml\nRun 'create ignition-configs'\nSetup HTTP Server & Stage Files"]

    P4_ISO --> P5["Phase 5: Initiating Cluster Boot"]
    P4_Net --> P5

    %% Phase 5
    P5 --> P5_Check{"Boot Method?"}
    P5_Check -- "iso" --> P5_ISO["Mount Controller NFS on VIOS\nCreate Virtual Optical Media (ISO)\nMap ISO to LPAR\nPower On LPAR (BootMode: norm)"]
    P5_Check -- "netboot" --> P5_Net["Power Cycle LPAR to Open Firmware\nDiscover Base Location Code\nTry -T0 Suffix (Fallback to -T1)\nPower On LPAR (BootMode: netboot)"]

    P5_ISO --> P6["Phase 6: Waiting for Installation"]
    P5_Net --> P6

    %% Phase 6
    P6 --> P6_Check{"Boot Method?"}
    P6_Check -- "iso" --> P6_ISO["Skip WaitForBootstrap\nRun 'agent wait-for install-complete' (Terminal output buffered)"]
    P6_Check -- "netboot" --> P6_Net["Run 'wait-for bootstrap-complete' (Terminal output buffered)\nRun 'wait-for install-complete' (Terminal output buffered)"]

    P6_ISO --> P7["Phase 7: Post-Install Cleanup & Caching"]
    P6_Net --> P7_Net["Phase 7: Post-Install Caching"]

    %% Phase 7 (ISO Only Cleanup + Universal Credential Caching)
    P7 --> P7_ISO["Unmap Optical Media from LPAR\nDelete ISO from VIOS\nUnmount NFS from VIOS\nCleanup Local NFS Export\nDownload Kubeconfig locally"]
    P7_Net --> P7_Cache["Download Kubeconfig locally"]
    
    P7_ISO --> Done(["Deployment Complete"])
    P7_Cache --> Done