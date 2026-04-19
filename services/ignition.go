package services

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/sudeeshjohn/shiftlaunch/localexec"
	"github.com/sudeeshjohn/shiftlaunch/types"
)

const installConfigTemplate = `apiVersion: v1
baseDomain: {{.BaseDomain}}
compute:
{{- if .IsSNO}}
- name: worker
  replicas: 0
{{- else}}
- hyperthreading: Enabled
  name: worker
  replicas: {{.WorkerReplicas}}
  architecture: ppc64le
{{- end}}
controlPlane:
{{- if .IsSNO}}
  name: master
  replicas: 1
{{- else}}
  hyperthreading: Enabled
  name: master
  replicas: {{.MasterReplicas}}
  architecture: ppc64le
{{- end}}
metadata:
  name: {{.ClusterName}}
networking:
  clusterNetwork:
  - cidr: {{.ClusterNetworkCIDR}}
    hostPrefix: {{.ClusterNetworkHostPrefix}}
  machineNetwork:
  - cidr: {{.MachineNetwork}}
  networkType: OVNKubernetes
  serviceNetwork:
  - {{.ServiceNetwork}}
platform:
  none: {}
{{- if .IsSNO}}
bootstrapInPlace:
  installationDisk: {{.DiskDevice}}
{{- else}}
fips: false
{{- end}}
pullSecret: '{{.PullSecret}}'
sshKey: '{{.SSHKey}}'
`

func GenerateIgnition(ctx context.Context,cfg *types.AgentConfig, exec *localexec.LocalClient, workspaceDir string) error {
	// --- NEW: Define and create the install-dir target directory ---
	targetDir := filepath.Join(workspaceDir, "install-dir")
	exec.Execute(ctx,fmt.Sprintf("mkdir -p %s", targetDir))

	// 1. Generate install-config.yaml
	installConfig, err := generateInstallConfigYAML(cfg)
	if err != nil {
		return err
	}

	// Write to the new install-dir directory
	configPath := filepath.Join(targetDir, "install-config.yaml")
	if err := os.WriteFile(configPath, []byte(installConfig), 0644); err != nil {
		return fmt.Errorf("failed to write install-config.yaml: %w", err)
	}

	// Backup the config because openshift-install consumes it
	exec.Execute(ctx,fmt.Sprintf("cp %s %s.bak", configPath, configPath))

	// 2. Run openshift-install locally
	// Note: The installer binary is still in the parent workspace's tools directory
	installerPath := filepath.Join(workspaceDir, "tools", "openshift-install")

	var cmd string
	if cfg.IsSNO() {
		// cd into targetDir before running the installer
		cmd = fmt.Sprintf("cd %s && %s create single-node-ignition-config --dir=.", targetDir, installerPath)
	} else {
		// cd into targetDir before running the installer
		cmd = fmt.Sprintf("cd %s && %s create ignition-configs --dir=.", targetDir, installerPath)
	}

	if _, err := exec.Execute(ctx,cmd); err != nil {
		return fmt.Errorf("failed to create ignition configs: %w", err)
	}

/* 	// 3. Stage files for HTTP hosting
	httpDir := filepath.Join(workspaceDir, "http")
	exec.Execute(fmt.Sprintf("mkdir -p %s/ignition", httpDir))
	exec.Execute(fmt.Sprintf("cp -r %s/rhcos %s/", workspaceDir, httpDir))

	if cfg.IsSNO() {
		// Copy from targetDir
		exec.Execute(fmt.Sprintf("cp %s/bootstrap-in-place-for-live-iso.ign %s/ignition/bootstrap.ign", targetDir, httpDir))
	} else {
		// Copy from targetDir
		exec.Execute(fmt.Sprintf("cp %s/*.ign %s/ignition/", targetDir, httpDir))
	} */


	return nil
}

func generateInstallConfigYAML(cfg *types.AgentConfig) (string, error) {
	tmpl, err := template.New("installConfig").Parse(installConfigTemplate)
	if err != nil {
		return "", err
	}

	pullSecret, _ := os.ReadFile(cfg.OpenShift.PullSecretFile)
	sshKey, _ := os.ReadFile(os.ExpandEnv(strings.ReplaceAll(cfg.OpenShift.SSHPublicKeyFile, "~", "$HOME")))

	data := struct {
		BaseDomain               string
		WorkerReplicas           int
		MasterReplicas           int
		ClusterName              string
		ClusterNetworkCIDR       string
		ClusterNetworkHostPrefix int
		ServiceNetwork           string
		MachineNetwork           string
		IsSNO                    bool
		DiskDevice               string
		PullSecret               string
		SSHKey                   string
	}{
		BaseDomain:               cfg.OpenShift.BaseDomain,
		WorkerReplicas:           len(cfg.Nodes.Workers),
		MasterReplicas:           len(cfg.Nodes.Masters),
		ClusterName:              cfg.OpenShift.ClusterName,
		ClusterNetworkCIDR:       cfg.OpenShift.ClusterNetworkCIDR,
		ClusterNetworkHostPrefix: cfg.OpenShift.HostPrefix,
		ServiceNetwork:           cfg.OpenShift.ServiceNetwork,
		MachineNetwork:           cfg.Network.MachineCIDR,
		IsSNO:                    cfg.IsSNO(),
		DiskDevice:               "/dev/sda", // Default disk device for SNO
		PullSecret:               strings.TrimSpace(string(pullSecret)),
		SSHKey:                   strings.TrimSpace(string(sshKey)),
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}