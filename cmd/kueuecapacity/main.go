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
	kueuev1beta1 "sigs.k8s.io/kueue/apis/kueue/v1beta1"
	kueueclientset "sigs.k8s.io/kueue/client-go/clientset/versioned"
	kueueinformers "sigs.k8s.io/kueue/client-go/informers/externalversions"
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
	cmd.Flags().StringP(FlagClusterQueue, "c", "default", "Name of the ClusterQueue to monitor")
	cmd.Flags().BoolP(FlagUpdateClusterQueue, "u", false, "Update the ClusterQueue with calculated resource quotas")

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
	viper.BindPFlag(FlagClusterQueue, cmd.Flags().Lookup(FlagClusterQueue))
	viper.BindPFlag(FlagUpdateClusterQueue, cmd.Flags().Lookup(FlagUpdateClusterQueue))

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
	clusterQueueName := viper.GetString(FlagClusterQueue)

	// Parse resources list
	resources := strings.Split(resourcesStr, ",")
	for i := range resources {
		resources[i] = strings.TrimSpace(resources[i])
	}

	log.InfoContext(ctx, "Starting kueuecapacity",
		"kubeconfig", kubeconfig,
		"labelSelector", labelSelector,
		"resources", resources,
		"clusterQueue", clusterQueueName)

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

	// Create Kueue clientset
	kueueClient, err := kueueclientset.NewForConfig(config)
	if err != nil {
		log.ErrorContext(ctx, "Failed to create Kueue clientset", "error", err)
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

	// Create Kueue informer factory for ResourceFlavors
	kueueFactory := kueueinformers.NewSharedInformerFactory(kueueClient, 0)
	resourceFlavorInformer := kueueFactory.Kueue().V1beta1().ResourceFlavors().Informer()

	// Helper function to get resource flavors from ClusterQueue
	getResourceFlavors := func() (map[string]*kueuev1beta1.ResourceFlavor, error) {
		clusterQueue, err := kueueClient.KueueV1beta1().ClusterQueues().Get(ctx, clusterQueueName, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to get ClusterQueue %s: %w", clusterQueueName, err)
		}

		flavors := make(map[string]*kueuev1beta1.ResourceFlavor)
		for _, rg := range clusterQueue.Spec.ResourceGroups {
			for _, fq := range rg.Flavors {
				flavorName := string(fq.Name)
				flavor, err := kueueClient.KueueV1beta1().ResourceFlavors().Get(ctx, flavorName, metav1.GetOptions{})
				if err != nil {
					log.WarnContext(ctx, "Failed to get ResourceFlavor", "name", flavorName, "error", err)
					continue
				}
				flavors[flavorName] = flavor
			}
		}
		return flavors, nil
	}

	// Helper function to update ClusterQueue quotas
	updateClusterQueueQuotas := func(ctx context.Context, log *slog.Logger, kueueClient *kueueclientset.Clientset, clusterQueueName string, resources []string, nodesByFlavor map[string][]*corev1.Node) {
		log.InfoContext(ctx, "Updating ClusterQueue resource quotas", "clusterQueue", clusterQueueName)
		
		// Get the current ClusterQueue
		cq, err := kueueClient.KueueV1beta1().ClusterQueues().Get(ctx, clusterQueueName, metav1.GetOptions{})
		if err != nil {
			log.ErrorContext(ctx, "Failed to get ClusterQueue", "error", err)
			return
		}
		
		// Find the ResourceGroup with matching coveredResources
		var targetGroupIndex int = -1
		for i, rg := range cq.Spec.ResourceGroups {
			// Check if coveredResources match our tracked resources
			if len(rg.CoveredResources) == len(resources) {
				match := true
				for _, res := range resources {
					found := false
					for _, coveredRes := range rg.CoveredResources {
						if string(coveredRes) == res {
							found = true
							break
						}
					}
					if !found {
						match = false
						break
					}
				}
				if match {
					targetGroupIndex = i
					break
				}
			}
		}
		
		if targetGroupIndex == -1 {
			log.WarnContext(ctx, "No matching ResourceGroup found in ClusterQueue", 
				"requiredResources", resources,
				"availableGroups", len(cq.Spec.ResourceGroups))
			return
		}
		
		// Calculate totals per ResourceFlavor
		flavorTotals := make(map[string]map[string]*resource.Quantity)
		for flavorName, nodes := range nodesByFlavor {
			flavorTotals[flavorName] = make(map[string]*resource.Quantity)
			for _, res := range resources {
				flavorTotals[flavorName][res] = resource.NewQuantity(0, resource.DecimalSI)
			}
			
			for _, node := range nodes {
				for _, res := range resources {
					resName := corev1.ResourceName(res)
					quantity := node.Status.Capacity[resName]
					flavorTotals[flavorName][res].Add(quantity)
				}
			}
		}
		
		// Update the FlavorQuotas in the target ResourceGroup
		for i, fq := range cq.Spec.ResourceGroups[targetGroupIndex].Flavors {
			flavorName := string(fq.Name)
			if totals, exists := flavorTotals[flavorName]; exists {
				// Update each resource quota
				for j, rq := range fq.Resources {
					resourceName := string(rq.Name)
					if newQuota, ok := totals[resourceName]; ok {
						oldQuota := rq.NominalQuota.DeepCopy()
						cq.Spec.ResourceGroups[targetGroupIndex].Flavors[i].Resources[j].NominalQuota = *newQuota
						log.InfoContext(ctx, "Updating resource quota",
							"flavor", flavorName,
							"resource", resourceName,
							"oldQuota", oldQuota.String(),
							"newQuota", newQuota.String())
					}
				}
			} else {
				// Set quotas to zero for flavors with no nodes
				for j := range fq.Resources {
					resourceName := string(fq.Resources[j].Name)
					cq.Spec.ResourceGroups[targetGroupIndex].Flavors[i].Resources[j].NominalQuota = *resource.NewQuantity(0, resource.DecimalSI)
					log.InfoContext(ctx, "Setting resource quota to zero",
						"flavor", flavorName,
						"resource", resourceName)
				}
			}
		}
		
		// Update the ClusterQueue
		updated, err := kueueClient.KueueV1beta1().ClusterQueues().Update(ctx, cq, metav1.UpdateOptions{})
		if err != nil {
			log.ErrorContext(ctx, "Failed to update ClusterQueue", "error", err)
			return
		}
		
		log.InfoContext(ctx, "Successfully updated ClusterQueue",
			"clusterQueue", clusterQueueName,
			"generation", updated.Generation)
		fmt.Printf("\n✓ ClusterQueue '%s' updated successfully\n", clusterQueueName)
	}

	// Helper function to match node to resource flavors
	matchNodeToFlavors := func(node *corev1.Node, flavors map[string]*kueuev1beta1.ResourceFlavor) []string {
		var matchedFlavors []string
		for name, flavor := range flavors {
			if flavor.Spec.NodeLabels == nil || len(flavor.Spec.NodeLabels) == 0 {
				continue
			}
			
			// Check if node has all labels specified in the flavor
			matched := true
			for key, value := range flavor.Spec.NodeLabels {
				if nodeValue, exists := node.Labels[key]; !exists || nodeValue != value {
					matched = false
					break
				}
			}
			if matched {
				matchedFlavors = append(matchedFlavors, name)
			}
		}
		return matchedFlavors
	}

	// Helper function to print all matching nodes with resource capacity
	printMatchingNodes := func(eventType string, triggerNode string) {
		nodes := nodeInformer.GetStore().List()
		fmt.Printf("\n[%s] Event triggered by: %s\n", eventType, triggerNode)
		fmt.Printf("ClusterQueue: %s\n", clusterQueueName)
		fmt.Printf("Current nodes matching selector '%s': %d\n", labelSelector, len(nodes))
		fmt.Printf("Tracking resources: %v\n", resources)
		fmt.Println("========================================")

		// Get resource flavors from ClusterQueue
		flavors, err := getResourceFlavors()
		if err != nil {
			log.WarnContext(ctx, "Failed to get ResourceFlavors", "error", err)
			// Fall back to simple output without grouping
			flavors = make(map[string]*kueuev1beta1.ResourceFlavor)
		}

		// Group nodes by resource flavor
		nodesByFlavor := make(map[string][]*corev1.Node)
		var unmatchedNodes []*corev1.Node

		for _, obj := range nodes {
			if node, ok := obj.(*corev1.Node); ok {
				matchedFlavors := matchNodeToFlavors(node, flavors)
				if len(matchedFlavors) == 0 {
					unmatchedNodes = append(unmatchedNodes, node)
				} else {
					for _, flavorName := range matchedFlavors {
						nodesByFlavor[flavorName] = append(nodesByFlavor[flavorName], node)
					}
				}
			}
		}

		// Initialize grand totals
		grandTotals := make(map[string]*resource.Quantity)
		for _, res := range resources {
			grandTotals[res] = resource.NewQuantity(0, resource.DecimalSI)
		}

		// Helper function to print nodes table
		printNodesTable := func(title string, nodes []*corev1.Node) map[string]*resource.Quantity {
			fmt.Printf("\n%s\n", title)
			
			// Initialize subtotals
			subtotals := make(map[string]*resource.Quantity)
			for _, res := range resources {
				subtotals[res] = resource.NewQuantity(0, resource.DecimalSI)
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

			for _, node := range nodes {
				// Extract capacity from node
				capacity := node.Status.Capacity

				// Build row starting with node name
				row := node.Name

				// Process each configured resource
				for _, res := range resources {
					resName := corev1.ResourceName(res)
					quantity := capacity[resName]

					// Add to subtotals
					if subtotals[res] != nil {
						subtotals[res].Add(quantity)
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

			// Print subtotals
			if len(nodes) > 0 {
				fmt.Fprintln(w, separator)
				subtotalRow := "Subtotal"
				for _, res := range resources {
					quantity := subtotals[res]

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

					subtotalRow += "\t" + formatted
				}
				fmt.Fprintln(w, subtotalRow)
			}

			w.Flush()
			return subtotals
		}

		// Print nodes grouped by ResourceFlavor
		for flavorName, flavorNodes := range nodesByFlavor {
			flavor := flavors[flavorName]
			title := fmt.Sprintf("ResourceFlavor: %s", flavorName)
			if flavor != nil && len(flavor.Spec.NodeLabels) > 0 {
				title += fmt.Sprintf(" (nodeLabels: %v)", flavor.Spec.NodeLabels)
			}
			
			subtotals := printNodesTable(title, flavorNodes)
			
			// Add to grand totals
			for res, quantity := range subtotals {
				grandTotals[res].Add(*quantity)
			}
		}

		// Print unmatched nodes
		if len(unmatchedNodes) > 0 {
			subtotals := printNodesTable("Unmatched Nodes:", unmatchedNodes)
			
			// Add to grand totals
			for res, quantity := range subtotals {
				grandTotals[res].Add(*quantity)
			}
		}

		// Print grand totals
		fmt.Println("\n----------------------------------------")
		fmt.Print("GRAND TOTAL: ")
		for i, res := range resources {
			quantity := grandTotals[res]

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

			if i > 0 {
				fmt.Print(", ")
			}
			fmt.Printf("%s=%s", res, formatted)
		}
		fmt.Println("\n========================================")
		
		// Update ClusterQueue if requested
		if viper.GetBool(FlagUpdateClusterQueue) {
			updateClusterQueueQuotas(ctx, log, kueueClient, clusterQueueName, resources, nodesByFlavor)
		}
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
	kueueFactory.Start(stopCh)

	// Wait for cache sync
	if !cache.WaitForCacheSync(runCtx.Done(), nodeInformer.HasSynced, resourceFlavorInformer.HasSynced) {
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
