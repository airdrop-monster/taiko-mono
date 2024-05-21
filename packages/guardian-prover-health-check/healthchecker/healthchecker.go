package healthchecker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/labstack/echo/v4"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	guardianproverhealthcheck "github.com/taikoxyz/taiko-mono/packages/guardian-prover-health-check"
	"github.com/taikoxyz/taiko-mono/packages/guardian-prover-health-check/bindings/guardianprover"
	hchttp "github.com/taikoxyz/taiko-mono/packages/guardian-prover-health-check/http"
	"github.com/taikoxyz/taiko-mono/packages/guardian-prover-health-check/repo"
	"github.com/urfave/cli/v2"
)

type HealthChecker struct {
	ctx                    context.Context
	cancelCtx              context.CancelFunc
	healthCheckRepo        guardianproverhealthcheck.HealthCheckRepository
	guardianProverContract *guardianprover.GuardianProver
	numGuardians           uint64
	guardianProvers        []guardianproverhealthcheck.GuardianProver
	httpSrv                *hchttp.Server
	httpPort               uint64
}

func (h *HealthChecker) Name() string {
	return "healthchecker"
}

func (h *HealthChecker) Close(ctx context.Context) {
	h.cancelCtx()

	if err := h.httpSrv.Shutdown(ctx); err != nil {
		slog.Error("error shutting down http server", "error", err)
	}
}

func (h *HealthChecker) InitFromCli(ctx context.Context, c *cli.Context) error {
	cfg, err := NewConfigFromCliContext(c)
	if err != nil {
		return err
	}

	return h.initFromConfig(ctx, cfg)
}

func (h *HealthChecker) initFromConfig(ctx context.Context, cfg *Config) error {
	db, err := cfg.OpenDBFunc()
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}

	if err := h.setupRepositories(db); err != nil {
		return err
	}

	if err := h.setupClients(cfg); err != nil {
		return err
	}

	if err := h.setupGuardianProvers(); err != nil {
		return err
	}

	h.httpPort = cfg.HTTPPort
	h.ctx, h.cancelCtx = context.WithCancel(ctx)

	return nil
}

func (h *HealthChecker) setupRepositories(db *sql.DB) error {
	healthCheckRepo, err := repo.NewHealthCheckRepository(db)
	if err != nil {
		return fmt.Errorf("failed to initialize HealthCheckRepository: %w", err)
	}

	signedBlockRepo, err := repo.NewSignedBlockRepository(db)
	if err != nil {
		return fmt.Errorf("failed to initialize SignedBlockRepository: %w", err)
	}

	startupRepo, err := repo.NewStartupRepository(db)
	if err != nil {
		return fmt.Errorf("failed to initialize StartupRepository: %w", err)
	}

	h.healthCheckRepo = healthCheckRepo

	return nil
}

func (h *HealthChecker) setupClients(cfg *Config) error {
	l1EthClient, err := ethclient.Dial(cfg.L1RPCUrl)
	if err != nil {
		return fmt.Errorf("failed to connect to L1 Ethereum client: %w", err)
	}

	l2EthClient, err := ethclient.Dial(cfg.L2RPCUrl)
	if err != nil {
		return fmt.Errorf("failed to connect to L2 Ethereum client: %w", err)
	}

	guardianProverContract, err := guardianprover.NewGuardianProver(
		common.HexToAddress(cfg.GuardianProverContractAddress),
		l1EthClient,
	)
	if err != nil {
		return fmt.Errorf("failed to instantiate GuardianProver contract: %w", err)
	}

	h.guardianProverContract = guardianProverContract

	return nil
}

func (h *HealthChecker) setupGuardianProvers() error {
	numGuardians, err := h.guardianProverContract.NumGuardians(nil)
	if err != nil {
		return fmt.Errorf("failed to get number of guardians: %w", err)
	}

	h.numGuardians = numGuardians.Uint64()
	slog.Info("number of guardians", "numGuardians", h.numGuardians)

	for i := 0; i < int(h.numGuardians); i++ {
		guardianProver, err := h.createGuardianProver(i)
		if err != nil {
			return err
		}

		h.guardianProvers = append(h.guardianProvers, guardianProver)
	}

	return h.initializeHTTPServer()
}

func (h *HealthChecker) createGuardianProver(index int) (guardianproverhealthcheck.GuardianProver, error) {
	guardianAddress, err := h.guardianProverContract.Guardians(&bind.CallOpts{}, new(big.Int).SetInt64(int64(index)))
	if err != nil {
		return guardianproverhealthcheck.GuardianProver{}, fmt.Errorf("failed to get guardian address at index %d: %w", index, err)
	}

	guardianId, err := h.guardianProverContract.GuardianIds(&bind.CallOpts{}, guardianAddress)
	if err != nil {
		return guardianproverhealthcheck.GuardianProver{}, fmt.Errorf("failed to get guardian ID for address %s: %w", guardianAddress.Hex(), err)
	}

	slog.Info("setting guardian prover address", "address", guardianAddress.Hex(), "id", guardianId.Uint64())

	return guardianproverhealthcheck.GuardianProver{
		Address: guardianAddress,
		ID:      new(big.Int).Sub(guardianId, common.Big1),
		HealthCheckCounter: promauto.NewCounter(prometheus.CounterOpts{
			Name: fmt.Sprintf("guardian_prover_%v_health_checks_ops_total", guardianId.Uint64()),
			Help: "The total number of health checks",
		}),
		SignedBlockCounter: promauto.NewCounter(prometheus.CounterOpts{
			Name: fmt.Sprintf("guardian_prover_%v_signed_block_ops_total", guardianId.Uint64()),
			Help: "The total number of signed blocks",
		}),
	}, nil
}

func (h *HealthChecker) initializeHTTPServer() error {
	server, err := hchttp.NewServer(hchttp.NewServerOpts{
		Echo:            echo.New(),
		EthClient:       l2EthClient,
		HealthCheckRepo: healthCheckRepo,
		SignedBlockRepo: signedBlockRepo,
		StartupRepo:     startupRepo,
		GuardianProvers: guardianProvers,
	})

	if err != nil {
		return fmt.Errorf("failed to initialize HTTP server: %w", err)
	}

	h.httpSrv = server
	return nil
}

func (h *HealthChecker) Start() error {
	go func() {
		if err := h.httpSrv.Start(fmt.Sprintf(":%v", h.httpPort)); !errors.Is(err, http.ErrServerClosed) {
			slog.Error("failed to start HTTP server", "error", err)
		}
	}()

	return nil
}
