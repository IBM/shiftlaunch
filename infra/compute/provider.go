package compute

import (
	"context"
	"fmt"

	hmc "github.com/IBM/infra-go-sdk/phmc"
	"github.com/IBM/shiftlaunch/infra"
	"github.com/IBM/shiftlaunch/logger"
	"github.com/IBM/shiftlaunch/types"
)

// Provider defines operations for pre-provisioned infrastructure [cite: 39]
type Provider interface {
	// DiscoverMetadata queries the infra to find UUIDs, MACs, and Location Codes [cite: 39]
	DiscoverMetadata(ctx context.Context) error

	// BootNode triggers the boot sequence for a SINGLE node (e.g., Netboot via HMC)
	// DEPRECATED: Use BootNodes for better performance with bulk operations
	BootNode(ctx context.Context, node *types.NodeConfig) error

	// BootNodes triggers the boot sequence for ALL nodes in the cluster simultaneously
	// This method supports bulk ISO mapping operations for improved performance
	BootNodes(ctx context.Context) error

	// PowerOffNodes gracefully stops nodes for the 'delete' command [cite: 39]
	PowerOffNodes(ctx context.Context) error
}

// NewProvider creates a new compute provider instance without state management
func NewProvider(cfg *types.AgentConfig, log *logger.Logger, debug bool) (Provider, error) {
	return NewProviderWithState(cfg, log, debug, nil)
}

// NewProviderWithState creates a new compute provider instance with optional state management
func NewProviderWithState(cfg *types.AgentConfig, log *logger.Logger, debug bool, stateManager *types.StateManager) (Provider, error) {
	log.Debug("Connecting to HMC...", "ip", cfg.HMC.IP, "user", cfg.HMC.Username)

	// Always attach the debug transport so the deployment log captures full HMC
	// traffic regardless of --debug. The console only shows it when debug==true
	// because the console logger is set to InfoLevel in that case.
	baseClient := hmc.NewRestClient(cfg.HMC.IP).WithTLSInsecure()
	client := baseClient.WithTransport(infra.HMCDebugTransport(log)(baseClient.HTTPTransport()))

	log.Debug("Authenticating with HMC...")
	if err := client.Login(context.Background(), cfg.HMC.Username, cfg.HMC.Password); err != nil {
		return nil, fmt.Errorf("HMC login failed for user %s at %s: %w. Please verify HMC is accessible and credentials are correct", cfg.HMC.Username, cfg.HMC.IP, err)
	}

	log.Debug("Successfully authenticated with HMC", "ip", cfg.HMC.IP, "user", cfg.HMC.Username, "session", client.Session()[:8]+"...")

	return &HMCProvider{
		cfg:          cfg,
		hmcClient:    client,
		logger:       log,
		debug:        debug,
		stateManager: stateManager,
	}, nil
}
