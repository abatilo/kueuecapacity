package main

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"strings"
	"text/tabwriter"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	kueuev1beta1 "sigs.k8s.io/kueue/apis/kueue/v1beta1"
)

// displayCapacityInformation displays the capacity information for nodes
func (c *Controller) displayCapacityInformation(ctx context.Context, nodes []any, nodesByFlavor map[string][]*corev1.Node, flavors map[string]*kueuev1beta1.ResourceFlavor) map[string]*resource.Quantity {
	grandTotals := make(map[string]*resource.Quantity)
	for _, res := range c.resources {
		grandTotals[res] = resource.NewQuantity(0, resource.DecimalSI)
	}

	var unmatchedNodes []*corev1.Node
	matchedNodeNames := make(map[string]bool)
	for _, flavorNodes := range nodesByFlavor {
		for _, node := range flavorNodes {
			matchedNodeNames[node.Name] = true
		}
	}

	for _, obj := range nodes {
		if node, ok := obj.(*corev1.Node); ok {
			if !matchedNodeNames[node.Name] {
				unmatchedNodes = append(unmatchedNodes, node)
			}
		}
	}

	for flavorName, flavorNodes := range nodesByFlavor {
		flavor := flavors[flavorName]
		nodeLabels := map[string]string{}
		if flavor != nil && len(flavor.Spec.NodeLabels) > 0 {
			nodeLabels = flavor.Spec.NodeLabels
		}

		subtotals := c.logNodesTable(ctx, "ResourceFlavor", flavorName, nodeLabels, flavorNodes)

		for res, quantity := range subtotals {
			grandTotals[res].Add(*quantity)
		}
	}

	if len(unmatchedNodes) > 0 {
		subtotals := c.logNodesTable(ctx, "Unmatched", "none", nil, unmatchedNodes)

		for res, quantity := range subtotals {
			grandTotals[res].Add(*quantity)
		}
	}

	return grandTotals
}

// logNodesTable logs a table of nodes with their resource capacity
func (c *Controller) logNodesTable(ctx context.Context, flavorType string, flavorName string, nodeLabels map[string]string, nodes []*corev1.Node) map[string]*resource.Quantity {
	subtotals := make(map[string]*resource.Quantity)
	for _, res := range c.resources {
		subtotals[res] = resource.NewQuantity(0, resource.DecimalSI)
	}

	// Build the table in a buffer for structured logging
	var buf bytes.Buffer
	w := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)

	header := "Node"
	separator := "----"
	for _, res := range c.resources {
		header += "\t" + res
		separator += "\t" + strings.Repeat("-", len(res))
	}
	_, _ = fmt.Fprintln(w, header)
	_, _ = fmt.Fprintln(w, separator)

	// Build node details for structured logging
	nodeDetails := make([]map[string]any, 0, len(nodes))

	for _, node := range nodes {
		capacity := node.Status.Capacity
		row := node.Name

		nodeCapacity := make(map[string]string)
		for _, res := range c.resources {
			resName := corev1.ResourceName(res)
			quantity := capacity[resName]

			if subtotals[res] != nil {
				subtotals[res].Add(quantity)
			}

			formatted := formatResourceQuantity(res, quantity)
			row += "\t" + formatted
			nodeCapacity[res] = formatted
		}

		_, _ = fmt.Fprintln(w, row)

		nodeDetails = append(nodeDetails, map[string]any{
			"name":     node.Name,
			"capacity": nodeCapacity,
		})
	}

	if len(nodes) > 0 {
		_, _ = fmt.Fprintln(w, separator)
		subtotalRow := "Subtotal"
		subtotalValues := make(map[string]string)
		for _, res := range c.resources {
			quantity := subtotals[res]
			formatted := formatResourceQuantity(res, *quantity)
			subtotalRow += "\t" + formatted
			subtotalValues[res] = formatted
		}
		_, _ = fmt.Fprintln(w, subtotalRow)

		_ = w.Flush()

		// Log as structured data
		c.log.DebugContext(ctx, "Node capacity summary",
			"flavorType", flavorType,
			"flavorName", flavorName,
			"nodeLabels", nodeLabels,
			"nodeCount", len(nodes),
			"nodes", nodeDetails,
			"subtotals", subtotalValues,
			"tableOutput", buf.String())
	}

	return subtotals
}

// formatResourceQuantity formats a resource quantity for display
func formatResourceQuantity(resourceType string, quantity resource.Quantity) string {
	if resourceType == "memory" || resourceType == "ephemeral-storage" || resourceType == "storage" {
		gi := math.Floor(float64(quantity.Value()) / (1024 * 1024 * 1024))
		return fmt.Sprintf("%.0f Gi", gi)
	} else if quantity.IsZero() {
		return "0"
	} else {
		return quantity.String()
	}
}
