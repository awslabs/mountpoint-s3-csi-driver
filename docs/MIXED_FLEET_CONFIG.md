# Mixed Fleet Configuration of Mountpoint for Amazon S3 CSI Driver

By default, every node runs the driver with the same mounter settings: the same `maxVolumesPerNode`, memory request and cache. If your nodes differ in size or hardware, for example a NodePool of large-memory nodes or nodes with local NVMe disks, one set of settings either wastes the large nodes or doesn't fit the small ones. `additionalDaemonSets` lets you give each kind of node its own settings.

This document explains how the driver's DaemonSets are laid out, how to set up and change a mixed fleet, and what to watch for. It applies only when `experimental.mounterMode` is `daemonset`.

## How it works

The driver runs two DaemonSets on each node: a CSI node (`s3-csi-node`), which handles mount requests from the kubelet, and a mounter (`s3-csi-mounter`), which runs every Mountpoint process on the node. Together they form a **pair**: one CSI node and one mounter DaemonSet, which run on the same nodes.

* **The default pair** is `s3-csi-node` and `s3-csi-mounter`, from the top-level `node` and `mounter` values. It always renders.
* **Each `additionalDaemonSets` entry** adds one more pair, `s3-csi-node-<name>` and `s3-csi-mounter-<name>`.

An entry copies every setting from `node` and `mounter`, and the values it sets override the copy. So an entry only lists what's different for its nodes, such as `mounter.maxVolumesPerNode` and `mounter.resources`. Three things work differently:

* **Placement isn't copied.** Each entry must set its own `node.nodeSelector`, `node.affinity` or both, and both Pods of its pair use it.
* **An entry's `mounter.cache` replaces the copied cache whole** instead of merging with it, and `cache: null` gives the entry no cache. `helm upgrade --reuse-values` drops that `null` on Helm 3.20.0 and older, so the entry copies the default cache again.
* **`node.serviceAccount` and `node.preventPodSchedulingIfMissing` apply to every CSI node,** so they're only accepted at the top level.

Each CSI node uses the mounter on its own node, and advertises that mounter Pod's `maxVolumesPerNode` as the node's limit of S3 volumes. That's its pair's `mounter.maxVolumesPerNode`, once the mounter Pod runs with the current values. A PV needs no node affinity: a workload's S3 volume is mounted on whatever node its Pod is scheduled to, by that node's pair.

> [!WARNING]
> No node may match more than one pair, including the default pair. The driver supports one CSI node and mounter per node. On a node that matches more than one pair, the CSI node that started first keeps working and the others wait, but new mounts on that node can fail. See [My node runs more than one `s3-csi-node` Pod](TROUBLESHOOTING.md#my-node-runs-more-than-one-s3-csi-node-pod).
>
> The chart rejects an entry whose `nodeSelector` and `affinity` are identical to another pair's. It also rejects every entry when `node` has no `nodeSelector` and its `affinity` is empty or left at the default, since the default pair would then run on every entry's nodes. It can't detect any other overlap. For example, an entry with the same `nodeSelector` as `node` isn't rejected, since the default pair keeps the default `affinity`. Keeping placements apart is up to you.

## Setting up a mixed fleet

1. **Narrow the default pair's placement** so it stays off the entries' nodes, but still matches every other node that should run the driver. Set `node.nodeSelector` (see the three-pair example below), or add an expression to each `node.affinity` term, as here. Helm replaces lists whole, so copy both default terms. For example, this keeps the default pair on every node outside the `bigmem` and `gpu` NodePools, including nodes without a `karpenter.sh/nodepool` label:
   ```yaml
   node:
     affinity:
       nodeAffinity:
         requiredDuringSchedulingIgnoredDuringExecution:
           nodeSelectorTerms:
             - matchExpressions:
                 - { key: eks.amazonaws.com/compute-type, operator: NotIn, values: [fargate, hybrid, auto] }
                 - { key: karpenter.sh/nodepool, operator: NotIn, values: [bigmem, gpu] }
             - matchExpressions:
                 - { key: eks.amazonaws.com/compute-type, operator: In, values: [auto] }
                 - { key: karpenter.sh/nodepool, operator: NotIn, values: [system, bigmem, gpu] }
   ```
2. **Add one `additionalDaemonSets` entry per kind of node.** Give each entry a name of lowercase letters, numbers and `-`, starting and ending with a letter or number, since it names the entry's DaemonSets. Set its placement and the mounter settings its nodes need; see [Choosing each entry's values](#choosing-each-entrys-values).
3. **Cordon and drain every existing node that will stop matching the default pair**, including the entries' nodes. Narrowing the default pair deletes its mounter on those nodes, which breaks every S3 mount it serves. See [Updating the configuration](#updating-the-configuration). Nodes that don't exist yet, such as those of a new NodePool, need no drain. Give new NodePools the [node startup taint](CONFIGURATION.md#configure-node-startup-taint) (on Karpenter, in the NodePool's `spec.template.spec.startupTaints`), so Pods wait until the driver runs there.
4. Apply the change (`helm upgrade`, or update the EKS add-on's configuration values), then uncordon any nodes you drained.

For example, three pairs, one per Karpenter NodePool: the default pair on `general` nodes, `bigmem` on `bigmem` nodes and `gpu` on `gpu` nodes:

```yaml
node:
  nodeSelector:
    karpenter.sh/nodepool: general

additionalDaemonSets:
  bigmem:
    node:
      nodeSelector:
        karpenter.sh/nodepool: bigmem
    mounter:
      maxVolumesPerNode: 32
      resources:
        requests:
          memory: "32Gi"
          cpu: "2"
  gpu:
    node:
      affinity:
        nodeAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            nodeSelectorTerms:
              - matchExpressions:
                  - key: karpenter.sh/nodepool
                    operator: In
                    values: ["gpu"]
    mounter:
      maxVolumesPerNode: 8
      resources:
        requests:
          memory: "8Gi"
```

Each pair selects a different `karpenter.sh/nodepool` value, so no node matches two pairs.

This example assumes every node that runs S3 workloads is in the `general`, `bigmem` or `gpu` NodePool. If your cluster has other nodes, such as managed node groups or other NodePools, narrow the default pair with step 1's `node.affinity` instead, so it keeps running on them.

EKS add-on users set the same keys in the add-on's configuration values.
<!-- TODO: true once the EKS add-on's configuration schema accepts additionalDaemonSets, which is being added in parallel. -->

### Choosing each entry's values

Size each entry for the smallest node it selects. With `memoryLimitStrategy` (`equalSplit` by default) and `cache.limitStrategy` set to `equalSplit`, each volume gets an equal share of the mounter's memory request and cache, so when you raise an entry's `maxVolumesPerNode`, raise its `resources.requests.memory` and cache size with it. See [MEMORY.md](MEMORY.md#sizing-the-mounter-memory-request) and [CACHING.md](CACHING.md).
<!-- TODO: link the per-entry cache section of CACHING.md once #1041 merges (CACHING.md doesn't cover mounter.cache until then); it documents caches on additionalDaemonSets entries, so they aren't repeated here. -->

## Running workloads on the right nodes

Nothing changes in your PVs. To run a workload on one kind of node, give its Pod a `nodeSelector` or node affinity for those nodes, and the node's pair serves its volumes with that pair's settings. See [mixed_fleet.yaml](../examples/kubernetes/static_provisioning/mixed_fleet.yaml) for an example.

A PV that requests a local cache can only mount on nodes whose pair has a cache. If only some entries have one, pin the Pods that use such PVs to those entries' nodes.

## Updating the configuration

How a change reaches the nodes depends on which DaemonSet it changes:

* **CSI node settings** (`node`, or `additionalDaemonSets.<name>.node`) roll out when you apply the change, one node at a time. Restarting an `s3-csi-node` Pod doesn't affect the S3 volumes mounted on its node. New mounts on that node fail for a few seconds while it restarts, and the kubelet retries them. Placement (`nodeSelector`, `affinity`) is the exception: see the warning below.
* **Mounter settings** (`mounter`, or `additionalDaemonSets.<name>.mounter`, and the tolerations under `node`, which the mounter also uses) use the `OnDelete` update strategy, since replacing a mounter Pod breaks every S3 mount on its node. After you apply the change, each mounter Pod keeps its old settings until you replace it. To apply them on a node:
  ```bash
  $ kubectl cordon <node-name>
  $ kubectl drain <node-name> --ignore-daemonsets
  $ kubectl delete pod -n kube-system -l app=s3-csi-mounter --field-selector spec.nodeName=<node-name>
  $ kubectl uncordon <node-name>
  ```

> [!WARNING]
> When a pair stops matching a node, Kubernetes deletes its mounter Pod there right away, even with `OnDelete`, and every S3 mount it serves breaks. That happens when you remove or rename an entry, change a pair's placement (including narrowing the default pair's), or change a node's labels. Cordon and drain those nodes first, then apply the change, then uncordon them. If no other pair matches those nodes afterwards, S3 volumes can't mount there, so widen another pair's placement to cover them in the same change.
>
> When you fix an overlap, you don't need to drain a node if the pair you keep there is the one whose mounter serves the node's mounts. See [My node runs more than one `s3-csi-node` Pod](TROUBLESHOOTING.md#my-node-runs-more-than-one-s3-csi-node-pod).

To remove an entry, delete it from your values and run `helm upgrade` without `--reuse-values`, since `--reuse-values` keeps the entry even if you set it to `null`. Or run `helm upgrade --reset-then-reuse-values` with `additionalDaemonSets.<name>: null`.

## Verifying the configuration

Check which pair serves each node, and that no node runs more than one mounter (the second command prints nothing):

```bash
$ kubectl get pods -n kube-system -l app=s3-csi-mounter -o 'custom-columns=NODE:.spec.nodeName,PAIR:.metadata.ownerReferences[0].name'
$ kubectl get pods -n kube-system -l app=s3-csi-mounter -o jsonpath='{range .items[*]}{.spec.nodeName}{"\n"}{end}' | sort | uniq -d
```

Check the volume limit a node advertises, which should be its pair's `maxVolumesPerNode` once its mounter Pod runs with the current values:

```bash
$ kubectl get csinode <node-name> -o jsonpath='{.spec.drivers[?(@.name=="s3.csi.aws.com")].allocatable.count}'
```

On EKS before 1.34 and Kubernetes before 1.35, the kubelet reads the limit only when `s3-csi-node` starts, so it can show an old value. See [My Pod is stuck at `ContainerCreating` with error "no usable mounter on node"](TROUBLESHOOTING.md#my-pod-is-stuck-at-containercreating-with-error-no-usable-mounter-on-node).

## Troubleshooting

* [My node runs more than one `s3-csi-node` Pod](TROUBLESHOOTING.md#my-node-runs-more-than-one-s3-csi-node-pod)
* [My Pod is stuck at `ContainerCreating` with error "no usable mounter on node"](TROUBLESHOOTING.md#my-pod-is-stuck-at-containercreating-with-error-no-usable-mounter-on-node)
* [My `s3-csi-node` Pod logs "another CSI node on this node holds ... node.lock"](TROUBLESHOOTING.md#my-s3-csi-node-pod-logs-another-csi-node-on-this-node-holds--nodelock)
