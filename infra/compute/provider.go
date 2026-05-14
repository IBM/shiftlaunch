package compute

import (
	"context"
	"fmt"

	"github.com/sudeeshjohn/shiftlaunch/infra"
	"github.com/sudeeshjohn/shiftlaunch/logger"
	"github.com/sudeeshjohn/shiftlaunch/types"
	hmc "github.ibm.com/sudeeshjohn/infra-go-sdk/phmc"
)

// ComputeProvider defines operations for pre-provisioned infrastructure [cite: 39]
type ComputeProvider interface {
	// DiscoverMetadata queries the infra to find UUIDs, MACs, and Location Codes [cite: 39]
	DiscoverMetadata(ctx context.Context) error
	
	// BootNode triggers the boot sequence for a SINGLE node (e.g., Netboot via HMC)
	BootNode(ctx context.Context, node *types.NodeConfig) error
	
	// PowerOffNodes gracefully stops nodes for the 'delete' command [cite: 39]
	PowerOffNodes(ctx context.Context) error
}

func NewProvider(cfg *types.AgentConfig, log *logger.Logger, debug bool) (ComputeProvider, error) {
	return NewProviderWithState(cfg, log, debug, nil)
}

func NewProviderWithState(cfg *types.AgentConfig, log *logger.Logger, debug bool, stateManager *types.StateManager) (ComputeProvider, error) {
	log.Info("Connecting to HMC...", "ip", cfg.HMC.IP, "user", cfg.HMC.Username)
	
	client := hmc.NewHmcRestClient(cfg.HMC.IP)
	
	// Configure HMC logger to write API traffic to deployment log only (not terminal)
	hmcLogger := infra.NewHMCLoggerAdapter(log, debug)
	client.SetLogger(hmcLogger)
	
	// The library now wants (ctx, username, password, verbose_tracing_enabled)
	log.Info("Authenticating with HMC...")
	if err := client.Login(context.Background(), cfg.HMC.Username, cfg.HMC.Password, debug); err != nil {
		return nil, fmt.Errorf("HMC login failed for user %s at %s: %w. Please verify HMC is accessible and credentials are correct", cfg.HMC.Username, cfg.HMC.IP, err)
	}
	
	log.Info("Successfully authenticated with HMC", "ip", cfg.HMC.IP, "user", cfg.HMC.Username, "session", client.Session()[:8]+"...")
	
	return &HMCProvider{
		cfg:          cfg,
		hmcClient:    client,
		logger:       log,
		debug:        debug,
		stateManager: stateManager,
	}, nil
}