package command

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	log "github.com/hashicorp/go-hclog"
	cserver "github.com/hashicorp/vault/command/server"
	"github.com/hashicorp/vault/internalshared/listenerutil"
	"github.com/hashicorp/vault/internalshared/reloadutil"
	"github.com/hashicorp/vault/sdk/version"
	"github.com/hashicorp/vault/vault/diagnose"
	"github.com/mitchellh/cli"
	"github.com/posener/complete"
)

const OperatorDiagnoseEnableEnv = "VAULT_DIAGNOSE"

var _ cli.Command = (*OperatorDiagnoseCommand)(nil)
var _ cli.CommandAutocomplete = (*OperatorDiagnoseCommand)(nil)

type OperatorDiagnoseCommand struct {
	*BaseCommand

	flagDebug    bool
	flagSkips    []string
	flagConfigs  []string
	cleanupGuard sync.Once

	reloadFuncsLock *sync.RWMutex
	reloadFuncs     *map[string][]reloadutil.ReloadFunc
	startedCh       chan struct{} // for tests
	reloadedCh      chan struct{} // for tests
}

func (c *OperatorDiagnoseCommand) Synopsis() string {
	return "Troubleshoot problems starting Vault"
}

func (c *OperatorDiagnoseCommand) Help() string {
	helpText := `
Usage: vault operator diagnose 

  This command troubleshoots Vault startup issues, such as TLS configuration or
  auto-unseal. It should be run using the same environment variables and configuration
  files as the "vault server" command, so that startup problems can be accurately
  reproduced.

  Start diagnose with a configuration file:
    
     $ vault operator diagnose -config=/etc/vault/config.hcl

  Perform a diagnostic check while Vault is still running:

     $ vault operator diagnose -config=/etc/vault/config.hcl -skip=listener

` + c.Flags().Help()
	return strings.TrimSpace(helpText)
}

func (c *OperatorDiagnoseCommand) Flags() *FlagSets {
	set := NewFlagSets(c.UI)
	f := set.NewFlagSet("Command Options")

	f.StringSliceVar(&StringSliceVar{
		Name:   "config",
		Target: &c.flagConfigs,
		Completion: complete.PredictOr(
			complete.PredictFiles("*.hcl"),
			complete.PredictFiles("*.json"),
			complete.PredictDirs("*"),
		),
		Usage: "Path to a Vault configuration file or directory of configuration " +
			"files. This flag can be specified multiple times to load multiple " +
			"configurations. If the path is a directory, all files which end in " +
			".hcl or .json are loaded.",
	})

	f.StringSliceVar(&StringSliceVar{
		Name:   "skip",
		Target: &c.flagSkips,
		Usage:  "Skip the health checks named as arguments. May be 'listener', 'storage', or 'autounseal'.",
	})

	f.BoolVar(&BoolVar{
		Name:    "debug",
		Target:  &c.flagDebug,
		Default: false,
		Usage:   "Dump all information collected by Diagnose.",
	})
	return set
}

func (c *OperatorDiagnoseCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictNothing
}

func (c *OperatorDiagnoseCommand) AutocompleteFlags() complete.Flags {
	return c.Flags().Completions()
}

func (c *OperatorDiagnoseCommand) Run(args []string) int {
	f := c.Flags()
	if err := f.Parse(args); err != nil {
		c.UI.Error(err.Error())
		return 1
	}
	return c.RunWithParsedFlags()
}

func (c *OperatorDiagnoseCommand) RunWithParsedFlags() int {
	if len(c.flagConfigs) == 0 {
		c.UI.Error("Must specify a configuration file using -config.")
		return 1
	}
	diagnose.Init()
	ctx := context.Background()
	ctx, span := diagnose.StartSpan(ctx, "initialization")
	defer func() {
		span.End()
		r := diagnose.Shutdown()
		r.Write(os.Stdout)
	}()

	diagnose.Error(ctx, errors.New("nope, didn't work"), diagnose.Action("one-off"))
	c.UI.Output(version.GetVersion().FullVersionNumber(true))
	rloadFuncs := make(map[string][]reloadutil.ReloadFunc)
	server := &ServerCommand{
		// TODO: set up a different one?
		// In particular, a UI instance that won't output?
		BaseCommand: c.BaseCommand,

		// TODO: refactor to a common place?
		AuditBackends:        auditBackends,
		CredentialBackends:   credentialBackends,
		LogicalBackends:      logicalBackends,
		PhysicalBackends:     physicalBackends,
		ServiceRegistrations: serviceRegistrations,

		// TODO: other ServerCommand options?

		logger:          log.NewInterceptLogger(nil),
		allLoggers:      []log.Logger{},
		reloadFuncs:     &rloadFuncs,
		reloadFuncsLock: new(sync.RWMutex),
	}
	var config *cserver.Config
	err := diagnose.Test(ctx, "Parse configuration",
		func(ctx context.Context) error {
			server.flagConfigs = c.flagConfigs
			var err error
			config, err = server.parseConfig()
			if err != nil {
				return err
			}
			errors := config.Validate("test")
			for _, cerr := range errors {
				diagnose.Warn(ctx, cerr.String())
			}
			return err
		})
	if err != nil {
		return 1
	}
	// Check Listener Information
	// TODO: Run Diagnose checks on the actual net.Listeners

	disableClustering := config.HAStorage != nil && config.HAStorage.DisableClustering
	infoKeys := make([]string, 0, 10)
	info := make(map[string]string)

	err = diagnose.Test(ctx, "init-listeners", func(ctx context.Context) error {
		lns, _, err := server.InitListeners(ctx, config, disableClustering, &infoKeys, &info)
		if err != nil {
			return err
		}
		// Make sure we close all listeners from this point on
		listenerCloseFunc := func() {
			for _, ln := range lns {
				ln.Listener.Close()
			}
		}

		defer c.cleanupGuard.Do(listenerCloseFunc)

		sanitizedListeners := make([]listenerutil.Listener, 0, len(config.Listeners))
		for _, ln := range lns {
			if ln.Config.TLSDisable {
				diagnose.Warn(ctx, "TLS is disabled in a Listener config stanza.")
				continue
			}
			if ln.Config.TLSDisableClientCerts {
				diagnose.Warn(ctx, "TLS for a listener is turned on without requiring client certs.")
			}

			// Check ciphersuite and load ca/cert/key files
			// TODO: TLSConfig returns a reloadFunc and a TLSConfig. We can use this to
			// perform an active probe.
			_, _, err := listenerutil.TLSConfig(ln.Config, make(map[string]string), c.UI)
			if err != nil {
				return fmt.Errorf("error creating TLS Configuration out of config file: %w", err)
			}

			sanitizedListeners = append(sanitizedListeners, listenerutil.Listener{
				Listener: ln.Listener,
				Config:   ln.Config,
			})
		}
		return diagnose.ListenerChecks(sanitizedListeners)
	})

	if err != nil {
		return 1
	}
	// Errors in these items could stop Vault from starting but are not yet covered:
	// TODO: logging configuration
	// TODO: SetupTelemetry
	// TODO: check for storage backend

	err = diagnose.Test(ctx, "storage", func(ctx context.Context) error {
		_, err := server.setupStorage(config)
		return err
	})
	if err != nil {
		return 1
	}
	return 0
}
