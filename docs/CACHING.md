# Caching Configuration of Mountpoint for Amazon S3 CSI Driver

[Mountpoint for Amazon S3](https://github.com/awslabs/mountpoint-s3) supports caching file system metadata and object content to reduce cost and improve performance for repeated reads to the same file. See [Mountpoint's caching configuration](https://github.com/awslabs/mountpoint-s3/blob/main/doc/CONFIGURATION.md#caching-configuration) for more details about caching.

## Metadata Cache

The `metadata-ttl <SECONDS|indefinite|minimal>` flag in `mountOptions` controls the time-to-live (TTL) for cached metadata entries. It can be set to a positive numerical value in seconds, or to one of the pre-configured values of `minimal` (default configuration when not using a [local](#local-data-cache) or [shared](#shared-cache) data cache) or `indefinite` (metadata entries never expire).

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

One `s3-csi-mounter` DaemonSet Pod runs on each node and hosts all `mount-s3` processes, one per PersistentVolume mounted on the node. All `mount-s3` processes share a single cache volume if configured, with each PV using its own sub-directory. A PV's cache on a node is deleted once no pod there uses the PV. The mounter empties the cache volume at startup and exit, so cached data does not survive mounter restarts.

See [Mountpoint's documentation](https://github.com/awslabs/mountpoint-s3/blob/main/doc/CONFIGURATION.md#local-cache) for more details about local cache.

If upgrading from v1/v2 to v3, start with [Upgrading to v3](UPGRADING_TO_V3.md).

### Step 1: Choose how PVs share the cache volume

Mountpoint processes do not know how much cache the other processes use, so without a cap on each PV, one PV can take the cache space that the others need.

`cache.limitStrategy` is required in each `cache` block, and decides where each PV's `max-cache-size` comes from. Note `maxVolumesPerNode` is the limit of S3 volumes per node, and both the scheduler and the driver enforce this limit; [Sizing the mounter memory request](MEMORY.md#sizing-the-mounter-memory-request) in MEMORY.md shows how to choose it.

A PV's cache directory is deleted when its Mountpoint process exits or crashes. Until a deletion finishes, the directory still takes one of the node's `maxVolumesPerNode` slots, so the mounter may refuse a new mount.

We recommend `equalSplit` strategy. `equalSplit` gives every PV on a node an equal share of the cache volume. Choose `none` strategy only if PVs on the same node need different cache sizes, or you need to disable the limit on S3 volumes per node (via `maxVolumesPerNode: 0`).

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

We highly recommend setting `max-cache-size` on every PV that enables cache:

```yaml
spec:
  mountOptions:
    - max-cache-size 5000   # in MiB
```

### Step 2: Give each node a cache volume

The CSI Driver allows you to configure an [emptyDir](https://kubernetes.io/docs/concepts/storage/volumes/#emptydir) or a [generic ephemeral volume](https://kubernetes.io/docs/concepts/storage/ephemeral-volumes/#generic-ephemeral-volumes) as the cache volume.

Add a `cache` block, with exactly one of `emptyDir` or `ephemeral`, to `mounter`, or to `additionalDaemonSets.<name>.mounter` if using `additionalDaemonSets`, for the nodes whose S3 workloads require cache. Here are some examples, with example changes to `values.yaml` for Helm installation method. EKS Add-on users can set the same keys in the add-on's configuration values, as its JSON schema describes.

| Cache volume                                      | Use it when                                                                  | Cost                                         |
| ------------------------------------------------- | ---------------------------------------------------------------------------- | -------------------------------------------- |
| Node disk (`emptyDir`)                            | you want the simplest setup                                                  | shares the node's disk with other processes  |
| Memory (`emptyDir` with `medium: "Memory"`)       | you want the lowest latency, and the data you read repeatedly fits in memory | reserves `sizeLimit` of memory on every node |
| EBS volume (`ephemeral` with an EBS StorageClass) | you want the cache to be away from the node's root disk                      | an EBS volume for every node                 |

#### `emptyDir`

You can specify `emptyDir` in the `cache` block to use an `emptyDir` volume as the cache volume:

```yaml
mounter:
  maxVolumesPerNode: 4
  cache:
    emptyDir:
      sizeLimit: "10Gi"      # each of 4 PVs gets 10 Gi / 4 * 95% = 2432 MiB
    limitStrategy: equalSplit
```

`sizeLimit` sets the maximum size of the `emptyDir` volume. It is required with `equalSplit`, which divides it between the PVs.

On the node's disk, the kubelet enforces `sizeLimit` by evicting the Pod, and it never evicts the `system-node-critical` mounter pod, so only the cap from Step 1 bounds the cache.

> [!WARNING]
> Choose a `sizeLimit` that fits in the disk's free space and still leaves 10% of the disk free, kubelet's default [eviction threshold](https://kubernetes.io/docs/concepts/scheduling-eviction/node-pressure-eviction). Otherwise when the cache fills up, kubelet will evict workload pods on that node.

`medium` controls the storage medium for the `emptyDir` volume. If not specified, the cache uses the node's default medium (disk, SSD, or network storage). Set it to `"Memory"` to use a `tmpfs` ramdisk instead, which offers lower latency at the cost of consuming node memory.

If you plan to use `"Memory"` medium: you must raise `resources.requests.memory` by `sizeLimit`, as a tmpfs is charged to the mounter's memory. [MEMORY.md](MEMORY.md#the-equalsplit-strategy) shows the effect on each PV's memory share.

For example, for a 2Gi cache in memory:

```yaml
mounter:
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

Kubernetes deletes the `emptyDir` when the mounter Pod is removed from the node.

See [Kubernetes's documentation](https://kubernetes.io/docs/concepts/storage/volumes/#emptydir) for more details about `emptyDir`.

#### `ephemeral`

You can specify `ephemeral` in the `cache` block, alongside a StorageClass and a storage size, to use a generic ephemeral volume as the cache volume. The CSI Driver will create a PersistentVolumeClaim (PVC) template within the mounter Pod's volumes, using the configured values and [`ReadWriteOnce` access mode](https://kubernetes.io/docs/concepts/storage/persistent-volumes/#access-modes), to get a unique PVC created for each mounter Pod.

`storageClassName` names the [StorageClass](https://kubernetes.io/docs/concepts/storage/storage-classes/), and `resourceRequests` is the storage size each PVC requests from that StorageClass. Both are required.

The cache volume must let root change file ownership: the mounter makes each PV's cache directory private to that PV's Mountpoint process, and fails to start if it cannot. So NFS with `root_squash` and EFS access points are not supported; EBS and instance store volumes work.

##### Using EBS CSI Driver to provision an EBS volume dynamically

Using `ephemeral`, you can use the [Amazon Elastic Block Store (EBS) CSI driver](https://github.com/kubernetes-sigs/aws-ebs-csi-driver) to dynamically provision an EBS volume. 

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
mounter:
  cache:
    ephemeral:
      storageClassName: s3-cache-ebs-sc
      resourceRequests: 40Gi
    limitStrategy: equalSplit
```

With this configuration, once the mounter Pod is scheduled onto a node, the EBS CSI Driver will dynamically provision an EBS volume and attach it to the node for Mountpoint to use as cache. The mounter Pod starts only once the EBS volume is attached, so until then no S3 volume mounts on that node, cached or not.

The EBS volume and the mounter Pod (therefore its ephemeral PVC) will be automatically cleaned up once the mounter Pod is deleted, for example when its node is removed. We highly recommend you use `reclaimPolicy: Delete` in your StorageClass to ensure the EBS volume is cleaned up as part of this process.

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
      cache: enabled    # <-- Enable
```

| `cache` value        | Result                |
| -------------------- | --------------------- |
| unset, or `disabled` | no cache              |
| `enabled`            | uses the node's cache |
| anything else        | the PV fails to mount |

> [!WARNING]
> On a node with no configured cache volume, a PV that sets `cache: enabled` will fail to mount.

#### Legacy support

The v2 values `emptyDir` and `ephemeral`, and the v1 `cache <dir>` mount option, also turn the cache on with the node's cache volume. We recommend `cache: enabled` instead. The v2 attributes `cacheEmptyDirMedium`, `cacheEmptyDirSizeLimit`, `cacheEphemeralStorageClassName` and `cacheEphemeralStorageResourceRequest` are ignored. For a PV that still uses any of these v1 or v2 settings, s3-csi-node logs a warning when a pod mounts it.

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

You can opt in to a local cache and shared cache together if you have unused space on your instance, but also want to share the cache across multiple instances. This avoids redundant read requests from the same instance to the shared cache in S3 directory bucket when the required data is cached in local storage, reducing request cost as well as improving performance. To opt in to local and shared cache together, you can specify both the [Local Cache](#local-data-cache) and [Shared Cache](#shared-cache) in your PV:

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
      cache: enabled
```

See [Mountpoint's documentation](https://github.com/awslabs/mountpoint-s3/blob/main/doc/CONFIGURATION.md#combined-local-and-shared-cache) for more details about combined local and shared cache.


<!-- TODO: How to change cache volume configurations (point towards guide on updating config with drain / delete process) -->
<!-- TODO: How to change cache PV configurations (point towards guide on PV) -->
