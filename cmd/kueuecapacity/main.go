package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	kueueclientset "sigs.k8s.io/kueue/client-go/clientset/versioned"
)

func main() {
	viper.SetEnvPrefix("KC")
	viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	viper.AutomaticEnv()

	cmd := &cobra.Command{
		Use:   "kueuecapacity",
		Short: "Monitor and sync Kubernetes node capacity with Kueue ClusterQueues",
		Long: `kueuecapacity monitors Kubernetes nodes and automatically updates
Kueue ClusterQueue resource quotas to match actual cluster capacity.

It watches node additions, updates, and deletions in real-time,
groups nodes by ResourceFlavors, and synchronizes the ClusterQueue's
nominalQuota values with the calculated capacity.

Use --dry-run to only display capacity without updating the ClusterQueue.`,
		Run: run,
	}

	cmd.PersistentFlags().BoolP(FlagVerbose, "v", false, "Enable verbose logging")
	cmd.Flags().StringP(FlagLabelSelector, "l", "", "Label selector to filter nodes (e.g., 'node-role.kubernetes.io/worker=true')")
	cmd.Flags().StringP(FlagResources, "r", "cpu,memory,nvidia.com/gpu", "Comma-separated list of resources to track (e.g., 'cpu,memory,nvidia.com/gpu')")
	cmd.Flags().StringP(FlagClusterQueue, "c", "default", "Name of the ClusterQueue to monitor")
	cmd.Flags().Bool(FlagDryRun, false, "Only display capacity without updating the ClusterQueue")

	defaultKubeconfig := ""
	if home := homedir.HomeDir(); home != "" {
		defaultKubeconfig = filepath.Join(home, ".kube", "config")
	}
	cmd.Flags().String(FlagKubeconfig, defaultKubeconfig, "Path to kubeconfig file (uses in-cluster config if not specified)")

	_ = viper.BindPFlag(FlagVerbose, cmd.PersistentFlags().Lookup(FlagVerbose))
	_ = viper.BindPFlag(FlagLabelSelector, cmd.Flags().Lookup(FlagLabelSelector))
	_ = viper.BindPFlag(FlagKubeconfig, cmd.Flags().Lookup(FlagKubeconfig))
	_ = viper.BindPFlag(FlagResources, cmd.Flags().Lookup(FlagResources))
	_ = viper.BindPFlag(FlagClusterQueue, cmd.Flags().Lookup(FlagClusterQueue))
	_ = viper.BindPFlag(FlagDryRun, cmd.Flags().Lookup(FlagDryRun))

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

	kubeconfig := viper.GetString(FlagKubeconfig)
	labelSelector := viper.GetString(FlagLabelSelector)
	resourcesStr := viper.GetString(FlagResources)
	clusterQueueName := viper.GetString(FlagClusterQueue)
	dryRun := viper.GetBool(FlagDryRun)

	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		log.ErrorContext(ctx, "Failed to build Kubernetes config", "error", err)
		os.Exit(1)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.ErrorContext(ctx, "Failed to create Kubernetes clientset", "error", err)
		os.Exit(1)
	}

	kueueClient, err := kueueclientset.NewForConfig(config)
	if err != nil {
		log.ErrorContext(ctx, "Failed to create Kueue clientset", "error", err)
		os.Exit(1)
	}

	controller := NewController(log, clientset, kueueClient, labelSelector, resourcesStr, clusterQueueName, dryRun)
	controller.Run(ctx)
}
