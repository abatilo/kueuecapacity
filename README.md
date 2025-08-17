# kueuecapacity

You can indicate capacity to [kueue](https://github.com/kubernetes-sigs/kueue)
by configuring your [kind:
ClusterQueue](https://kueue.sigs.k8s.io/docs/concepts/cluster_queue/) resources.
This works quite well in cloud environments where you have the ability to auto
scaling capacity from your compute provider and can always have a reasonable
amount of confidence that whatever you configure your `nominalQuota` to, there
will be capacity available in the region.

This model does not work as well when you're in an environment where you're
expected to operate with fixed capacity, and that capacity is not consistently
available due to hardware failures. In this case, your `kind: ClusterQueue`
might be configured to make workload scheduling decisions with an inaccurate
understanding of the world.

In an ideal world, kueue has the capability of dynamically counting how much
available quota there is in the cluster. As of `August 17, 2025`, this does not
seem to be the case, and thus, `kueuecapacity` has been created to solve this
problem. There are [existing
issues](https://github.com/kubernetes-sigs/kueue/issues/3183) discussing this
capability, but these conversations have seemingly gone stale.

`kueuecapacity` will take in a configuration that you provide that will
synchronize between a list of available nodes, and then will update the
available quota that's configured on a target `kind: ClusterQueue`.
