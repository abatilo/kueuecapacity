package main

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kueuev1beta1 "sigs.k8s.io/kueue/apis/kueue/v1beta1"
)

// getResourceFlavors retrieves resource flavors from the ClusterQueue
func (c *Controller) getResourceFlavors(ctx context.Context) (map[string]*kueuev1beta1.ResourceFlavor, error) {
	clusterQueue, err := c.kueueClient.KueueV1beta1().ClusterQueues().Get(ctx, c.clusterQueueName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	flavors := make(map[string]*kueuev1beta1.ResourceFlavor)
	for _, rg := range clusterQueue.Spec.ResourceGroups {
		for _, fq := range rg.Flavors {
			flavorName := string(fq.Name)
			flavor, err := c.kueueClient.KueueV1beta1().ResourceFlavors().Get(ctx, flavorName, metav1.GetOptions{})
			if err != nil {
				c.log.WarnContext(ctx, "Failed to get ResourceFlavor", "name", flavorName, "error", err)
				continue
			}
			flavors[flavorName] = flavor
		}
	}
	return flavors, nil
}

// updateClusterQueueQuotas updates the ClusterQueue resource quotas based on node capacity
func (c *Controller) updateClusterQueueQuotas(ctx context.Context, nodesByFlavor map[string][]*corev1.Node) {
	c.log.InfoContext(ctx, "Updating ClusterQueue resource quotas", "clusterQueue", c.clusterQueueName)

	cq, err := c.kueueClient.KueueV1beta1().ClusterQueues().Get(ctx, c.clusterQueueName, metav1.GetOptions{})
	if err != nil {
		c.log.ErrorContext(ctx, "Failed to get ClusterQueue", "error", err)
		return
	}

	targetGroupIndex := c.findTargetResourceGroup(cq)
	if targetGroupIndex == -1 {
		c.log.WarnContext(ctx, "No matching ResourceGroup found in ClusterQueue",
			"requiredResources", c.resources,
			"availableGroups", len(cq.Spec.ResourceGroups))
		return
	}

	flavorTotals := c.calculateFlavorTotals(nodesByFlavor)

	c.updateFlavorQuotas(ctx, cq, targetGroupIndex, flavorTotals)

	updated, err := c.kueueClient.KueueV1beta1().ClusterQueues().Update(ctx, cq, metav1.UpdateOptions{})
	if err != nil {
		c.log.ErrorContext(ctx, "Failed to update ClusterQueue", "error", err)
		return
	}

	c.log.InfoContext(ctx, "Successfully updated ClusterQueue",
		"clusterQueue", c.clusterQueueName,
		"generation", updated.Generation,
		"status", "✓ Updated successfully")
}

// findTargetResourceGroup finds the ResourceGroup with matching coveredResources
func (c *Controller) findTargetResourceGroup(cq *kueuev1beta1.ClusterQueue) int {
	for i, rg := range cq.Spec.ResourceGroups {
		if len(rg.CoveredResources) == len(c.resources) {
			match := true
			for _, res := range c.resources {
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
				return i
			}
		}
	}
	return -1
}

// calculateFlavorTotals calculates resource totals per ResourceFlavor
func (c *Controller) calculateFlavorTotals(nodesByFlavor map[string][]*corev1.Node) map[string]map[string]*resource.Quantity {
	flavorTotals := make(map[string]map[string]*resource.Quantity)

	for flavorName, nodes := range nodesByFlavor {
		flavorTotals[flavorName] = make(map[string]*resource.Quantity)
		for _, res := range c.resources {
			flavorTotals[flavorName][res] = resource.NewQuantity(0, resource.DecimalSI)
		}

		for _, node := range nodes {
			for _, res := range c.resources {
				resName := corev1.ResourceName(res)
				quantity := node.Status.Capacity[resName]
				flavorTotals[flavorName][res].Add(quantity)
			}
		}
	}

	return flavorTotals
}

// updateFlavorQuotas updates the FlavorQuotas in the target ResourceGroup
func (c *Controller) updateFlavorQuotas(ctx context.Context, cq *kueuev1beta1.ClusterQueue, targetGroupIndex int, flavorTotals map[string]map[string]*resource.Quantity) {
	for i, fq := range cq.Spec.ResourceGroups[targetGroupIndex].Flavors {
		flavorName := string(fq.Name)
		if totals, exists := flavorTotals[flavorName]; exists {
			for j, rq := range fq.Resources {
				resourceName := string(rq.Name)
				if newQuota, ok := totals[resourceName]; ok {
					oldQuota := rq.NominalQuota.DeepCopy()
					cq.Spec.ResourceGroups[targetGroupIndex].Flavors[i].Resources[j].NominalQuota = *newQuota
					c.log.DebugContext(ctx, "Updating resource quota",
						"flavor", flavorName,
						"resource", resourceName,
						"oldQuota", oldQuota.String(),
						"newQuota", newQuota.String())
				}
			}
		} else {
			for j := range fq.Resources {
				resourceName := string(fq.Resources[j].Name)
				cq.Spec.ResourceGroups[targetGroupIndex].Flavors[i].Resources[j].NominalQuota = *resource.NewQuantity(0, resource.DecimalSI)
				c.log.DebugContext(ctx, "Setting resource quota to zero",
					"flavor", flavorName,
					"resource", resourceName)
			}
		}
	}
}

// matchNodeToFlavors matches a node to resource flavors based on node labels
func matchNodeToFlavors(node *corev1.Node, flavors map[string]*kueuev1beta1.ResourceFlavor) []string {
	var matchedFlavors []string
	for name, flavor := range flavors {
		if len(flavor.Spec.NodeLabels) == 0 {
			continue
		}

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
