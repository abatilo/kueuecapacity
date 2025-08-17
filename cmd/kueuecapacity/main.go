package main

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

func main() {
	viper.SetEnvPrefix("KC")
	viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	viper.AutomaticEnv()

	cmd := &cobra.Command{
		Use:   "kueuecapacity",
		Short: "Watch and monitor Kubernetes node events",
		Long: `kueuecapacity watches Kubernetes nodes for changes and events.
It monitors node additions, updates, and deletions in real-time,
with support for label-based filtering to focus on specific node groups.`,
		Run: run,
	}

	cmd.PersistentFlags().BoolP(FlagVerbose, "v", false, "Enable verbose logging")
	cmd.Flags().StringP(FlagLabelSelector, "l", "", "Label selector to filter nodes (e.g., 'node-role.kubernetes.io/worker=true')")
	cmd.Flags().StringP(FlagResources, "r", "cpu,memory,nvidia.com/gpu", "Comma-separated list of resources to track (e.g., 'cpu,memory,nvidia.com/gpu')")

	// Set default kubeconfig path
	defaultKubeconfig := ""
	if home := homedir.HomeDir(); home != "" {
		defaultKubeconfig = filepath.Join(home, ".kube", "config")
	}
	cmd.Flags().String(FlagKubeconfig, defaultKubeconfig, "Path to kubeconfig file (uses in-cluster config if not specified)")

	viper.BindPFlag(FlagVerbose, cmd.PersistentFlags().Lookup(FlagVerbose))
	viper.BindPFlag(FlagLabelSelector, cmd.Flags().Lookup(FlagLabelSelector))
	viper.BindPFlag(FlagKubeconfig, cmd.Flags().Lookup(FlagKubeconfig))
	viper.BindPFlag(FlagResources, cmd.Flags().Lookup(FlagResources))

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

	// Get configuration values
	kubeconfig := viper.GetString(FlagKubeconfig)
	labelSelector := viper.GetString(FlagLabelSelector)
	resourcesStr := viper.GetString(FlagResources)

	// Parse resources list
	resources := strings.Split(resourcesStr, ",")
	for i := range resources {
		resources[i] = strings.TrimSpace(resources[i])
	}

	log.InfoContext(ctx, "Starting kueuecapacity",
		"kubeconfig", kubeconfig,
		"labelSelector", labelSelector,
		"resources", resources)

	// Build Kubernetes config
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		log.ErrorContext(ctx, "Failed to build Kubernetes config", "error", err)
		os.Exit(1)
	}

	// Create Kubernetes clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.ErrorContext(ctx, "Failed to create Kubernetes clientset", "error", err)
		os.Exit(1)
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Create SharedInformerFactory for Nodes with label selector
	var factory informers.SharedInformerFactory
	if labelSelector != "" {
		factory = informers.NewSharedInformerFactoryWithOptions(
			clientset,
			0, // resync period
			informers.WithTweakListOptions(func(options *metav1.ListOptions) {
				options.LabelSelector = labelSelector
			}),
		)
	} else {
		factory = informers.NewSharedInformerFactory(clientset, 0)
	}

	nodeInformer := factory.Core().V1().Nodes().Informer()

	// Helper function to print all matching nodes with resource capacity
	printMatchingNodes := func(eventType string, triggerNode string) {
		nodes := nodeInformer.GetStore().List()
		fmt.Printf("\n[%s] Event triggered by: %s\n", eventType, triggerNode)
		fmt.Printf("Current nodes matching selector '%s': %d\n", labelSelector, len(nodes))
		fmt.Printf("Tracking resources: %v\n", resources)
		fmt.Println("========================================")

		// Initialize aggregates map for dynamic resources
		totals := make(map[string]*resource.Quantity)
		for _, res := range resources {
			totals[res] = resource.NewQuantity(0, resource.DecimalSI)
		}

		// Create a tabwriter for formatted output
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)

		// Build dynamic header
		header := "Node"
		separator := "----"
		for _, res := range resources {
			header += "\t" + res
			separator += "\t" + strings.Repeat("-", len(res))
		}
		fmt.Fprintln(w, header)
		fmt.Fprintln(w, separator)

		for _, obj := range nodes {
			if node, ok := obj.(*corev1.Node); ok {
				// Extract capacity from node
				capacity := node.Status.Capacity

				// Build row starting with node name
				row := node.Name

				// Process each configured resource
				for _, res := range resources {
					resName := corev1.ResourceName(res)
					quantity := capacity[resName]

					// Add to totals
					if totals[res] != nil {
						totals[res].Add(quantity)
					}

					// Format the value based on resource type
					var formatted string
					if res == "memory" || res == "ephemeral-storage" || res == "storage" {
						// Memory/storage resources - show in GiB
						gi := math.Floor(float64(quantity.Value()) / (1024 * 1024 * 1024))
						formatted = fmt.Sprintf("%.0f Gi", gi)
					} else if quantity.IsZero() {
						// Resource not present
						formatted = "0"
					} else {
						// Other resources - show as is
						formatted = quantity.String()
					}

					row += "\t" + formatted
				}

				fmt.Fprintln(w, row)
			}
		}

		// Print separator
		fmt.Fprintln(w, separator)

		// Print totals
		totalRow := "TOTAL"
		for _, res := range resources {
			quantity := totals[res]

			var formatted string
			if res == "memory" || res == "ephemeral-storage" || res == "storage" {
				// Memory/storage resources - show in GiB
				gi := math.Floor(float64(quantity.Value()) / (1024 * 1024 * 1024))
				formatted = fmt.Sprintf("%.0f Gi", gi)
			} else if quantity.IsZero() {
				formatted = "0"
			} else {
				// Other resources - show as is
				formatted = quantity.String()
			}

			totalRow += "\t" + formatted
		}
		fmt.Fprintln(w, totalRow)

		w.Flush()
		fmt.Println("========================================")
	}

	// Add event handlers
	if _, err := nodeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			node, ok := obj.(*corev1.Node)
			if ok {
				log.InfoContext(ctx, "Node added", "node", node.Name)
				printMatchingNodes("ADD", node.Name)
			}
		},
		UpdateFunc: func(oldObj, newObj any) {
			node, ok := newObj.(*corev1.Node)
			if ok {
				log.InfoContext(ctx, "Node updated", "node", node.Name)
				printMatchingNodes("UPDATE", node.Name)
			}
		},
		DeleteFunc: func(obj any) {
			node, ok := obj.(*corev1.Node)
			if ok {
				log.InfoContext(ctx, "Node deleted", "node", node.Name)
				printMatchingNodes("DELETE", node.Name)
			}
		},
	}); err != nil {
		log.ErrorContext(ctx, "Error adding event handler", "error", err)
		os.Exit(1)
	}

	// Start the informer
	stopCh := make(chan struct{})
	defer close(stopCh)

	log.InfoContext(ctx, "Starting node informer with label selector", "selector", labelSelector)
	factory.Start(stopCh)

	// Wait for cache sync
	if !cache.WaitForCacheSync(runCtx.Done(), nodeInformer.HasSynced) {
		log.ErrorContext(ctx, "Timed out waiting for caches to sync")
		os.Exit(1)
	}

	log.InfoContext(ctx, "Cache synced, watching for node changes...")

	// Print initial state
	printMatchingNodes("INITIAL", "startup")

	// Wait for shutdown signal
	select {
	case sig := <-sigChan:
		log.InfoContext(ctx, "Received signal, initiating graceful shutdown", "signal", sig)
		cancel()

		// Print final state
		printMatchingNodes("FINAL", "shutdown")

		log.InfoContext(ctx, "Graceful shutdown complete")
	case <-runCtx.Done():
		log.InfoContext(ctx, "Context cancelled")
	}
}
