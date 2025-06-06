# Mountpoint Pod Sharing feature of Mountpoint for Amazon S3 CSI Driver

Mountpoint Pod Sharing improves resource utilization by allowing multiple workloads to share a single [Mountpoint for Amazon S3](https://github.com/awslabs/mountpoint-s3) instance when appropriate.

### How Mountpoint Pod Sharing Works

With Mountpoint Pod Sharing, the CSI Driver can reuse existing Mountpoint instances between multiple workloads instead of creating one instance per workload. This reduces the overall number of Mountpoint instances in your cluster, leading to better resource utilization.

```mermaid
graph LR;
    MP[Mountpoint Pod]

    P1[Workload Pod 1]
    P2[Workload Pod 2]
    P3[Workload Pod 3]

    P1 --> MP
    P2 --> MP
    P3 --> MP
```

The CSI Driver only shares Mountpoint instances between multiple workloads if all of the following conditions are met:

- Workloads are scheduled on the same node
- Workloads use the same volume (same PV name and volume ID)
- Workloads use the same mount options
- Workloads use the same authentication source (`driver` or `pod`)
- Workloads have the same FSGroup from Pod Security Context (if specified)
- For pod-level identity, workloads must also have:
  - The same namespace
  - The same service account name
  - The same IAM role ARN (from service account annotation)

### How Mountpoint Pod Sharing is Implemented

Mountpoint Pod Sharing is implemented using a Custom Resource Definition (CRD) called `MountpointS3PodAttachment`. This CRD stores the mapping between Mountpoint Pods and Workload Pods and serves as the source-of-truth for which workloads are assigned to which Mountpoint Pods.

The CSI Driver Controller component manages these CRDs based on workload pod events:
- When a new workload pod is scheduled, the controller checks if there's an existing Mountpoint Pod that can be shared
- If a suitable Mountpoint Pod exists, the workload is added to its attachments list
- If no suitable Mountpoint Pod exists, a new one is created
- When a workload pod is terminated, it's removed from the attachments list
- When a Mountpoint Pod has no more attached workloads, it's marked for unmounting and termination

The CSI Driver Node component reads these CRDs to determine the correct Mountpoint Pod to use during mount operations and creates bind mounts from the Mountpoint Pod to each workload pod.
