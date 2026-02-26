# Troubleshooting

If the CSI Driver is not working as expected, there are some common errors to look out for and this document lists these common errors. If you still experience problems, feel free to [create a GitHub issue](https://github.com/awslabs/mountpoint-s3-csi-driver/issues/new/choose) with all the details.

Regardless of the issue, a good first step would be checking logs from the CSI Driver and Mountpoint by following the [logging guide](./LOGGING.md). Also, [troubleshooting guide of Mountpoint](https://github.com/awslabs/mountpoint-s3/blob/main/doc/TROUBLESHOOTING.md) might be useful as well.

## I'm trying to use multiple S3 volumes in the same Pod but my Pod is stuck at `ContainerCreating` status

Make sure to use unique `volumeHandle` in your `PersistentVolume`s. For example, if you use the following:

```yaml
apiVersion: v1
kind: PersistentVolume
metadata:
  name: s3-pv-1
spec:
  # ...
  csi:
    driver: s3.csi.aws.com
    volumeHandle: s3-csi-driver-volume # <-- Must be unique
    # ...
---
apiVersion: v1
kind: PersistentVolume
metadata:
  name: s3-pv-2
spec:
  # ...
  csi:
    driver: s3.csi.aws.com
    volumeHandle: s3-csi-driver-volume # <-- Must be unique
    # ...
```

Kubernetes will only process the mount procedure for one of the volumes, and the other volume will be stuck pending and therefore the Pod will be stuck at `ContainerCreating` status. You need to make sure `volumeHandle` is unique in each volume. See [multiple_buckets_one_pod.yaml](../examples/kubernetes/static_provisioning/multiple_pods_one_pv.yaml) example for a correct usage.

When encountering this issue, you may see errors similar to the one below in `kubelet` logs:

```
E1118 09:00:04.929385    4821 pod_workers.go:1301] "Error syncing pod, skipping" err="unmounted volumes=[s3-volume-02], unattached volumes=[], failed to process volumes=[s3-volume-02]: context deadline exceeded" pod="my-namespace/my-pod" podUID="c0f72deb-0acf-401f-9f29-43ec0bb9db06"
```

## I'm using `subPath` of a S3 volume and getting `No such file or directory` errors

_This issue can also be observed as Pod getting stuck at `Terminating`/`Error` status._

This can happen due to behaviour of Mountpoint that if you delete all the files in a `subPath` mount, the directory will disappear after the last file has been deleted. After that point, the `subPath` mount will become unusable, for example, given the following Pod:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: busybox
spec:
  containers:
    - name: busybox
      # ...
      volumeMounts:
        - mountPath: "/data"
          subPath: some-prefix
          name: vol
```

If you perform the following steps to delete all files in `/data`:

```bash
$ kubectl exec -it busybox -- /bin/sh
# ls data/
# echo hello > /data/hello.txt
# rm /data/hello.txt
# ls /data
ls: /data: No such file or directory
```

The `/data` will become unusable and if you try to remove this Pod it will get stuck at `Terminating`/`Error` status.

There are some possible workarounds:
- You can use `prefix` feature of Mountpoint to mount a sub path:
  ```yaml
  apiVersion: v1
  kind: PersistentVolume
  metadata:
    name: s3-pv
  spec:
    # ...
    mountOptions:
      # ...
      - prefix some-prefix/ # <-

  ```

- You can keep the `subPath` mount alive by creating a marker file, e.g. `echo keep-prefix > /data/.keep-prefix`.

There is also [a feature request on Mountpoint](https://github.com/awslabs/mountpoint-s3/issues/1055) to improve this behaviour, and if the provided workarounds wouldn't work for you, we'd recommend adding +1 (via 👍 emoji on the original post) to help us to track interest on this feature.

## I'm using an S3 Outposts bucket and am getting 'The bucket does not exist' errors

When using S3 Outposts, it is required to include the full ARN for your Outpost bucket in the `bucketName` field.
See [the S3 Outposts example](../examples/kubernetes/static_provisioning/outpost_bucket.yaml).

## I'm using experimental "Reserving headroom for Mountpoint Pods" feature and my pods are stuck in `SchedulingGated`

The [Reserving headroom for Mountpoint Pods](./HEADROOM_FOR_MPPOD.md) requires you to add [Pod Scheduling Gates](https://kubernetes.io/docs/concepts/scheduling-eviction/pod-scheduling-readiness/) to your Workload Pods to opt-in. The CSI Driver then creates necessary Headroom Pods and removes the scheduling gate - making it ready to be scheduled.

If your Workload Pods are stuck in `SchedulingGated`, that means the CSI Driver fails to create Headroom Pods or remove the scheduling gate. In this case you can check the CSI Driver's controller logs to see if it experiences any errors:

```bash
$ kubectl logs -n kube-system -l app=s3-csi-controller
```

See [logging guide](./LOGGING.md#the-controller-component-aws-s3-csi-controller) for more details.

Another thing to ensure is that you're using correct scheduling gate, the CSI Driver expects the scheduling gate to be `s3.csi.aws.com/reserve-headroom-for-mppod`, and would ignore any other scheduling gates. See [configuration guide of Reserving headroom for Mountpoint Pods](./HEADROOM_FOR_MPPOD.md#how-is-it-used) feature for more details.

## My Pod is stuck at `ContainerCreating` with error "driver name s3.csi.aws.com not found in the list of registered CSI drivers"

This error can occur due to a race condition during node startup where workload pods are scheduled before the S3 CSI driver has completed registration with kubelet.

The S3 CSI driver includes a feature to prevent this race condition by using node startup taints. When a node is tainted with `s3.csi.aws.com/agent-not-ready:NoExecute`, workload pods cannot be scheduled on that node until the S3 CSI driver removes the taint after successful startup. See the [configuration guide](./CONFIGURATION.md#configure-node-startup-taint) for more details.

For EKS managed node groups, add the taint to your node group configuration (more details in [this documentation](https://docs.aws.amazon.com/eks/latest/userguide/node-taints-managed-node-groups.html)). For self-managed nodes, [apply the taint using kubectl](https://kubernetes.io/docs/reference/kubectl/generated/kubectl_taint/) when nodes join the cluster.

## Mountpoint pods are failing with "Failed to receive mount options from /comm/mount.sock"

Mountpoint pods are scheduled immediately when a workload's pod is scheduled.
Mountpoint pods wait for the CSI Driver's node service to pass options over a socket.

However, sometimes there may be issues creating the workload pod resulting in the node service never receiving a request from `kubelet` to create volumes.
This results in the Mountpoint pod failing with a log similar to the one below.

```
I0919 20:23:04.064233       1 main.go:64] Trying to receive mount options from /comm/mount.sock
F0919 20:25:04.161506       1 main.go:45] Failed to receive mount options from /comm/mount.sock: failed to accept connection from unix socket /comm/mount.sock: accept unix /comm/mount.sock: i/o timeout
```

One well-known cause of this issue is when <a href="#im-trying-to-use-multiple-s3-volumes-in-the-same-pod-but-my-pod-is-stuck-at-containercreating-status">volumes are specified without using a unique `volumeHandle` value</a>.
There may be other issues related to the volume or pod spec that can lead to the workload pod being stuck in `ContainerCreating`, and the Mountpoint pod failing with this error.

## My workload pods are getting "Transport endpoint is not connected" errors during node drain

This error occurs when workload pods are terminated before the Mountpoint pod that provides their S3 volume mount. To prevent this, the CSI driver implements graceful eviction:

- Mountpoint pods ignore SIGTERM signals and have a 10-minute termination grace period, which ensures the S3 mount stays available during typical workload pod shutdown.
- If any workload pod takes longer than 10 minutes to terminate, it may encounter "Transport endpoint is not connected" errors when the Mountpoint pod is force-killed after its grace period expires.

**Recommendation:** Keep workload pod termination grace periods sufficiently low to ensure graceful shutdown completes within the Mountpoint pod's 10-minute window and the grace period specified in the `drain` operation (e.g. via Karpenter NodePool [settings](https://karpenter.sh/docs/concepts/nodepools/#spectemplatespecterminationgraceperiod)).

## Cluster Autoscaler blocked by Mountpoint pods (prior to v2.5.0)

Prior to version v2.5.0, Cluster Autoscaler may stop scaling nodes down due to `mount-s3/mp-*` pods blocking node removal.

**Behavior:** Nodes with MP pods cannot be scaled down, showing logs like:
```
I1216 20:28:55.343278       1 cluster.go:169] Node ip-10-112-14-230.ec2.internal cannot be removed: mount-s3/mp-4c5r7 is not replicated
```

**Recommendation:** Upgrade to Mountpoint S3 CSI driver v2.5.0 or later. The fix adds `cluster-autoscaler.kubernetes.io/daemonset-pod: "true"` annotation to MP pods, allowing the autoscaler to treat them as DaemonSet-managed pods instead of standalone pods that block scale-down.

**Limitation:** Note that this may cause over-provisioning based on the templateNodeInfo mechanism. Overestimation of required pod slots is capped by the distinct S3 PV count (or the number of S3 pods on any running node), see: [podsExpectedOnFreshNode](https://github.com/kubernetes/autoscaler/blob/62b135abda1b345d3faca699d1965729ad3566c8/cluster-autoscaler/simulator/node_info_utils.go#L177) and [how-does-scale-up-work](https://github.com/kubernetes/autoscaler/blob/62b135abda1b345d3faca699d1965729ad3566c8/cluster-autoscaler/FAQ.md#how-does-scale-up-work).
