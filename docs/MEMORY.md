# Memory Configuration of Mountpoint for Amazon S3 CSI Driver

[Mountpoint for Amazon S3](https://github.com/awslabs/mountpoint-s3) buffers object data in memory while reading (e.g. prefetching ahead of your application) and while writing (holding parts until they are uploaded). How much memory it may use directly affects how fast your volumes are and, because every mount on a node shares one container, how safely mounts coexist.

This document explains the memory model and how to size it for your workload.

## The memory model

One `s3-csi-daemonset-mounter` Pod runs on each node and hosts **every** Mountpoint process on that node, one `mount-s3` process per distinct volume. All of those processes share that single container's memory.

The driver therefore gives each Mountpoint process an explicit budget using Mountpoint's [`--memory-target`](https://github.com/awslabs/mountpoint-s3/blob/main/doc/CONFIGURATION.md#configuring-memory-usage) argument, derived from two Helm values:

```yaml
daemonsetMounters:
  - maxVolumesPerNode: 4          # how many distinct S3 volumes a node may mount
    memoryLimitStrategy: equalSplit
    resources:
      requests:
        memory: "4Gi"             # the node-wide budget for all Mountpoint processes
        cpu: "500m"
```

With the default `equalSplit` strategy:

```
--memory-target (MiB) = (requests.memory in MiB − 64 − in-memory cache size) ÷ maxVolumesPerNode
```

The 64 MiB is held back for the mounter's own process, which lives in the same container but is invisible to Mountpoint's accounting. The in-memory cache size is `cache.emptyDir.sizeLimit` when the mounter's cache uses `medium: Memory`. That cache is a tmpfs whose pages are charged to the mounter's memory, so it is held back as well; it is zero for a disk or EBS cache, or no cache. Always set `sizeLimit` on an in-memory cache, since without one nothing is held back and the tmpfs can grow without bound.

With the defaults above, which configure no cache, every Mountpoint process gets `--memory-target=1008`, i.e. `(4096 − 64) ÷ 4`.

Three properties of this model matter when sizing it:

* **The budget is a per-node reservation.** `requests.memory` is requested on every node as soon as the driver is installed, whether or not any S3 volume is mounted. The 4Gi default is roughly 28% of the allocatable memory of a 16 GiB node.
* **The split is static.** Each process gets the same share regardless of how busy it is. One busy mount on a node with `maxVolumesPerNode: 4` is capped at a quarter of the budget even when the other three slots are empty.
* **The budget is per volume, not per workload Pod.** Workload Pods on the same node that use the same volume share its Mountpoint process, and therefore its budget.

`--memory-target` is a target, not a hard cap: Mountpoint slows I/O down, reclaims buffers and reduces prefetching to stay near it, rather than failing.

**Keep the targets within `requests.memory`.** The request is all the scheduler reserves for the mounter, so any memory used beyond it comes out of what was scheduled for other Pods. The mounter is `system-node-critical`, so under node memory pressure the kubelet never evicts it; it evicts your workload Pods instead.

The chart sets no `resources.limits.memory`. If you add one, leave headroom above `requests.memory`: the equal split hands out almost the whole request as targets, and an OOM kill of the mounter container can stop every mount on the node.

## Memory and throughput

**More memory per mount lets Mountpoint prefetch further ahead and keep more data in flight, which raises throughput until something else becomes the limit**, such as the instance's network bandwidth or CPU, FUSE overhead, or how your application reads and writes. Past that point, a higher target doesn't add throughput.

Where that point falls depends on the instance type and the workload, so there is no single recommended value. On smaller instances, network bandwidth or CPU is often already the limit at the default target of 1008 MiB, while on high-bandwidth instances a larger target can raise throughput substantially. Benchmark your own workload before settling on a value.

Two other related settings matter for throughput:

* Each Mountpoint process independently detects the node's full network bandwidth and sizes its request concurrency for it, so several busy mounts on one node all compete for the same link. If several mounts are busy at once, consider partitioning the bandwidth explicitly with [`--maximum-throughput-gbps`](https://github.com/awslabs/mountpoint-s3/blob/main/doc/CONFIGURATION.md#configuring-mountpoint-performance) in each PV's `mountOptions`.
* Bandwidth detection reads the instance type from [IMDS](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/configuring-instance-metadata-service.html). If IMDS is unreachable, Mountpoint assumes 10 Gbps. For the mounter, IMDS needs a hop limit of 2 or greater; see [Troubleshooting the automatic STS region detection](CONFIGURATION.md#troubleshooting-the-automatic-sts-region-detection) for how to check it. Volumes using [Pod-Level Credentials](CONFIGURATION.md#pod-level-credentials) never query IMDS, so set `--maximum-throughput-gbps` on them explicitly if the node is faster than 10 Gbps.

## Sizing the mounter memory request

Work in this order:

1. **Pick `maxVolumesPerNode`**: the largest number of *distinct* S3 volumes you expect a single node to mount. This is also advertised to the Kubernetes scheduler as the node's volume limit, so it caps how many S3 volumes are placed on a node. Prefer the smallest number that fits your workload, because a lower value means a larger share for each volume.
2. **Pick a per-mount target.** As a rough guide from tests with parallel sequential reads and writes: on a `t3.xlarge`, a target above the default 1008 MiB gave no more throughput; on a 100 Gbps `c6in.16xlarge`, one mount reached about 37 Gbps at 2 GiB and 65–70 Gbps at 4–6 GiB, with no gain above that.
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

## Verifying the configuration

The mounter logs the resolved strategy and the inputs it used when it starts:

```sh
$ kubectl logs -n kube-system <mounter-pod> | grep memoryLimitStrategy
memoryLimitStrategy=equalSplit: each Mountpoint gets --memory-target=1008 (this container's resources.requests.memory of 4294967296 bytes, minus 64 MiB for the mounter process, divided by maxVolumesPerNode=4).
```

After a `helm upgrade`, a node shows the new values only once its mounter Pod has been replaced, because the mounter DaemonSet uses the `OnDelete` update strategy.

To find the mounter Pod on a particular node:

```sh
kubectl get pods -n kube-system -l app=s3-csi-daemonset-mounter --field-selector spec.nodeName=<node>
```

The mounter also repeats the effective share in its periodic status line, alongside the live mount count:

```
Status: tracked=2 actual_children=2 open_fds=14 goroutines=21 memory_limit_strategy=equalSplit share_mib=1008 mounts=[...]
```

Mountpoint's own startup lines are forwarded into the same log, prefixed with the mount ID, and confirm the memory target each process accepted and the network bandwidth it detected:

```sh
kubectl logs -n kube-system <mounter-pod> | grep -e "memory target" -e "target network throughput"
```

## Setting `--memory-target` per volume

If an equal split does not suit your workload, for example when one volume needs far more memory than the others on the same node, set `memoryLimitStrategy: none` and specify `--memory-target` per volume in the PV's `mountOptions`.

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

With `none`, the driver passes your value through untouched, so keeping the targets within the mounter's `requests.memory` is up to you. The scheduler counts volumes against `maxVolumesPerNode` but knows nothing about their targets, so size the request for the largest set of volumes that can end up on one node: 64 MiB, plus the in-memory cache size if any, plus the targets of your `maxVolumesPerNode` largest volumes, or of all your volumes if you have fewer. Each volume runs at most one Mountpoint process per node however many Pods use it, which is why the example above needs no more than the sum of its two targets. If you have more large volumes than one node can hold at once, either size for the worst case or keep them apart, for example with pod anti-affinity between the workloads that use them.

> [!WARNING]
> A PV with no `--memory-target` gets Mountpoint's default: 95% of the mounter container's `resources.limits.memory`, or of roughly the whole node's memory if no limit is set. Every such mount computes the same figure independently; it is not divided between them. Set `--memory-target` on every PV when using `none`. The mounter logs a warning naming any volume that doesn't.

`memoryLimitStrategy: none` is also the only strategy compatible with `maxVolumesPerNode: 0` (no scheduler volume limit), since there is then nothing to divide the budget by. With both, nothing bounds how much memory the mounts on a node can use.

Under `equalSplit`, a `--memory-target` in a PV's `mountOptions` is **ignored** and the equal share is used instead. The mounter logs a warning naming the volume, so check the mounter log if a per-volume target does not seem to take effect.
