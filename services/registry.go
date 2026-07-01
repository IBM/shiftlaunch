package services

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/IBM/shiftlaunch/localexec"
	"github.com/IBM/shiftlaunch/logger"
	"github.com/IBM/shiftlaunch/types"
	"gopkg.in/yaml.v3"
)

const registryServiceTemplate = `[Unit]
Description=Local Container Registry for Disconnected OpenShift
After=network.target syslog.target

[Service]
Type=simple
TimeoutStartSec=5m
TimeoutStopSec=15s
ExecStartPre=-/usr/bin/podman rm -f "local-registry"

ExecStart=/usr/bin/podman run --name local-registry -p 5000:5000 \
                                -v /opt/registry/data:/var/lib/registry:z \
                                -v /opt/registry/auth:/auth:z \
                                -e "REGISTRY_AUTH=htpasswd" \
                                -e "REGISTRY_AUTH_HTPASSWD_REALM=Registry Realm" \
                                -e REGISTRY_AUTH_HTPASSWD_PATH=/auth/htpasswd \
                                -v /opt/registry/certs:/certs:z \
                                -e REGISTRY_HTTP_TLS_CERTIFICATE=/certs/domain.crt \
                                -e REGISTRY_HTTP_TLS_KEY=/certs/domain.pem \
                                {{.RegistryImage}}

ExecReload=-/usr/bin/podman stop -t 2 "local-registry"
ExecReload=-/usr/bin/podman rm -f "local-registry"
ExecStop=-/usr/bin/podman stop -t 2 "local-registry"
Restart=always
RestartSec=30

[Install]
WantedBy=multi-user.target
`

// RegistryManager handles local container registry setup for disconnected OpenShift deployments
type RegistryManager struct {
	cfg          *types.AgentConfig
	executor     *localexec.LocalClient
	logger       *logger.Logger
	stateManager *types.StateManager
	state        *types.DeploymentState
	workspaceDir string
}

// NewRegistryManager creates a new registry manager instance
func NewRegistryManager(cfg *types.AgentConfig, exec *localexec.LocalClient, log *logger.Logger, stateMgr *types.StateManager, state *types.DeploymentState, workspaceDir string) *RegistryManager {
	return &RegistryManager{
		cfg:          cfg,
		executor:     exec,
		logger:       log,
		stateManager: stateMgr,
		state:        state,
		workspaceDir: workspaceDir,
	}
}

// Setup configures and starts the local container registry with SSL and authentication
func (r *RegistryManager) Setup(ctx context.Context, workspaceDir string) error {
	r.workspaceDir = workspaceDir
	r.logger.Info("Setting up local container registry for disconnected deployment...")
	shieldedCtx := context.WithoutCancel(ctx)

	registryHost := r.getRegistryHost()
	registryURL := fmt.Sprintf("%s:5000", registryHost)

	// 1. Create directory structure
	r.logger.Debug("Creating registry directories...")
	if _, err := r.executor.Execute(shieldedCtx, "sudo mkdir -p /opt/registry/{auth,certs,data}"); err != nil {
		return fmt.Errorf("failed to create registry directories: %w", err)
	}

	// 2. Generate self-signed SSL certificate (Idempotent for Multi-Tenant)
	r.logger.Debug("Checking for existing SSL certificates...")
	if _, err := r.executor.Execute(shieldedCtx, "test -f /opt/registry/certs/domain.crt"); err != nil {
		r.logger.Debug("Generating all-inclusive multi-SAN SSL certificates...")

		// Dynamically assemble all possible identity aliases the cluster components will use
		sanDNS := fmt.Sprintf("DNS:%s", registryHost)
		sanIP := fmt.Sprintf("IP:%s,IP:127.0.0.1", r.cfg.Network.ControllerIP)

		// Fallback alias for cluster-scoped routing strings
		if r.cfg.OpenShift.ClusterName != "" && r.cfg.OpenShift.BaseDomain != "" {
			clusterFQDN := fmt.Sprintf("registry.%s.%s", r.cfg.OpenShift.ClusterName, r.cfg.OpenShift.BaseDomain)
			if clusterFQDN != registryHost {
				sanDNS += fmt.Sprintf(",DNS:%s", clusterFQDN)
			}
		}

		certCmd := fmt.Sprintf(`sudo openssl req -newkey rsa:4096 -nodes -sha256 \
			-keyout /opt/registry/certs/domain.pem \
			-x509 -days 365 \
			-out /opt/registry/certs/domain.crt \
			-subj "/CN=%s" \
			-addext "basicConstraints=critical,CA:TRUE" \
			-addext "keyUsage=critical,digitalSignature,keyEncipherment,keyCertSign" \
			-addext "extendedKeyUsage=serverAuth" \
			-addext "subjectAltName=%s,%s"`,
			registryHost, sanDNS, sanIP)

		if _, err := r.executor.Execute(shieldedCtx, certCmd); err != nil {
			return fmt.Errorf("failed to generate all-inclusive SSL certificate: %w", err)
		}
		r.executor.Execute(shieldedCtx, "sudo chmod 644 /opt/registry/certs/domain.pem")
	} else {
		r.logger.Info("Existing SSL certificate found. Reusing for multi-cluster support.")
	}

	// 3. Create or update htpasswd authentication
	r.logger.Debug("Configuring registry authentication...")
	username := r.cfg.Services.Registry.GetUser()
	password := r.cfg.Services.Registry.GetPass()

	//  Use -c to create only if the file doesn't exist. Otherwise, just append/update!
	authFlag := "-bBc"
	if _, err := r.executor.Execute(shieldedCtx, "test -f /opt/registry/auth/htpasswd"); err == nil {
		authFlag = "-bB"
	}
	htpasswdCmd := fmt.Sprintf("sudo htpasswd %s /opt/registry/auth/htpasswd %s %s", authFlag, username, password)
	if _, err := r.executor.Execute(shieldedCtx, htpasswdCmd); err != nil {
		return fmt.Errorf("failed to create htpasswd: %w", err)
	}

	// 4. Trust the certificate system-wide
	r.logger.Debug("Adding certificate to system trust store...")
	if _, err := r.executor.Execute(shieldedCtx, "sudo cp /opt/registry/certs/domain.crt /etc/pki/ca-trust/source/anchors/"); err != nil {
		return fmt.Errorf("failed to copy certificate: %w", err)
	}
	if _, err := r.executor.Execute(shieldedCtx, "sudo update-ca-trust"); err != nil {
		return fmt.Errorf("failed to update ca-trust: %w", err)
	}

	// =========================================================
	//  Move Firewall Configuration BEFORE starting Podman
	// =========================================================
	r.logger.Debug("Configuring firewall for registry...")
	if _, err := r.executor.Execute(shieldedCtx, "sudo firewall-cmd --permanent --add-port=5000/tcp"); err != nil {
		return fmt.Errorf("failed to add firewall rule: %w", err)
	}
	if _, err := r.executor.Execute(shieldedCtx, "sudo firewall-cmd --reload"); err != nil {
		return fmt.Errorf("failed to reload firewall: %w", err)
	}

	// 5 & 6. Smart Registry Detection & Reuse
	r.logger.Debug("Checking if port 5000 is already hosting a valid registry...")
	verifyCmd := fmt.Sprintf("env HTTP_PROXY='' HTTPS_PROXY='' http_proxy='' https_proxy='' curl -s -o /dev/null -w '%%{http_code}' --connect-timeout 3 -u %s:%s -k https://%s:5000/v2/", username, password, r.cfg.Network.ControllerIP)
	
	httpCode, _ := r.executor.Execute(shieldedCtx, verifyCmd)
	httpCode = strings.TrimSpace(httpCode)

	if httpCode == "200" {
		r.logger.Info("Valid registry already running on port 5000. Reusing existing instance.")
	} else {
		// Ensure the port isn't hogged by something we can't authenticate to
		portCheck, _ := r.executor.Execute(shieldedCtx, "ss -tlpn | grep ':5000 ' 2>/dev/null || true")
		if strings.TrimSpace(portCheck) != "" {
			return fmt.Errorf("port 5000 is in use but registry authentication failed (HTTP %s)", httpCode)
		}

		r.logger.Debug("Starting fresh local registry service...")

		tmpl, err := template.New("registry-service").Parse(registryServiceTemplate)
		if err == nil {
			var buf bytes.Buffer
			_ = tmpl.Execute(&buf, struct{ RegistryImage string }{RegistryImage: r.cfg.Services.Registry.RegistryImage})
			_ = r.executor.WriteFile(shieldedCtx, "/etc/systemd/system/local-registry.service", buf.Bytes(), 0644)
		}

		r.executor.Execute(shieldedCtx, "sudo podman rm -f local-registry 2>/dev/null || true")

		if _, err := r.executor.Execute(shieldedCtx, "sudo systemctl daemon-reload"); err != nil {
			return fmt.Errorf("failed to reload systemd: %w", err)
		}
		if err := r.executor.SystemctlEnable(shieldedCtx, "local-registry"); err != nil {
			return fmt.Errorf("failed to enable registry service: %w", err)
		}
		if err := r.executor.SystemctlRestart(shieldedCtx, "local-registry"); err != nil {
			return fmt.Errorf("failed to start registry service: %w", err)
		}
	}

	// 7. Ensure the Controller can resolve the registry hostname locally
	r.logger.Debug("Injecting registry hostname into local /etc/hosts for resolution...")

	// ---  Use a strict, cluster-specific marker to prevent wiping out the Controller IP ---
	marker := fmt.Sprintf("# ShiftLaunch-Registry: %s", r.cfg.OpenShift.ClusterName)
	hostsEntry := fmt.Sprintf("%s %s %s", r.cfg.Network.ControllerIP, registryHost, marker)

	// Clean up any stale entries first (using the precise marker), then append
	// STRICT MATCH FIX: Add the '$' anchor so 'my-cluster' doesn't delete 'my-cluster1'
	r.executor.Execute(shieldedCtx, fmt.Sprintf("sudo sed -i '/%s$/d' /etc/hosts", marker))
	r.executor.Execute(shieldedCtx, fmt.Sprintf("echo '%s' | sudo tee -a /etc/hosts > /dev/null", hostsEntry))

	// 8. Wait for registry to be ready
	r.logger.Debug("Waiting for registry to be ready...")

	// TARGET PHYSICAL IP FIX: Use ControllerIP instead of 127.0.0.1 so traffic hits Podman's NAT tables
	checkCmd := fmt.Sprintf("env HTTP_PROXY='' HTTPS_PROXY='' http_proxy='' https_proxy='' curl --connect-timeout 5 --max-time 10 -u %s:%s -k https://%s:5000/v2/_catalog", username, password, r.cfg.Network.ControllerIP)

	var lastErr error
	for i := 0; i < 10; i++ {
		if _, err := r.executor.Execute(shieldedCtx, checkCmd); err == nil {
			lastErr = nil //  Clear the sticky error before breaking!
			break
		} else {
			lastErr = err
			r.logger.Debug("Registry not ready yet, retrying...", "attempt", i+1)
			r.executor.Execute(shieldedCtx, "sleep 5")
		}
	}
	if lastErr != nil {
		return fmt.Errorf("registry failed to start after 10 attempts: %w", lastErr)
	}

	// 9. Update pull secret with local registry credentials
	r.logger.Info("Updating pull secret with local registry authentication...")

	//  Use the actual source file from the config!
	pullSecretPath := os.ExpandEnv(strings.ReplaceAll(r.cfg.OpenShift.PullSecretFile, "~", "$HOME"))
	updatedSecretPath := filepath.Join(workspaceDir, "pull-secret-updated.json")

	// export REG_TOKEN so jq can read it via env.REG_TOKEN — it never appears in
	// the filter string itself. registryURL is bound via --arg so it is an opaque
	// jq variable ($host) with no quoting issues. All credentials and paths are
	// wrapped in shellQuote so bash metacharacters cannot escape the shell command.
	updateSecretCmd := fmt.Sprintf(
		`export REG_TOKEN=$(printf '%%s:%%s' %s %s | base64 -w0) && `+
			`jq -c --arg host %s '.auths += {($host): {"auth": env.REG_TOKEN}}' `+
			`< %s > %s`,
		shellQuote(username), shellQuote(password), shellQuote(registryURL),
		shellQuote(pullSecretPath), shellQuote(updatedSecretPath),
	)
	if _, err := r.executor.Execute(shieldedCtx, updateSecretCmd); err != nil {
		return fmt.Errorf("failed to update pull secret: %w", err)
	}

	// 10. Mirror OpenShift release images (with idempotency to prevent re-runs on resume)
	if r.cfg.Services.Registry.AutoMirror {
		mirrorEventID := fmt.Sprintf("mirror_release_%s", r.cfg.OpenShift.Version)

		// Check if mirroring was already completed
		if r.state != nil && contains(r.state.CompletedEvents, mirrorEventID) {
			r.logger.Info("Image mirroring already completed, skipping...")
		} else {
			r.logger.Info("Mirroring OpenShift release images (this may take 15-30 minutes)...")
			if err := r.mirrorImages(shieldedCtx, workspaceDir, registryURL, updatedSecretPath); err != nil {
				return fmt.Errorf("failed to mirror images: %w", err)
			}

			// Mark mirroring as completed
			if r.state != nil && r.stateManager != nil {
				r.state.CompletedEvents = append(r.state.CompletedEvents, mirrorEventID)
				r.stateManager.SaveState(r.state)
			}
		}
	}

	r.logger.Info("Local registry setup complete", "url", fmt.Sprintf("https://%s", registryURL))
	return nil
}

// mirrorImages acts as a smart router based on the payload type
func (r *RegistryManager) mirrorImages(ctx context.Context, workspaceDir, registryURL, pullSecretPath string) error {
	releaseType := r.cfg.OpenShift.ReleaseType // <--- UPDATED PATH

	r.logger.Info("Starting image mirror process", "release_type", releaseType, "version", r.cfg.OpenShift.Version)

	if releaseType == "ci" {
		return r.mirrorViaOcAdm(ctx, workspaceDir, registryURL, pullSecretPath)
	}

	return r.mirrorViaOcMirrorV2(ctx, workspaceDir, registryURL, pullSecretPath)
}

// mirrorViaOcAdm blindly copies raw payloads without checking the graph (Best for CI/Nightly/Dev builds)
func (r *RegistryManager) mirrorViaOcAdm(ctx context.Context, workspaceDir, registryURL, pullSecretPath string) error {
	ocPath := filepath.Join(workspaceDir, "tools", "oc")
	releaseImage := r.cfg.Services.Registry.ReleaseImage
	localRepo := r.cfg.Services.Registry.LocalRepo
	releaseTag := r.cfg.OpenShift.Version

	mirrorCmd := fmt.Sprintf(`%s adm release mirror -a %s --from=%s --to=%s/%s --to-release-image=%s/%s:%s --insecure=true`,
		ocPath, pullSecretPath, releaseImage, registryURL, localRepo, registryURL, localRepo, releaseTag)

	r.logger.Debug("Executing oc adm mirror command", "cmd", mirrorCmd)
	output, err := r.executor.Execute(ctx, mirrorCmd)

	logFile := filepath.Join(workspaceDir, "registry-mirror-info.txt")
	os.WriteFile(logFile, []byte(output), 0644)

	if err != nil {
		// Parse the output for known issues to provide a clean UX
		lowerOut := strings.ToLower(output)
		if strings.Contains(lowerOut, "not found") || strings.Contains(lowerOut, "manifest unknown") {
			return fmt.Errorf("release image '%s' could not be found upstream. Check your version and release image URL", releaseTag)
		}
		if strings.Contains(lowerOut, "unauthorized") || strings.Contains(lowerOut, "invalid credentials") {
			return fmt.Errorf("authentication failed: your pull-secret is invalid or lacks access to the upstream registry")
		}
		if strings.Contains(lowerOut, "no space left on device") {
			return fmt.Errorf("mirroring failed: the local registry partition ran out of disk space")
		}

		// Generic fallback error pointing to the log
		return fmt.Errorf("oc adm release mirror failed. See detailed logs at: %s", logFile)
	}
	return nil
}

// mirrorViaOcMirrorV2 queries the Cincinnati Graph for official releases and enforces signatures (Best for Production)
func (r *RegistryManager) mirrorViaOcMirrorV2(ctx context.Context, workspaceDir, registryURL, pullSecretPath string) error {
	ocMirrorPath := filepath.Join(workspaceDir, "tools", "oc-mirror")
	version := r.cfg.OpenShift.Version
	localRepo := r.cfg.Services.Registry.LocalRepo

	// Extract channel (e.g. "4.21.14" -> "stable-4.21")
	parts := strings.Split(version, ".")
	channel := "stable-" + version
	if len(parts) >= 2 {
		channel = fmt.Sprintf("stable-%s.%s", parts[0], parts[1])
	}

	imageSetYaml := fmt.Sprintf(`kind: ImageSetConfiguration
apiVersion: mirror.openshift.io/v2alpha1
mirror:
  platform:
    architectures:
      - "ppc64le"
    channels:
    - name: %s
      type: ocp
      minVersion: %s
      maxVersion: %s
    graph: false
`, channel, version, version)

	imageSetPath := filepath.Join(workspaceDir, "imageset-config.yaml")
	os.WriteFile(imageSetPath, []byte(imageSetYaml), 0644)

	// --- THE MULTI-TENANT FIX ---

	// 1. The Scalpel: Kill zombie processes tied to THIS specific cluster's workspace
	r.logger.Debug("Sweeping for orphaned oc-mirror processes tied to this cluster...")
	targetKillCmd := fmt.Sprintf("sudo pkill -9 -f 'oc-mirror.*%s' 2>/dev/null || true", r.cfg.OpenShift.ClusterName)
	r.executor.Execute(ctx, targetKillCmd)

	// 2. The Queue: oc-mirror v2 hardcodes port 55000. Wait for it to clear across ALL TCP states (including TIME_WAIT).
	for i := 0; i < 60; i++ { // Wait up to 10 minutes
		// STRICT MATCH: Use awk to isolate Column 4 (Local Address) so we don't accidentally match inbound ephemeral client ports!
		out, _ := r.executor.Execute(ctx, "ss -tan | awk '{print $4}' | grep -E ':55000$' 2>/dev/null || true")
		if strings.TrimSpace(out) == "" {
			break // Port is free, we can proceed!
		}
		
		// ZOMBIE CHECK: If we are stuck in the queue for 30 seconds, it might be an orphaned registry child process from a previous crash.
		if i == 3 {
			r.logger.Info("Port 55000 is stuck. Hunting for zombie ephemeral registries...")
			r.executor.Execute(ctx, "sudo fuser -k -9 55000/tcp 2>/dev/null || true")
		}

		if i == 0 || i%6 == 0 {
			r.logger.Info("Waiting for ephemeral port 55000 to become available for mirroring...")
		}
		r.executor.Execute(ctx, "sleep 10")
	}

	// Execute v2 mirror logic
	mirrorCmd := fmt.Sprintf(`cd %s && unset REGISTRY_AUTH_FILE && %s --v2 --config=imageset-config.yaml --authfile=%s --workspace file://%s/oc-mirror-workspace docker://%s/%s --dest-tls-verify=false`,
		workspaceDir, ocMirrorPath, pullSecretPath, workspaceDir, registryURL, localRepo)

	r.logger.Debug("Executing oc-mirror v2 command", "cmd", mirrorCmd)
	output, err := r.executor.Execute(ctx, mirrorCmd)

	logFile := filepath.Join(workspaceDir, "registry-mirror-info.txt")
	os.WriteFile(logFile, []byte(output), 0644)

	if err != nil {
		// Parse the output for known issues to provide a clean UX
		lowerOut := strings.ToLower(output)
		if strings.Contains(lowerOut, "no release found") {
			return fmt.Errorf("OpenShift version %s is not available in the '%s' channel. Verify the version is correct", version, channel)
		}
		if strings.Contains(lowerOut, "unauthorized") || strings.Contains(lowerOut, "invalid credentials") {
			return fmt.Errorf("authentication failed: your pull-secret is invalid or lacks access to mirror these images")
		}
		if strings.Contains(lowerOut, "no space left on device") {
			return fmt.Errorf("mirroring failed: the local registry partition ran out of disk space")
		}

		// Generic fallback error pointing to the log
		return fmt.Errorf("oc-mirror failed to sync the payload. See detailed logs at: %s", logFile)
	}
	return nil
}

// getRegistryHost returns the hostname or IP address for the registry based on configuration
func (r *RegistryManager) getRegistryHost() string {
	//  For locally managed shared registries, the IP is the safest multi-tenant
	// identifier because it is permanently baked into the shared certificate's SAN!
	if r.cfg.Services.Registry.IsManaged() {
		return r.cfg.Network.ControllerIP
	}

	// Fallback for external user-managed registries
	if r.cfg.Services.Registry.GetExternal() != "" {
		return r.cfg.Services.Registry.GetExternal()
	}
	return fmt.Sprintf("registry.%s.%s", r.cfg.OpenShift.ClusterName, r.cfg.OpenShift.BaseDomain)
}

// GetRegistryURL returns the full registry URL for use in install-config
func (r *RegistryManager) GetRegistryURL() string {
	host := r.getRegistryHost()

	// If we are managing it locally, we know it's locked to 5000
	if r.cfg.Services.Registry.IsManaged() {
		return fmt.Sprintf("%s:5000", host)
	}

	// If it's an external enterprise registry, trust the user's string entirely.
	// If they omitted a port, standard tools default to 443 safely.
	return host
}

// GetCertificatePath returns the path to the registry certificate
func (r *RegistryManager) GetCertificatePath() string {
	return "/opt/registry/certs/domain.crt"
}

// Cleanup removes the registry service and configuration ONLY if no other clusters are using it
func (r *RegistryManager) Cleanup(ctx context.Context) error {
	//  Check for multi-tenancy before nuking the registry!
	if r.isRegistryShared() {
		r.logger.Info("Local registry is actively being used by other managed clusters. Bypassing registry teardown.")
		return nil
	}

	r.logger.Info("Cleaning up local registry...")
	shieldedCtx := context.WithoutCancel(ctx)

	// Stop and disable service
	r.executor.Execute(shieldedCtx, "sudo systemctl stop local-registry")
	r.executor.Execute(shieldedCtx, "sudo systemctl disable local-registry")

	// Remove firewall rule
	r.executor.Execute(shieldedCtx, "sudo firewall-cmd --permanent --remove-port=5000/tcp")
	r.executor.Execute(shieldedCtx, "sudo firewall-cmd --reload")

	// Remove certificate from trust store
	r.executor.Execute(shieldedCtx, "sudo rm -f /etc/pki/ca-trust/source/anchors/domain.crt")
	r.executor.Execute(shieldedCtx, "sudo update-ca-trust")

	r.logger.Info("Registry cleanup complete")
	return nil
}

// isRegistryShared checks if other managed clusters are currently using the local registry
func (r *RegistryManager) isRegistryShared() bool {
	//  Use the dynamic workspace directory instead of hardcoding /opt/shiftlaunch
	workspaceParent := filepath.Dir(r.workspaceDir)

	entries, err := os.ReadDir(workspaceParent)
	if err != nil {
		return false
	}

	activeCount := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		clusterName := entry.Name()
		if clusterName == r.cfg.OpenShift.ClusterName {
			continue // Skip our own cluster
		}

		// A cluster is active if it has a config file and is NOT marked as deleted
		deletedMarker := filepath.Join(workspaceParent, clusterName, ".deleted")
		if _, err := os.Stat(deletedMarker); err == nil {
			continue
		}

		configPath := filepath.Join(workspaceParent, clusterName, "config.yaml")
		data, err := os.ReadFile(configPath)
		if err != nil {
			continue
		}

		// Parse the config to see if it relies on the managed registry
		var tmpCfg types.AgentConfig
		if err := yaml.Unmarshal(data, &tmpCfg); err == nil {
			//  Look exclusively at the Registry service toggle to prevent
			// unoptimized config files from being skipped!
			if tmpCfg.Services.Registry.IsManaged() {
				activeCount++
			}
		}
	}

	return activeCount > 0
}

// Helper function to check if a string exists in a slice
func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

// shellQuote wraps s in single quotes and escapes any literal single quotes
// inside it using the '\'' idiom, making the value safe to embed in a bash
// command string regardless of the characters it contains.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
