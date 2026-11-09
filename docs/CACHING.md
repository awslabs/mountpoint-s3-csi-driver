# Caching Configuration of Mountpoint for Amazon S3 CSI Driver

[Mountpoint for Amazon S3](https://github.com/awslabs/mountpoint-s3) supports caching file system metadata and object content to reduce cost and improve performance for repeated reads to the same file. See [Mountpoint's caching configuration](https://github.com/awslabs/mountpoint-s3/blob/main/doc/CONFIGURATION.md#caching-configuration) for more details about caching.

## Metadata Cache

The `metadata-ttl <SECONDS|indefinite|minimal>` flag in `mountOptions` controls the time-to-live (TTL) for cached metadata entries. It can be set to a positive numerical value in seconds, or to one of the pre-configured values of `minimal` (default configuration when not using [Data Cache](#data-cache)) or `indefinite` (metadata entries never expire).

```yaml
apiVersion: v1
kind: PersistentVolume
metadata:
  name: s3-pv
spec:
  mountOptions:
    - metadata-ttl indefinite # <SECONDS|indefinite|minimal>
  csi:
    driver: s3.csi.aws.com
    # ...
```

See [Mountpoint's documentation](https://github.com/awslabs/mountpoint-s3/blob/main/doc/CONFIGURATION.md#metadata-cache) for more details about metadata cache.

## Local Data Cache

To move v1 or v2 PVs that enable cache to v3, start with [Upgrading to v3](UPGRADING_TO_V3.md). For the v2 driver, see the [v2 caching guide](https://github.com/awslabs/mountpoint-s3-csi-driver/blob/v2/docs/CACHING.md).

To use a local cache:

1. Choose how PVs share a node's cache volume with `cache.limitStrategy`. We recommend `equalSplit`.
2. Configure the cache volumes: a `cache` block in each `daemonsetMounters` entry, in your Helm values or the EKS add-on's configuration values. With several entries, each entry sets its own cache volume.
3. Set `cache: enabled` on each PV to opt in to use the cache volume.

### How the cache works

One `s3-csi-daemonset-mounter` DaemonSet Pod runs on each node and hosts **every** Mountpoint process on that node, one `mount-s3` process per PersistentVolume mounted on the node. All of those processes share a single cache volume if configured, each PV using its own directory. A PV's cache on a node is deleted once no pod there uses the PV.

Mountpoint processes do not know how much cache the other processes use, so without a cap on each PV, one PV can take the cache space that the others need.

### Step 1: Choose how PVs share the cache volume

`cache.limitStrategy`, required in each `cache` block, decides where each PV's `max-cache-size` comes from. Note `maxVolumesPerNode` is the scheduler's limit of S3 volumes per node; [Sizing the mounter memory request](MEMORY.md#sizing-the-mounter-memory-request) in MEMORY.md shows how to choose it.

**We recommend `equalSplit`.** `equalSplit` gives every PV on a node an equal share of the cache volume. 

**Choose `none` only if** PVs on the same node need different cache sizes, or you need to disable the limit on S3 volumes per node (via `maxVolumesPerNode: 0`).

|                                                                                                                                                                          | `equalSplit` (recommended)                                                                      | `none`                                                                                                                                     |
| ------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | ----------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------ |
| Each PV's cap                                                                                                                                                            | 95% of the cache volume ÷ `maxVolumesPerNode`                                                   | the PV's `max-cache-size`, in MiB                                                                                                          |
| A PV that sets `max-cache-size`                                                                                                                                          | ignored, with a warning in the mounter's log                                                    | used as set                                                                                                                                |
| A PV without `max-cache-size`                                                                                                                                            | gets the equal share                                                                            | **no cap**, with a warning in the mounter's log                                                                                            |
| Size the cache volume as                                                                                                                                                 | the largest working set one PV re-reads × `maxVolumesPerNode` ÷ 0.95                            | the sum of the `maxVolumesPerNode` largest `max-cache-size` values ÷ 0.95                                                                  |
| [`maxVolumesPerNode`](https://kubernetes.io/docs/concepts/storage/storage-limits/) (maximum number of S3 volumes on one node; setting it to 0 disables the volume limit) | must be above 0                                                                                 | can be 0 only if `memoryLimitStrategy` is also `none` (see [the `none` strategy](MEMORY.md#the-none-strategy) in MEMORY.md)                |
| Cost                                                                                                                                                                     | on a node running fewer cache-enabled PVs than `maxVolumesPerNode`, the spare shares stay empty | you size every PV and the volume yourself, and a PV without a cap can fill the volume, which on the node's disk gets workload pods evicted |

The 5% is reserved headroom because Mountpoint evicts cached data only after a write has gone over its cap. 

#### Under `none` strategy

Since the scheduler counts S3 volumes instead of cache sizes, any `maxVolumesPerNode` of your cache-enabled PVs can land on one node. With `maxVolumesPerNode: 0`, nothing bounds the total cache size. 

We highly recommend you to set `max-cache-size` on every PV that enables cache:

```yaml
spec:
  mountOptions:
    - max-cache-size 5000   # in MiB
```

### Step 2: Give each node a cache volume

Add a `cache` block, with exactly one of `emptyDir` or `ephemeral`, to each `daemonsetMounters` entry whose nodes' S3 workloads require cache. Here are some examples, with example changes to `values.yaml` for Helm installation method. EKS Add-on users can reference the same style to apply to the `daemonsetMounters` entries.

| Cache volume                                      | Use it when                                                                  | Cost                                         |
| ------------------------------------------------- | ---------------------------------------------------------------------------- | -------------------------------------------- |
| Node disk (`emptyDir`)                            | you want the simplest setup                                                  | shares the node's disk with other processes  |
| Memory (`emptyDir` with `medium: "Memory"`)       | you want the lowest latency, and the data you read repeatedly fits in memory | reserves `sizeLimit` of memory on every node |
| EBS volume (`ephemeral` with an EBS StorageClass) | you want the cache to be away from the node's root disk                      | an EBS volume for every node                 |

#### Node disk

```yaml
daemonsetMounters:
  - maxVolumesPerNode: 4
    ...
    cache:
      emptyDir:
        sizeLimit: "10Gi"      # each of 4 PVs gets 10 Gi / 4 * 95% = 2432 MiB
      limitStrategy: equalSplit
```

On the node's disk, the kubelet enforces `sizeLimit` by evicting the Pod, and it never evicts the `system-node-critical` mounter pod, so only the cap from Step 1 bounds the cache. Choose a `sizeLimit` that fits in the disk's free space, or else if disk space gets full the workload pods would be evicted via [Node Pressure Eviction](https://kubernetes.io/docs/concepts/scheduling-eviction/node-pressure-eviction).

See [Kubernetes's documentation](https://kubernetes.io/docs/concepts/storage/volumes/#emptydir) for more details about `emptyDir`.

#### Memory (tmpfs)

```yaml
    resources:
      requests:
        memory: "6Gi"          # 4Gi + 2Gi tmpfs
        cpu: "500m"
    cache:
      emptyDir:
        sizeLimit: "2Gi"
        medium: "Memory"
      limitStrategy: equalSplit
```

A tmpfs is charged to the mounter's memory, so raise `resources.requests.memory` by `sizeLimit`. [MEMORY.md](MEMORY.md#the-equalsplit-strategy) shows the effect on each PV's memory share.

#### EBS volume

First, make sure to install the EBS CSI Driver in your cluster by following their [installation guide](https://github.com/kubernetes-sigs/aws-ebs-csi-driver/blob/master/docs/install.md).

You can then create a StorageClass and replace `cache` block with:

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: s3-cache-ebs-sc
provisioner: ebs.csi.aws.com
reclaimPolicy: Delete # delete each volume with its mounter Pod
volumeBindingMode: WaitForFirstConsumer
parameters: # all optional, see https://github.com/kubernetes-sigs/aws-ebs-csi-driver/blob/master/docs/parameters.md for more details
  type: gp3
```

```yaml
    cache:
      ephemeral:
        storageClassName: s3-cache-ebs-sc
        resourceRequests: 40Gi
      limitStrategy: equalSplit
```

### Step 3: Enable the cache on a PV

```yaml
apiVersion: v1
kind: PersistentVolume
metadata:
  name: s3-pv
spec:
  # ...
  csi:
    driver: s3.csi.aws.com
    volumeHandle: s3-csi-driver-volume
    volumeAttributes:
      bucketName: amzn-s3-demo-bucket
      cache: enabled
```

| `cache` value                         | Result                                   |
| ------------------------------------- | ---------------------------------------- |
| unset, or `disabled`                  | no cache                                 |
| `enabled`                             | uses the node's cache                    |
| `emptyDir` or `ephemeral` (v2 values) | uses the node's cache, whatever its type |
| anything else                         | the PV fails to mount                    |

Note: on a node whose with no configured cache volume, a PV that sets `cache: enabled` will fail to mount.

### Verifying the configuration

The mounter logs its cache strategy when it starts, and the warnings in the [Step 1](#step-1-choose-how-pvs-share-the-cache-volume) table when a PV mounts:

```sh
$ kubectl logs -n kube-system <mounter-pod> | grep cache.limitStrategy
cache.limitStrategy=equalSplit: each Mountpoint gets --max-cache-size=2432 (95% of this container's cache volume of 10737418240 bytes, divided by maxVolumesPerNode=4).
```

With `none` strategy, the line is `cache.limitStrategy=none: --max-cache-size is left to each PV's mountOptions.` instead. [Verifying the configuration](MEMORY.md#verifying-the-configuration) in MEMORY.md shows how to find `<mounter-pod>`.

### Shared Cache

When mounting an S3 bucket, you can opt in to a shared cache in [Amazon S3 Express One Zone](https://aws.amazon.com/s3/storage-classes/express-one-zone/). You should use the shared cache if you repeatedly read small objects (up to 1 MB) from multiple compute instances, or the size of the dataset that you repeatedly read often exceeds the size of your local cache. This improves latency when reading the same data repeatedly from multiple instances by avoiding redundant requests to your mounted S3 bucket. To enable shared cache, specify the `cache-xz` flag in `mountOptions` with your directory bucket name:

```yaml
apiVersion: v1
kind: PersistentVolume
metadata:
  name: s3-pv
spec:
  mountOptions:
    - cache-xz amzn-s3-demo-bucket--usw2-az1--x-s3
  csi:
    driver: s3.csi.aws.com
    # ...
```

See [Mountpoint's documentation](https://github.com/awslabs/mountpoint-s3/blob/main/doc/CONFIGURATION.md#shared-cache) for more details about shared cache.

### Combined Local and Shared Cache

You can opt in to a local cache and shared cache together if you have unused space on your instance, but also want to share the cache across multiple instances. This avoids redundant read requests from the same instance to the shared cache in S3 directory bucket when the required data is cached in local storage, reducing request cost as well as improving performance. To opt in to local and shared cache together, you can specify both the [Local Cache](#local-cache) and [Shared Cache](#shared-cache) in your PV:

```yaml
apiVersion: v1
kind: PersistentVolume
metadata:
  name: s3-pv
spec:
  mountOptions:
    - cache-xz amzn-s3-demo-bucket--usw2-az1--x-s3
  csi:
    driver: s3.csi.aws.com
    # ...
    volumeAttributes:
      bucketName: amzn-s3-demo-bucket
      cache: emptyDir
      cacheEmptyDirSizeLimit: 2Gi
```

See [Mountpoint's documentation](https://github.com/awslabs/mountpoint-s3/blob/main/doc/CONFIGURATION.md#combined-local-and-shared-cache) for more details about combined local and shared cache.


<!-- TODO: How to change cache volume configurations (point towards guide on updating config with drain / delete process) -->
<!-- TODO: How to change cache PV configurations (point towards guide on PV) -->
