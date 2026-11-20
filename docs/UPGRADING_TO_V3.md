# Upgrading Mountpoint for Amazon S3 CSI Driver to v3

## Upgrading PVs that use the cache

### What changes for a PV that caches

In v2, each mount ran in its own Mountpoint Pod, with a cache volume the PV described. In v3, every Mountpoint process on a node runs in one mounter Pod, `s3-csi-mounter`, so the node has [one cache volume](CACHING.md#local-data-cache), set in your Helm values or EKS add-on configuration.

PVs now only need to opt in to use the cache volume. See the comparison below for cache `volumeAttributes`:

```yaml
# v2 PV, emptyDir
spec:
  csi:
    volumeAttributes:
      cache: emptyDir
      cacheEmptyDirMedium: ""
      cacheEmptyDirSizeLimit: 1Gi
---
# v2 PV, ephemeral
spec:
  csi:
    volumeAttributes:
      cache: ephemeral
      cacheEphemeralStorageClassName: gp2
      cacheEphemeralStorageResourceRequest: 1Gi
---
# v3 PV
spec:
  csi:
    volumeAttributes:
      cache: enabled
```

v3 treats `cache: emptyDir` and `cache: ephemeral` as `cache: enabled`. The PV caches in its node's cache volume, with the node's type, size and `cache.limitStrategy`, even if the PV asked for another type or StorageClass, such as memory, local NVMe or an encrypted EBS class. v3 ignores the four `cacheEmptyDir*` and `cacheEphemeral*` attributes, so `cacheEmptyDirSizeLimit` and `cacheEphemeralStorageResourceRequest` no longer cap the cache.

v3 also accepts the v1 `cache <dir>` mount option, and ignores its path.

### Before you upgrade the driver

#### Find the PVs with v1 or v2 cache settings

This command needs `jq`. If it prints nothing, no PV caches, and you can skip the rest of this section.

```bash
kubectl get pv -o json | jq -c '.items[] | select(.spec.csi.driver == "s3.csi.aws.com")
  | {pv: .metadata.name,
     volumeAttributes: (.spec.csi.volumeAttributes // {} | with_entries(select(.key | startswith("cache")))),
     mountOptions: [.spec.mountOptions[]? | select(test("^ *(--)?(cache|max-cache-size)([ =]|$)"))]}
  | select(.volumeAttributes != {} or .mountOptions != [])'
```

A PV's v2 size is its `cacheEmptyDirSizeLimit` or `cacheEphemeralStorageResourceRequest`.

#### Choose a `cache.limitStrategy`

Choose how PVs share the node's cache volume, with Step 1 of [CACHING.md](CACHING.md#step-1-choose-how-pvs-share-the-cache-volume). If PVs on the same node need different cache sizes, which their v2 sizes can show, choose `none` strategy; otherwise we recommend `equalSplit` strategy.

If you choose `none` strategy, add `max-cache-size` (in MiB) to every PV that caches and does not set it yet. A PV without it has no cache upper bounds on the v3 driver, and on a node-disk cache can fill the node's disk until the kubelet evicts workload pods. Start from 95% of the PV's v2 size: for `2Gi`, `max-cache-size 1945`. While a PV uses `cache: emptyDir`, keep its `max-cache-size` at or below its `cacheEmptyDirSizeLimit`, or else v2 driver would fail every mount of the PV.

You can add the option to a live PV on v2, for example with `kubectl edit pv <name>`.

### Upgrade the driver

Upgrade the driver to v3 with a cache volume, and the `cache.limitStrategy` you chose, in your Helm values or EKS add-on configuration. On v3, a PV that caches fails to mount on a node without a cache volume. Start from the `mounter` examples in [CACHING.md](CACHING.md#step-2-give-each-node-a-cache-volume), and size the volume depending on your strategy. The largest v2 size used is a good starting point for the cache volume size. On a node-disk cache, remember to leave room for the v1 or v2 caches of pods that are still running.

Running pods keep their v1 or v2 mount and cache until they are recreated.

<!-- TODO: link to the general v3 upgrade steps once they exist: check that no node matches more than one `additionalDaemonSets` pair, including the default pair; check that each mounter DaemonSet is ready (if one is not, fix its values, upgrade again, and delete its Pods that are not ready, since it uses OnDelete); then wait for the s3-csi-node rollout. -->

### After you upgrade the driver

Once you no longer plan to downgrade to v2, we recommend switching each PV to `cache: enabled`, so the PV no longer shows a cache type and size that v3 ignores. Kubernetes does not allow editing a PV's `volumeAttributes`, so switching a PV means stopping every pod that uses the PV, and recreating it and its PVC with `cache: enabled` in place of its v1 or v2 cache settings.

<!-- TODO: link to the guide on changing a PV's volumeAttributes, with the steps (recreate in place, or move the workload to a new PV and PVC), once it exists. -->

### Downgrading to v2 or v1

v2 rejects `cache: enabled` and `cache: disabled`, so before you downgrade to v2, recreate every PV that sets either value, and its PVC, while every pod that uses them is stopped. v1 ignores both values, so on v1 such a PV mounts without a cache. To keep a PV caching, give it v2 cache attributes for v2, or the `cache <dir>` mount option for v1, which needs that directory on each node.

<!-- TODO: link to the general v3 downgrade steps once they exist: stop every workload that uses an S3 volume, since the downgrade removes the mounter Pods that serve every v3 mount; downgrade the driver and wait for the s3-csi-node rollout; then start the workloads again. -->
