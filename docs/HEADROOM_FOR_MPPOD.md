# [Experimental] Reserving headroom for Mountpoint Pods

_This is an experimental feature and the API or behavior may change in subsequent MINOR [releases](https://github.com/awslabs/mountpoint-s3-csi-driver?tab=readme-ov-file#releases)_.

This feature causes overprovisioning in [node autoscalers](https://kubernetes.io/docs/concepts/cluster-administration/node-autoscaling/) like [Karpenter](https://karpenter.sh/) with the goal of reserving some headroom for _dynamically_ spawned Mountpoint Pods _after_ node creation.

## Why is it needed?

The CSI Driver v2 spawns Mountpoint Pods into the same nodes as the workloads to provide volumes. The CSI Driver spawns these Mountpoint Pods _after_ the workloads are assigned to nodes. This approach is taken because we might be able to [share an existing Mountpoint Pod](./MOUNTPOINT_POD_SHARING.md) on a specific node, eliminating the need to spawn a new one. Therefore, the CSI Driver needs to wait until a workload is scheduled to a specific node before deciding whether to share an existing Mountpoint Pod or spawn a new one.

This dynamic spawning of Mountpoint Pods on specific nodes creates a problem with node autoscalers like Karpenter. Since autoscalers are not aware of Mountpoint Pod requirements when making scaling decisions, they may make suboptimal choices. As a result, there may not be enough space for a Mountpoint Pod on the specific node where the workload is deployed.

This feature aims to mitigate this issue by reserving headroom for upcoming Mountpoint Pods, ensuring autoscalers create instances large enough to host Mountpoint Pods alongside workloads.

## How does it work?

When the CSI Driver detects a Workload Pod using the scheduling gate to enable this feature, it performs the following steps:

  1. Labels the Workload Pod to use inter-pod affinity rules in the Headroom Pods
  2. Creates Headroom Pods using a pause container with inter-pod affinity rule to the Workload Pod - since node autoscalers like [Karpenter supports inter-pod affinity rules](https://karpenter.sh/docs/concepts/scheduling/#pod-affinityanti-affinity), this should help them to choose a right instance type
  3. Ungates the scheduling gate from the Workload Pod to let it scheduled - alongside the Headroom Pods if possible
  4. Schedules Mountpoint Pod if necessary (i.e., the CSI Driver cannot share an existing Mountpoint Pod) into the same node as the Workload and Headroom Pods using a preempting priority class
  5. Mountpoint Pod most likely preempts the Headroom Pods if there is no space in the node - as the Headroom Pods uses a negative priority -, or just gets scheduled if there is enough space for all pods
  6. Deletes the Headroom Pods as soon as the Workload Pod is running or terminated - as Mountpoint Pods are already scheduled or no longer needed

## What are the limitations of this feature?

### Overprovisioning

This feature may cause overprovisioning (i.e., autoscalers may create larger instance types than needed to host pending workloads), which could result in allocating more resources than the cluster requires. This occurs because the CSI Driver allocates one Headroom Pod per volume, but in some cases that capacity may not be needed due to the Mountpoint Pod sharing feature. This might also cause Workload Pods might deploy more sparsely - reducing the utilization of Mountpoint Pod sharing feature overall.

Features like [Karpenter's consolidation](https://karpenter.sh/docs/concepts/disruption/#consolidation) mechanisms may help the cluster settle on more appropriately sized instance types after all pods are scheduled, but this would still consume additional resources and time since autoscalers were not aware of the exact number of workloads in advance.

### Downsides of inter-pod affinity

As noted in [Kubernetes's documentation](https://kubernetes.io/docs/concepts/scheduling-eviction/assign-pod-node/#inter-pod-affinity-and-anti-affinity), inter-pod affinity rules require substantial amounts of processing which can slow down scheduling in large clusters significantly, and it's not recommended in clusters larger than several hundred nodes.

Additionally, inter-pod affinity rules are insufficient to capture the intent of "all-or-nothing" scheduling (also known as [co-scheduling](https://github.com/kubernetes-sigs/scheduler-plugins/blob/master/pkg/coscheduling/README.md), [gang scheduling](https://yunikorn.apache.org/docs/user_guide/gang_scheduling/), or group scheduling) requirements. Therefore, the Kubernetes Scheduler may still schedule a Workload Pod without considering its associated Headroom Pod.

### Preempting incorrect pods

Even though the CSI Driver spawns Headroom Pods with a negative priority, there is still a chance that Mountpoint Pods might evict some other pods in case if Headroom Pods got evicted by some other high priority pod and there is no Headroom Pod for Mountpoint Pod to evict.

## How is it used?

Opting into this feature requires two steps.

### 1. Enable this feature during Helm chart installation (one-time setup)

```bash
$ helm upgrade --install aws-mountpoint-s3-csi-driver \
    --set experimental.reserveHeadroomForMountpointPods=true \
    ...
    aws-mountpoint-s3-csi-driver/aws-mountpoint-s3-csi-driver
```

This is behind a feature flag because it adds cluster-wide `patch` permissions on pods to the CSI Driver Controller's Service Account. This permission is needed for:
  1. Adding labels to Workload Pods for use in inter-pod affinity rules in Headroom Pods
  2. Removing scheduling gates from Workload Pods after creating Headroom Pods to make the Workload Pod ready for scheduling

### 2. Add a scheduling gate for each Workload Pod where reserving headroom for Mountpoint Pods is desired

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: workload
spec:
  schedulingGates:
    - name: s3.csi.aws.com/reserve-headroom-for-mppod # <-- HERE
  containers:
    # ...
  volumes:
    - name: vol
      persistentVolumeClaim:
        claimName: s3-pvc
```

This is currently done manually, and we may move this logic into a mutating admission webhook in the future to make configuration easier.
