package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	kueuev1beta1 "sigs.k8s.io/kueue/apis/kueue/v1beta1"
	kueueclientset "sigs.k8s.io/kueue/client-go/clientset/versioned"
	kueueinformers "sigs.k8s.io/kueue/client-go/informers/externalversions"
)

// Controller holds the shared dependencies for the kueuecapacity controller
type Controller struct {
	log              *slog.Logger
	kueueClient      *kueueclientset.Clientset
	clientset        *kubernetes.Clientset
	clusterQueueName string
	resources        []string
	dryRun           bool
	labelSelector    string
}

// NewController creates a new Controller instance
func NewController(log *slog.Logger, clientset *kubernetes.Clientset, kueueClient *kueueclientset.Clientset, labelSelector, resourcesStr, clusterQueueName string, dryRun bool) *Controller {
	resources := strings.Split(resourcesStr, ",")
	for i := range resources {
		resources[i] = strings.TrimSpace(resources[i])
	}

	return &Controller{
		log:              log,
		kueueClient:      kueueClient,
		clientset:        clientset,
		clusterQueueName: clusterQueueName,
		resources:        resources,
		dryRun:           dryRun,
		labelSelector:    labelSelector,
	}
}

// Run starts the controller and watches for node changes
func (c *Controller) Run(ctx context.Context) {
	c.log.InfoContext(ctx, "Starting kueuecapacity",
		"labelSelector", c.labelSelector,
		"resources", c.resources,
		"clusterQueue", c.clusterQueueName,
		"dryRun", c.dryRun)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var factory informers.SharedInformerFactory
	if c.labelSelector != "" {
		factory = informers.NewSharedInformerFactoryWithOptions(
			c.clientset,
			0, // resync period
			informers.WithTweakListOptions(func(options *metav1.ListOptions) {
				options.LabelSelector = c.labelSelector
			}),
		)
	} else {
		factory = informers.NewSharedInformerFactory(c.clientset, 0)
	}

	nodeInformer := factory.Core().V1().Nodes().Informer()

	kueueFactory := kueueinformers.NewSharedInformerFactory(c.kueueClient, 0)
	resourceFlavorInformer := kueueFactory.Kueue().V1beta1().ResourceFlavors().Informer()

	if _, err := nodeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			node, ok := obj.(*corev1.Node)
			if ok {
				c.log.InfoContext(ctx, "Node added", "node", node.Name)
				c.handleNodeChange(ctx, "ADD", node.Name, nodeInformer)
			}
		},
		UpdateFunc: func(oldObj, newObj any) {
			node, ok := newObj.(*corev1.Node)
			if ok {
				c.log.InfoContext(ctx, "Node updated", "node", node.Name)
				c.handleNodeChange(ctx, "UPDATE", node.Name, nodeInformer)
			}
		},
		DeleteFunc: func(obj any) {
			node, ok := obj.(*corev1.Node)
			if ok {
				c.log.InfoContext(ctx, "Node deleted", "node", node.Name)
				c.handleNodeChange(ctx, "DELETE", node.Name, nodeInformer)
			}
		},
	}); err != nil {
		c.log.ErrorContext(ctx, "Error adding event handler", "error", err)
		os.Exit(1)
	}

	stopCh := make(chan struct{})
	defer close(stopCh)

	c.log.InfoContext(ctx, "Starting node informer with label selector", "selector", c.labelSelector)
	factory.Start(stopCh)
	kueueFactory.Start(stopCh)

	if !cache.WaitForCacheSync(runCtx.Done(), nodeInformer.HasSynced, resourceFlavorInformer.HasSynced) {
		c.log.ErrorContext(ctx, "Timed out waiting for caches to sync")
		os.Exit(1)
	}

	c.log.InfoContext(ctx, "Cache synced, watching for node changes...")

	c.handleNodeChange(ctx, "INITIAL", "startup", nodeInformer)

	select {
	case sig := <-sigChan:
		c.log.InfoContext(ctx, "Received signal, initiating graceful shutdown", "signal", sig)
		cancel()

		c.handleNodeChange(ctx, "FINAL", "shutdown", nodeInformer)

		c.log.InfoContext(ctx, "Graceful shutdown complete")
	case <-runCtx.Done():
		c.log.InfoContext(ctx, "Context cancelled")
	}
}

// handleNodeChange processes node changes and updates the display
func (c *Controller) handleNodeChange(ctx context.Context, eventType string, triggerNode string, nodeInformer cache.SharedIndexInformer) {
	nodes := nodeInformer.GetStore().List()
	fmt.Printf("\n[%s] Event triggered by: %s\n", eventType, triggerNode)
	fmt.Printf("ClusterQueue: %s\n", c.clusterQueueName)
	fmt.Printf("Current nodes matching selector '%s': %d\n", c.labelSelector, len(nodes))
	fmt.Printf("Tracking resources: %v\n", c.resources)
	fmt.Println("========================================")

	flavors, err := c.getResourceFlavors(ctx)
	if err != nil {
		c.log.WarnContext(ctx, "Failed to get ResourceFlavors", "error", err)
		flavors = make(map[string]*kueuev1beta1.ResourceFlavor)
	}

	nodesByFlavor := c.groupNodesByFlavor(nodes, flavors)

	grandTotals := c.displayCapacityInformation(nodes, nodesByFlavor, flavors)

	printGrandTotals(c.resources, grandTotals)

	if !c.dryRun {
		c.updateClusterQueueQuotas(ctx, nodesByFlavor)
	} else {
		fmt.Println("\n🔍 Dry-run mode: ClusterQueue will not be updated")
	}
}

// groupNodesByFlavor groups nodes by their matching resource flavors
func (c *Controller) groupNodesByFlavor(nodes []any, flavors map[string]*kueuev1beta1.ResourceFlavor) map[string][]*corev1.Node {
	nodesByFlavor := make(map[string][]*corev1.Node)

	for _, obj := range nodes {
		if node, ok := obj.(*corev1.Node); ok {
			matchedFlavors := matchNodeToFlavors(node, flavors)
			for _, flavorName := range matchedFlavors {
				nodesByFlavor[flavorName] = append(nodesByFlavor[flavorName], node)
			}
		}
	}

	return nodesByFlavor
}
