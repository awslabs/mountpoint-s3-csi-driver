# Memory Configuration of Mountpoint for Amazon S3 CSI Driver

[Mountpoint for Amazon S3](https://github.com/awslabs/mountpoint-s3) buffers object data in memory while reading (e.g. prefetching ahead of your application) and while writing (holding parts until they are uploaded). How much memory it may use directly affects how fast your volumes are and, because every mount on a node shares one container, how safely mounts coexist.

This document explains the memory model and how to size it for your workload.

## The memory model

One `s3-csi-daemonset-mounter` DaemonSet Pod runs on each node and hosts **every** Mountpoint process on that node, one `mount-s3` process per PersistentVolume mounted on the node. All of those processes share that single container's memory.

The driver therefore bounds each Mountpoint process with Mountpoint's [`--memory-target`](https://github.com/awslabs/mountpoint-s3/blob/main/doc/CONFIGURATION.md#configuring-memory-usage) argument. `--memory-target` is not a guaranteed limit, but Mountpoint manages its buffers to stay within it: under memory pressure it slows down I/O, reclaims buffers it no longer needs, and reduces prefetching.

How each process gets its `--memory-target` is set by the `memoryLimitStrategy` Helm value:

* [`equalSplit`](#the-equalsplit-strategy) (default): the driver splits the mounter's `resources.requests.memory` equally between `maxVolumesPerNode` volumes and ignores any `--memory-target` in a PV's `mountOptions`.
* [`none`](#the-none-strategy): you set `--memory-target` per volume in each PV's `mountOptions` and size `resources.requests.memory` to cover them.

Whichever strategy you use:

* **The budget is a per-node reservation.** `requests.memory` is requested on every node as soon as the driver is installed, whether or not any S3 volume is mounted. The 4Gi default is roughly 28% of the allocatable memory of a 16 GiB node.
* **The budget is per volume, not per workload Pod.** Workload Pods on the same node that use the same volume share its Mountpoint process, and therefore its budget.

**Keep the targets within `requests.memory`.** The request is all the scheduler reserves for the mounter, so any memory used beyond it comes out of what was scheduled for other Pods. The mounter is `system-node-critical`, so under node memory pressure the kubelet never evicts it; it evicts your workload Pods instead.

> [!WARNING]
> The chart sets no `resources.limits.memory`. If you add one, leave headroom above `requests.memory`. The targets add up to almost the whole request, and Mountpoint's [`--memory-target` is not a guaranteed limit](https://github.com/awslabs/mountpoint-s3/blob/main/doc/CONFIGURATION.md#configuring-memory-usage), so a limit equal to the request risks an OOM kill of the mounter container, which can stop every mount on the node.

## Memory and throughput

**More memory per mount lets Mountpoint prefetch further ahead and keep more data in flight, which raises throughput until something else becomes the limit**, such as the instance's network bandwidth or CPU, FUSE overhead, or how your application reads and writes. Once one of those is the limit, raising the target adds no more throughput.

The target at which throughput stops improving depends on the instance type and the workload, so there is no single recommended value. On smaller instances, network bandwidth or CPU is often already the limit at the default target of 1008 MiB, while on high-bandwidth instances a larger target can raise throughput substantially. Benchmark your own workload before settling on a value.

Two other related settings matter for throughput:

* Each Mountpoint process independently detects the node's full network bandwidth and sizes its request concurrency for it, so several busy mounts on one node all compete for the same link. If several mounts are busy at once, consider partitioning the bandwidth explicitly with [`--maximum-throughput-gbps`](https://github.com/awslabs/mountpoint-s3/blob/main/doc/CONFIGURATION.md#configuring-mountpoint-performance) in each PV's `mountOptions`.
* Mountpoint detects the node's bandwidth by reading its instance type from [IMDS](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/configuring-instance-metadata-service.html), and uses 10 Gbps if IMDS is unreachable. The mounter doesn't use the host network, so it can reach IMDS only if the node's IMDS hop limit is 2 or greater. If it isn't and the node is faster than 10 Gbps, set `--maximum-throughput-gbps` in the PV's `mountOptions` rather than raising the hop limit, which would also expose the node's IAM credentials to all Pods on the node. Volumes using [Pod-Level Credentials](CONFIGURATION.md#pod-level-credentials) never query IMDS, so on such nodes set `--maximum-throughput-gbps` on them whatever the hop limit. To check the bandwidth each mount detected, see [Verifying the configuration](#verifying-the-configuration).

## The `equalSplit` strategy

`equalSplit` is the default. It gives each Mountpoint process an equal share of the mounter's memory request as its `--memory-target`, derived from two Helm values:

```yaml
daemonsetMounters:
  - maxVolumesPerNode: 4          # how many distinct S3 volumes a node may mount
    memoryLimitStrategy: equalSplit
    resources:
      requests:
        memory: "4Gi"             # the node-wide budget for all Mountpoint processes
        cpu: "500m"
```

Each Mountpoint process gets:

```
--memory-target (MiB) = (requests.memory in MiB − 64 − in-memory cache size) ÷ maxVolumesPerNode
```

The 64 MiB is held back for the mounter's own process, which lives in the same container but is invisible to Mountpoint's accounting. The in-memory cache size is `cache.emptyDir.sizeLimit` when the mounter's cache uses `medium: Memory`. That cache is a tmpfs whose pages are charged to the mounter's memory, so it is held back as well; it is zero for a disk or EBS cache, or no cache. Always set `sizeLimit` on an in-memory cache, since without one nothing is held back and the tmpfs can grow without bound.

With the defaults above, which configure no cache, every Mountpoint process gets `--memory-target=1008`, i.e. `(4096 − 64) ÷ 4`.

**The split is static.** Each process gets the same share regardless of how busy it is. One busy mount on a node with `maxVolumesPerNode: 4` is capped at a quarter of the budget even when the other three slots are empty.

A `--memory-target` in a PV's `mountOptions` is **ignored** and the equal share is used instead. The mounter logs a warning naming the volume, so check the mounter log if a per-volume target does not seem to take effect.

### Sizing the mounter memory request

Work in this order:

1. **Pick `maxVolumesPerNode`**: the largest number of distinct S3 PersistentVolumes a single node should mount at once. It is also advertised to the Kubernetes scheduler as the node's volume limit: Pods that would exceed it are placed on another node, or stay `Pending` until one has room. To choose it:
   * Count the distinct S3 PVs used by the Pods you expect to run together on one node. A Pod that mounts more S3 PVs than `maxVolumesPerNode` can never be scheduled.
   * If you are upgrading from v2, count the `MountpointS3PodAttachment` objects on each node at a busy time. The first column is the count; the top line is your busiest node:
     ```sh
     kubectl get s3pa -o json | jq -r '.items[].spec.nodeName' | sort | uniq -c | sort -rn | head
     ```
   * Prefer the smallest number that fits, because a lower value means a larger share for each volume.
2. **Pick a per-mount target.** As a rough guide from tests with parallel sequential reads and writes: on a `t3.xlarge`, raising the target above the default 1008 MiB didn't increase throughput; on a 100 Gbps `c6in.16xlarge`, one mount reached about 37 Gbps at 2 GiB and 65–70 Gbps at 4–6 GiB, and throughput plateaued above that.
3. **Compute the request:** `requests.memory = 64 MiB + in-memory cache size + (target × maxVolumesPerNode)`.
4. **Check that it fits** alongside your workloads on the smallest node type in the cluster, remembering that it is reserved on every node.

Mountpoint's minimum `--memory-target` is 512 MiB, which sets a floor on the request. The table assumes no in-memory cache; if you have one, add its size to every request figure:

| `maxVolumesPerNode` | Minimum `requests.memory` | Per-mount target at 4Gi | Per-mount target at 8Gi | Per-mount target at 16Gi |
| ------------------- | ------------------------- | ----------------------- | ----------------------- | ------------------------ |
| 1                   | 576Mi                     | 4032 MiB                | 8128 MiB                | 16320 MiB                |
| 2                   | 1088Mi                    | 2016 MiB                | 4064 MiB                | 8160 MiB                 |
| 4 (default)         | 2112Mi                    | 1008 MiB (default)      | 2032 MiB                | 4080 MiB                 |
| 8                   | 4160Mi                    | *rejected* (504 MiB)    | 1016 MiB                | 2040 MiB                 |
| 16                  | 8256Mi                    | *rejected* (252 MiB)    | *rejected* (508 MiB)    | 1020 MiB                 |

Combinations that would give a share below 512 MiB are rejected.

The memory target also caps how many files a mount can have open for writing at once (109 at the default 1008 MiB); see [Maximum number of files open for writing](https://github.com/awslabs/mountpoint-s3/blob/main/doc/CONFIGURATION.md#maximum-number-of-files-open-for-writing).

## The `none` strategy

If an equal split does not suit your workload, for example when one volume needs far more memory than the others on the same node, set `memoryLimitStrategy: none` and specify `--memory-target` per volume in the PV's `mountOptions`.

### Setting `--memory-target` per volume

For example, with one volume read at high throughput and another used only for uploading logs:

```yaml
daemonsetMounters:
  - maxVolumesPerNode: 2
    memoryLimitStrategy: none
    resources:
      requests:
        memory: "6720Mi"   # 64Mi + 6144Mi + 512Mi
        cpu: "500m"
    logLevel: 4
```

```yaml
apiVersion: v1
kind: PersistentVolume
metadata:
  name: s3-pv-reader
spec:
  # ...
  mountOptions:
    - memory-target 6144   # MiB
---
apiVersion: v1
kind: PersistentVolume
metadata:
  name: s3-pv-logs
spec:
  # ...
  mountOptions:
    - memory-target 512    # MiB, the minimum
```

See [`memory_target_per_volume.yaml`](../examples/kubernetes/static_provisioning/memory_target_per_volume.yaml) for the complete example.

### Sizing the mounter memory request

With `none`, the driver passes each PV's `--memory-target` through unchanged. It is your responsibility to ensure that the targets of the volumes mounted on a node fit within the mounter's `requests.memory`.

The scheduler places up to `maxVolumesPerNode` S3 volumes on a node but doesn't know their targets, so assume that the `maxVolumesPerNode` PVs with the highest `--memory-target` can end up on the same node, and size for them:

```
requests.memory = 64 MiB + in-memory cache size + sum of the maxVolumesPerNode highest --memory-target values among your PVs
```

If you have no more PVs than `maxVolumesPerNode`, this is the sum of all their targets, as in the example above: 64 + 6144 + 512 = 6720 MiB. Each PV counts once per node, however many Pods on the node use it, because they share one Mountpoint process.

For example, with `maxVolumesPerNode: 2` and three PVs with targets of 6144, 4096 and 512 MiB, size for the two highest: 64 + 6144 + 4096 = 10304 MiB. If that is too much to reserve on every node, keep the 6144 MiB and 4096 MiB volumes on different nodes, for example with pod anti-affinity between the workloads that use them, and size for the highest pair that can still share a node: 64 + 6144 + 512 = 6720 MiB.

> [!WARNING]
> A PV with no `--memory-target` gets Mountpoint's default: 95% of the mounter container's `resources.limits.memory`, or of roughly the whole node's memory if no limit is set. Every such mount computes the same figure independently; it is not divided between them. Set `--memory-target` on every PV when using `none`. The mounter logs a warning naming any volume that doesn't.

`memoryLimitStrategy: none` is also the only strategy compatible with `maxVolumesPerNode: 0` (no scheduler volume limit), since there is then nothing to divide the budget by. With both, nothing bounds how much memory the mounts on a node can use.

## Verifying the configuration

The mounter logs the resolved strategy and the inputs it used when it starts:

```sh
$ kubectl logs -n kube-system <mounter-pod> | grep memoryLimitStrategy
memoryLimitStrategy=equalSplit: each Mountpoint gets --memory-target=1008 (this container's resources.requests.memory of 4294967296 bytes, minus 64 MiB for the mounter process, divided by maxVolumesPerNode=4).
```

With `none`, the line is `memoryLimitStrategy=none: --memory-target is left to each PV's mountOptions.` instead.

After a `helm upgrade`, a node shows the new values only once its mounter Pod has been replaced, because the mounter DaemonSet uses the `OnDelete` update strategy.

To find the mounter Pod on a particular node:

```sh
kubectl get pods -n kube-system -l app=s3-csi-daemonset-mounter --field-selector spec.nodeName=<node>
```

Under `equalSplit`, the mounter also repeats the effective share in its periodic status line, alongside the live mount count:

```
Status: tracked=2 actual_children=2 open_fds=14 goroutines=21 memory_limit_strategy=equalSplit share_mib=1008 mounts=[...]
```

Mountpoint's own startup lines are forwarded into the same log, prefixed with the mount ID, and confirm the memory target each process accepted and the network bandwidth it detected:

```sh
kubectl logs -n kube-system <mounter-pod> | grep -e "memory target" -e "target network throughput"
```
