package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	kueuev1beta1 "sigs.k8s.io/kueue/apis/kueue/v1beta1"
	kueueclientset "sigs.k8s.io/kueue/client-go/clientset/versioned"
	kueueinformers "sigs.k8s.io/kueue/client-go/informers/externalversions"
)

// EventType represents the type of node event
type EventType int

const (
	EventAdd EventType = iota
	EventUpdate
	EventDelete
	EventInitial
	EventFinal
)

// String returns the string representation of the EventType
func (e EventType) String() string {
	switch e {
	case EventAdd:
		return "ADD"
	case EventUpdate:
		return "UPDATE"
	case EventDelete:
		return "DELETE"
	case EventInitial:
		return "INITIAL"
	case EventFinal:
		return "FINAL"
	default:
		return "UNKNOWN"
	}
}

// IsImportant returns true for events that should be logged at INFO level
func (e EventType) IsImportant() bool {
	return e == EventAdd || e == EventDelete || e == EventInitial || e == EventFinal
}

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
				c.log.DebugContext(ctx, "Node added", "node", node.Name)
				c.handleNodeChange(ctx, EventAdd, node.Name, nodeInformer)
			}
		},
		UpdateFunc: func(oldObj, newObj any) {
			node, ok := newObj.(*corev1.Node)
			if ok {
				c.log.DebugContext(ctx, "Node updated", "node", node.Name)
				c.handleNodeChange(ctx, EventUpdate, node.Name, nodeInformer)
			}
		},
		DeleteFunc: func(obj any) {
			node, ok := obj.(*corev1.Node)
			if ok {
				c.log.DebugContext(ctx, "Node deleted", "node", node.Name)
				c.handleNodeChange(ctx, EventDelete, node.Name, nodeInformer)
			}
		},
	}); err != nil {
		c.log.ErrorContext(ctx, "Error adding event handler", "error", err)
		os.Exit(1)
	}

	stopCh := make(chan struct{})
	defer close(stopCh)

	c.log.DebugContext(ctx, "Starting node informer with label selector", "selector", c.labelSelector)
	factory.Start(stopCh)
	kueueFactory.Start(stopCh)

	if !cache.WaitForCacheSync(runCtx.Done(), nodeInformer.HasSynced, resourceFlavorInformer.HasSynced) {
		c.log.ErrorContext(ctx, "Timed out waiting for caches to sync")
		os.Exit(1)
	}

	c.log.DebugContext(ctx, "Cache synced, watching for node changes...")

	c.handleNodeChange(ctx, EventInitial, "startup", nodeInformer)

	select {
	case sig := <-sigChan:
		c.log.InfoContext(ctx, "Received signal, initiating graceful shutdown", "signal", sig)
		cancel()

		c.handleNodeChange(ctx, EventFinal, "shutdown", nodeInformer)

		c.log.InfoContext(ctx, "Graceful shutdown complete")
	case <-runCtx.Done():
		c.log.DebugContext(ctx, "Context cancelled")
	}
}

// handleNodeChange processes node changes and updates the display
func (c *Controller) handleNodeChange(ctx context.Context, eventType EventType, triggerNode string, nodeInformer cache.SharedIndexInformer) {
	nodes := nodeInformer.GetStore().List()
	c.log.DebugContext(ctx, "Node event triggered",
		"eventType", eventType.String(),
		"triggerNode", triggerNode,
		"clusterQueue", c.clusterQueueName,
		"nodeCount", len(nodes),
		"labelSelector", c.labelSelector,
		"resources", c.resources)

	flavors, err := c.getResourceFlavors(ctx)
	if err != nil {
		c.log.WarnContext(ctx, "Failed to get ResourceFlavors", "error", err)
		flavors = make(map[string]*kueuev1beta1.ResourceFlavor)
	}

	nodesByFlavor := c.groupNodesByFlavor(nodes, flavors)

	grandTotals := c.displayCapacityInformation(ctx, nodes, nodesByFlavor, flavors)

	c.logGrandTotals(ctx, grandTotals, nodesByFlavor, nodes, eventType)

	if !c.dryRun {
		c.updateClusterQueueQuotas(ctx, nodesByFlavor)
	} else {
		c.log.InfoContext(ctx, "Dry-run mode enabled", "message", "ClusterQueue will not be updated")
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

// logGrandTotals logs the grand totals of all resources and breakdown by flavor
func (c *Controller) logGrandTotals(ctx context.Context, grandTotals map[string]*resource.Quantity, nodesByFlavor map[string][]*corev1.Node, allNodes []any, eventType EventType) {
	// Log grand totals
	totals := make(map[string]string)
	for _, res := range c.resources {
		quantity := grandTotals[res]
		totals[res] = formatResourceQuantity(res, *quantity)
	}

	// Calculate per-flavor totals
	flavorBreakdown := make(map[string]map[string]any)
	for flavorName, nodes := range nodesByFlavor {
		flavorTotals := make(map[string]*resource.Quantity)
		for _, res := range c.resources {
			flavorTotals[res] = resource.NewQuantity(0, resource.DecimalSI)
		}

		for _, node := range nodes {
			for _, res := range c.resources {
				resName := corev1.ResourceName(res)
				quantity := node.Status.Capacity[resName]
				flavorTotals[res].Add(quantity)
			}
		}

		flavorSummary := make(map[string]string)
		for _, res := range c.resources {
			flavorSummary[res] = formatResourceQuantity(res, *flavorTotals[res])
		}

		flavorBreakdown[flavorName] = map[string]any{
			"nodeCount": len(nodes),
			"resources": flavorSummary,
		}
	}

	// Calculate unmatched nodes
	matchedNodeNames := make(map[string]bool)
	for _, flavorNodes := range nodesByFlavor {
		for _, node := range flavorNodes {
			matchedNodeNames[node.Name] = true
		}
	}

	var unmatchedNodes []*corev1.Node
	for _, obj := range allNodes {
		if node, ok := obj.(*corev1.Node); ok {
			if !matchedNodeNames[node.Name] {
				unmatchedNodes = append(unmatchedNodes, node)
			}
		}
	}

	// Add unmatched nodes to breakdown if any exist
	if len(unmatchedNodes) > 0 {
		unmatchedTotals := make(map[string]*resource.Quantity)
		for _, res := range c.resources {
			unmatchedTotals[res] = resource.NewQuantity(0, resource.DecimalSI)
		}

		for _, node := range unmatchedNodes {
			for _, res := range c.resources {
				resName := corev1.ResourceName(res)
				quantity := node.Status.Capacity[resName]
				unmatchedTotals[res].Add(quantity)
			}
		}

		unmatchedSummary := make(map[string]string)
		for _, res := range c.resources {
			unmatchedSummary[res] = formatResourceQuantity(res, *unmatchedTotals[res])
		}

		flavorBreakdown["_unmatched"] = map[string]any{
			"nodeCount": len(unmatchedNodes),
			"resources": unmatchedSummary,
		}
	}

	// Use INFO level for important events, DEBUG for routine updates
	if eventType.IsImportant() {
		c.log.InfoContext(ctx, "Cluster capacity summary",
			"eventType", eventType.String(),
			"grandTotals", totals,
			"resourceFlavors", flavorBreakdown)
	} else {
		c.log.DebugContext(ctx, "Cluster capacity summary",
			"eventType", eventType.String(),
			"grandTotals", totals,
			"resourceFlavors", flavorBreakdown)
	}
}
