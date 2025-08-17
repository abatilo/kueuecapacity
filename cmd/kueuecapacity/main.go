package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

func main() {
	viper.SetEnvPrefix("KC")
	viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	viper.AutomaticEnv()

	cmd := &cobra.Command{
		Use:   "kueuecapacity",
		Short: "Automatically update kueue ClusterQueue quota",
		Run:   run,
	}

	cmd.PersistentFlags().BoolP(FlagVerbose, "v", false, "Enable verbose logging")

	_ = viper.BindPFlag(FlagVerbose, cmd.PersistentFlags().Lookup(FlagVerbose))

	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func run(cmd *cobra.Command, _ []string) {
	logLevel := &slog.LevelVar{}
	if viper.GetBool(FlagVerbose) {
		logLevel.Set(slog.LevelDebug)
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel,
	}))

	log = log.With("service", "kueuecapacity")

	ctx := context.Background()

	log.InfoContext(ctx, "Starting kueuecapacity")

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	done := make(chan error, 1)

	go func() {
		log.DebugContext(runCtx, "Starting main process")
		// TODO: Implement main logic here
		<-runCtx.Done()
		done <- runCtx.Err()
	}()

	select {
	case err := <-done:
		if err != nil && err != context.Canceled {
			log.ErrorContext(ctx, "Process failed", "error", err)
			os.Exit(1)
		}
		log.InfoContext(ctx, "Process completed successfully")

	case sig := <-sigChan:
		log.InfoContext(ctx, "Received signal, initiating graceful shutdown", "signal", sig)
		cancel()

		log.InfoContext(ctx, "Waiting for process to finish...")
		if err := <-done; err != nil && err != context.Canceled {
			log.ErrorContext(ctx, "Process failed during shutdown", "error", err)
			os.Exit(1)
		}
		log.InfoContext(ctx, "Graceful shutdown complete")
	}
}
