# Caching Configuration of Mountpoint for Amazon S3 CSI Driver

Mountpoint supports caching file system metadata and object content to reduce cost and improve performance for repeated reads to the same file. The CSI Driver allows you to configure caching of Mountpoint in your PersistentVolume (PV) definition. See [Mountpoint's caching configuration]((https://github.com/awslabs/mountpoint-s3/blob/main/doc/CONFIGURATION.md#caching-configuration)) for more details about caching.

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

## Data Cache

Mountpoint supports different types of data caching that you can opt in to accelerate repeated read requests.

### Local Cache

The CSI Driver allows you to configure an [emptyDir](https://kubernetes.io/docs/concepts/storage/volumes/#emptydir) or a [generic ephemeral volume](https://kubernetes.io/docs/concepts/storage/ephemeral-volumes/#generic-ephemeral-volumes) as a local cache.
The CSI Drivers mounts the provided cache volume to the Mountpoint Pod, and configures Mountpoint to use that volume as local cache.

See [Mountpoint's documentation](https://github.com/awslabs/mountpoint-s3/blob/main/doc/CONFIGURATION.md#local-cache) for more details about local cache.

#### `emptyDir`

You can specify `emptyDir` as cache type in your PV to use an `emptyDir` volume as local cache:

```yaml
apiVersion: v1
kind: PersistentVolume
metadata:
  name: s3-pv
spec:
  # ...
  csi:
    driver: s3.csi.aws.com
    # ...
    volumeAttributes:
      bucketName: amzn-s3-demo-bucket
      cache: emptyDir
      cacheEmptyDirSizeLimit: 2Gi # optional but highly recommended!
      cacheEmptyDirMedium: Memory # optional
```

Both `cacheEmptyDirSizeLimit` and `cacheEmptyDirMedium` are optional, but we highly recommended you to specify a size limit on your cache, it might use all your node's storage otherwise depending on the cluster's configuration. If `cacheEmptyDirMedium` is not specified, the default storage medium will be used.

The `emptyDir` will be unique to the each Mountpoint Pod and won't be shared between other Mountpoint instances.

See [Kubernetes's documentation](https://kubernetes.io/docs/concepts/storage/volumes/#emptydir) for more details about `emptyDir`.

#### `ephemeral`

You can specify `ephemeral` as cache type alongide a StorageClass and storage size in your PV to use an generic ephemeral volume as local cache:

```yaml
apiVersion: v1
kind: PersistentVolume
metadata:
  name: s3-pv
spec:
  # ...
  csi:
    driver: s3.csi.aws.com
    # ...
    volumeAttributes:
      bucketName: amzn-s3-demo-bucket
      cache: ephemeral
      cacheEphemeralStorageClassName: nvme-ssd # required
      cacheEphemeralStorageResourceRequest: 4Gi # required
```

The CSI Driver will create a PersistentVolumeClaim (PVC) template within the Mountpoint Pod's volumes using the configured values and [`ReadWriteOnce` access mode](https://kubernetes.io/docs/concepts/storage/persistent-volumes/#access-modes) to get a unique PVC created for the Mountpoint Pod.
Both `cacheEphemeralStorageClassName` and `cacheEphemeralStorageResourceRequest` are required to specify a StorageClass name, and a storage size to request from the StorageClass respectively.

Using `ephemeral` cache type, you can use [Amazon Elastic Block Store (EBS) CSI driver](https://github.com/kubernetes-sigs/aws-ebs-csi-driver) to dynamically provision an EBS volume or use [Local Volume Static Provisioner](https://github.com/kubernetes-sigs/sig-storage-local-static-provisioner) to access your [Amazon EC2 Instance Store](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/InstanceStorage.html). See examples below for more details.

##### Using EBS CSI Driver to provision an EBS volume dynamically

First, make sure to install EBS CSI Driver in your cluster by following their [installation guide](https://github.com/kubernetes-sigs/aws-ebs-csi-driver/blob/master/docs/install.md).

You can then create a StorageClass using EBS CSI Driver for Mountpoint CSI Driver to request a volume to use as local cache:

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: s3-cache-ebs-sc
provisioner: ebs.csi.aws.com
reclaimPolicy: Delete
volumeBindingMode: WaitForFirstConsumer
parameters: # all optional, see https://github.com/kubernetes-sigs/aws-ebs-csi-driver/blob/master/docs/parameters.md for more details
  type: io2
  iopsPerGB: "256000"
  blockExpress: "true"
```

You can then reference this StorageClass from your S3 PV:

```yaml
apiVersion: v1
kind: PersistentVolume
metadata:
  name: s3-pv
spec:
  # ...
  csi:
    driver: s3.csi.aws.com
    # ...
    volumeAttributes:
      bucketName: amzn-s3-demo-bucket
      cache: ephemeral
      cacheEphemeralStorageClassName: s3-cache-ebs-sc
      cacheEphemeralStorageResourceRequest: 10Gi
```

With this configuration, once your workload is scheduled into a node, Mountpoint CSI Driver will schedule a Mountpoint Pod to the same node with the `ephemeral` volume. EBS CSI Driver will then dynamically provision an EBS volume and attach it to the node for Mountpoint to use as cache.

The EBS volume and the Mountpoint Pod (therefore it's ephemeral PVC) will automatically cleaned up once the workload is terminated. We highly recommend you to use `reclaimPolicy: Delete` in your StorageClass to ensure the cache PV is automatically cleaned up as part of this process.

##### Using Local Volume Static Provisioner to use local NVMe

Some Amazon EC2 instances offer non-volatile memory express (NVMe) solid state drives (SSD) instance store volumes. You can utilise [Local Volume Static Provisioner](https://github.com/kubernetes-sigs/sig-storage-local-static-provisioner) to use instance store as cache. See [Instance store volume limits for EC2 instances](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/instance-store-volumes.html) for more details about instance store support on EC2 instances.

The Local Volume Static Provisioner allows you to configure some options, you can find more details in their [Getting started guide](https://github.com/kubernetes-sigs/sig-storage-local-static-provisioner/blob/master/docs/getting-started.md).

As an example, you can configure your [eksctl](https://eksctl.io/) configuration to mount available NVMe instance storage disks at `/dev/disk/kubernetes`:

```yaml
apiVersion: eksctl.io/v1alpha5
kind: ClusterConfig
metadata:
  name: cluster-with-storage
  region: eu-central-1
managedNodeGroups:
  - name: storage-nvme
    desiredCapacity: 1
    instanceType: i3.8xlarge
    amiFamily: AmazonLinux2023
    preBootstrapCommands:
      - |
          cat <<EOF > /etc/udev/rules.d/90-kubernetes-discovery.rules
          # Discover Instance Storage disks so kubernetes local provisioner can pick them up from /dev/disk/kubernetes
          KERNEL=="nvme[0-9]*n[0-9]*", ENV{DEVTYPE}=="disk", ATTRS{model}=="Amazon EC2 NVMe Instance Storage", ATTRS{serial}=="?*", SYMLINK+="disk/kubernetes/nvme-\\\$attr{model}_\\\$attr{serial}", OPTIONS="string_escape=replace"
          EOF
      - udevadm control --reload && udevadm trigger
```

The `i3.8xlarge` instance type provides four NVMe instance storage disks. After applying your changes using `eksctl`, you can install the example EKS NVMe manifest:
```bash
$ kubectl apply -f https://raw.githubusercontent.com/kubernetes-sigs/sig-storage-local-static-provisioner/refs/heads/master/helm/generated_examples/eks-nvme-ssd.yaml
```

This should create a StorageClass named `nvme-ssd`, and four PVs for each local NVMe instance storage disk attached to the instance:

```bash
$ kubectl get sc nvme-ssd
NAME       PROVISIONER                    RECLAIMPOLICY   VOLUMEBINDINGMODE      ALLOWVOLUMEEXPANSION   AGE
nvme-ssd   kubernetes.io/no-provisioner   Delete          WaitForFirstConsumer   false                  5m1s

$ kubectl get pv
NAME                CAPACITY   ACCESS MODES   RECLAIM POLICY   STATUS      CLAIM   STORAGECLASS   VOLUMEATTRIBUTESCLASS   REASON   AGE
local-pv-575f3c43   1769Gi     RWO            Delete           Available           nvme-ssd       <unset>                          27s
local-pv-b0013057   1769Gi     RWO            Delete           Available           nvme-ssd       <unset>                          17s
local-pv-bfea2335   1769Gi     RWO            Delete           Available           nvme-ssd       <unset>                          17s
local-pv-d63df84    1769Gi     RWO            Delete           Available           nvme-ssd       <unset>                          17s
```

You can now specify StorageClass `nvme-ssd` in your PV's configuration with `ephemeral` cache type:

```yaml
apiVersion: v1
kind: PersistentVolume
metadata:
  name: s3-pv
spec:
  # ...
  csi:
    driver: s3.csi.aws.com
    # ...
    volumeAttributes:
      bucketName: amzn-s3-demo-bucket
      cache: ephemeral
      cacheEphemeralStorageClassName: nvme-ssd
      cacheEphemeralStorageResourceRequest: 10Gi
```

One thing to note is that, since the local NVMe instance storage disks are local to the nodes,
you need to ensure your workload and therefore the Mountpoint Pod is scheduled into a node with local NVMe and associated PV available.
You can use [`nodeSelector`](https://kubernetes.io/docs/concepts/scheduling-eviction/assign-pod-node/#nodeselector) or [Node affinity](https://kubernetes.io/docs/concepts/scheduling-eviction/assign-pod-node/#affinity-and-anti-affinity) rules to achieve that.

For example, this configuration would ensure that your workload is scheduled a node from `eksctl` node group `storage-nvme`:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: workload
spec:
  containers:
    # ...
  volumes:
    - name: vol
      persistentVolumeClaim:
        claimName: s3-pvc
  affinity:
    nodeAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        nodeSelectorTerms:
          - matchExpressions:
              - key: alpha.eksctl.io/nodegroup-name
                operator: In
                values:
                  - storage-nvme
          # OR using node name
          # - matchExpressions:
          #     - key: kubernetes.io/hostname
          #       operator: In
          #       values:
          #         - ip-192-0-2-0.region-code.compute.internal
```

After deploying your workload, the Mountpoint Pod should also be deployed to the same node automatically with an local NVMe PV attached to it:
```bash
$ kubectl describe po -n mount-s3
Name:                 mp-ql5rd
Namespace:            mount-s3
...
Volumes:
  ...
  local-cache:
    Type:          EphemeralVolume (an inline specification for a volume that gets created and deleted with the pod)
    StorageClass:  nvme-ssd
    Volume:
    Labels:            s3.csi.aws.com/type=local-ephemeral-cache
    Annotations:       <none>
    Capacity:
    Access Modes:
    VolumeMode:    Filesystem

$ kubectl describe pvc -n mount-s3
Name:          mp-xt6c4-local-cache
Namespace:     mount-s3
StorageClass:  nvme-ssd
Status:        Bound
Volume:        local-pv-bfea2335
Labels:        s3.csi.aws.com/type=local-ephemeral-cache
Annotations:   pv.kubernetes.io/bind-completed: yes
               pv.kubernetes.io/bound-by-controller: yes
Finalizers:    [kubernetes.io/pvc-protection]
Capacity:      1769Gi
Access Modes:  RWO
VolumeMode:    Filesystem
Used By:       mp-xt6c4
```

Note that if there is no local NVMe available in the scheduled node, Mountpoint Pod would fail to schedule and your workload would hang in `Pending` state.
You must ensure your workload (and therefore the Mountpoint Pod) is scheduled to a node with local NVMe available to use.

Ensure to check [other configurations of Local Volume Static Provisioner](https://github.com/kubernetes-sigs/sig-storage-local-static-provisioner/tree/master?tab=readme-ov-file#user-guide) including
[Local Volume Node Cleanup Controller](https://github.com/kubernetes-sigs/sig-storage-local-static-provisioner/blob/master/docs/node-cleanup-controller.md) for volume clean up and other details.

#### (Deprecated) `cache` flag via `mountOptions`

With the CSI Driver v1, the Mountpoint instances were spawned on the host using `systemd`, and the `cache` flag in `mountOptions` was a relative path to the host. The cache folder also needed to exists for Mountpoint to use. We deprecated this usage, and will fallback to using [`emptyDir`](#emptyDir) with the default storage medium without any limit by default.

You no longer needed to create a cache folder on the host, and the configured path will be ignored by the CSI Driver v2! We recommend customers to migrate to [`emptyDir`](#emptyDir) and specify a limit.

This deprecated use of cache:

```yaml
apiVersion: v1
kind: PersistentVolume
metadata:
  name: s3-pv
spec:
  mountOptions:
    - cache /cache/folder/on/host
  csi:
    driver: s3.csi.aws.com
    # ...
    volumeAttributes:
      bucketName: amzn-s3-demo-bucket
```

will translate the following automatically by the CSI Driver v2:

```yaml
apiVersion: v1
kind: PersistentVolume
metadata:
  name: s3-pv
spec:
  # ...
  csi:
    driver: s3.csi.aws.com
    # ...
    volumeAttributes:
      bucketName: amzn-s3-demo-bucket
      cache: emptyDir
```

### Shared Cache

When mounting an S3 bucket, you can opt in to a shared cache in [Amazon S3 Express One Zone](https://aws.amazon.com/s3/storage-classes/express-one-zone/). You should use the shared cache if you repeatedly read small objects (up to 1 MB) from multiple compute instances, or the size of the dataset that you repeatedly read often exceeds the size of your local cache. This improves latency when reading the same data repeatedly from multiple instances by avoiding redundant requests to your mounted S3 bucket. To enable shared cache, specify `cache-xz` flag in `mountOptions` with your directory bucket name:

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
