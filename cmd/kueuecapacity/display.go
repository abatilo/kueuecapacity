package main

import (
	"fmt"
	"math"
	"os"
	"strings"
	"text/tabwriter"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	kueuev1beta1 "sigs.k8s.io/kueue/apis/kueue/v1beta1"
)

// displayCapacityInformation displays the capacity information for nodes
func (c *Controller) displayCapacityInformation(nodes []any, nodesByFlavor map[string][]*corev1.Node, flavors map[string]*kueuev1beta1.ResourceFlavor) map[string]*resource.Quantity {
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
		title := fmt.Sprintf("ResourceFlavor: %s", flavorName)
		if flavor != nil && len(flavor.Spec.NodeLabels) > 0 {
			title += fmt.Sprintf(" (nodeLabels: %v)", flavor.Spec.NodeLabels)
		}

		subtotals := printNodesTable(title, flavorNodes, c.resources)

		for res, quantity := range subtotals {
			grandTotals[res].Add(*quantity)
		}
	}

	if len(unmatchedNodes) > 0 {
		subtotals := printNodesTable("Unmatched Nodes:", unmatchedNodes, c.resources)

		for res, quantity := range subtotals {
			grandTotals[res].Add(*quantity)
		}
	}

	return grandTotals
}

// printNodesTable prints a table of nodes with their resource capacity
func printNodesTable(title string, nodes []*corev1.Node, resources []string) map[string]*resource.Quantity {
	fmt.Printf("\n%s\n", title)

	subtotals := make(map[string]*resource.Quantity)
	for _, res := range resources {
		subtotals[res] = resource.NewQuantity(0, resource.DecimalSI)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)

	header := "Node"
	separator := "----"
	for _, res := range resources {
		header += "\t" + res
		separator += "\t" + strings.Repeat("-", len(res))
	}
	_, _ = fmt.Fprintln(w, header)
	_, _ = fmt.Fprintln(w, separator)

	for _, node := range nodes {
		capacity := node.Status.Capacity

		row := node.Name

		for _, res := range resources {
			resName := corev1.ResourceName(res)
			quantity := capacity[resName]

			if subtotals[res] != nil {
				subtotals[res].Add(quantity)
			}

			formatted := formatResourceQuantity(res, quantity)
			row += "\t" + formatted
		}

		_, _ = fmt.Fprintln(w, row)
	}

	if len(nodes) > 0 {
		_, _ = fmt.Fprintln(w, separator)
		subtotalRow := "Subtotal"
		for _, res := range resources {
			quantity := subtotals[res]
			formatted := formatResourceQuantity(res, *quantity)
			subtotalRow += "\t" + formatted
		}
		_, _ = fmt.Fprintln(w, subtotalRow)
	}

	_ = w.Flush()
	return subtotals
}

// printGrandTotals prints the grand totals of all resources
func printGrandTotals(resources []string, grandTotals map[string]*resource.Quantity) {
	fmt.Println("\n----------------------------------------")
	fmt.Print("GRAND TOTAL: ")
	for i, res := range resources {
		quantity := grandTotals[res]
		formatted := formatResourceQuantity(res, *quantity)

		if i > 0 {
			fmt.Print(", ")
		}
		fmt.Printf("%s=%s", res, formatted)
	}
	fmt.Println("\n========================================")
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
