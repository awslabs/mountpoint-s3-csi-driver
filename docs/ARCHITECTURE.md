# Architecture of Mountpoint for Amazon S3 CSI Driver

The Mountpoint for Amazon S3 CSI Driver conforms to the [v1.9.0 version of Container Storage Interface (CSI)](https://github.com/container-storage-interface/spec/blob/v1.9.0/spec.md). The CSI Driver uses [Mountpoint for Amazon S3](https://github.com/awslabs/mountpoint-s3) under the hood to present an Amazon S3 bucket as a storage volume accessible by containers in your Kubernetes cluster.

The CSI Driver consists of three components deployed to your cluster.

## The Controller Component (`aws-s3-csi-controller`)

This component is deployed as a Pod in the cluster using a Deployment. It uses [controller-runtime](https://github.com/kubernetes-sigs/controller-runtime) to implement a Kubernetes controller. It’s responsible for watching newly scheduled workload Pods that use a volume backed by the CSI Driver and scheduling Mountpoint Pods to provide those volumes on the same node.

This component manages a [Custom Resource Definition (CRD)](https://kubernetes.io/docs/concepts/extend-kubernetes/api-extension/custom-resources/) called `MountpointS3PodAttachment`. The source-of-truth of which workload assigned to which Mountpoint Pod is stored using instances of this CRD. This component responsible for assignments and deassignments. This component decides when a Mountpoint Pod is no longer needed and marks it as such, notifying the Node Component to perform a clean exit.

This is what happens when there is a new workload using a volume backed by the CSI Driver scheduled to the cluster:

```mermaid
sequenceDiagram
    actor user as User
    participant apiSrv as Control Plane<br/>api-server
    participant scheduler as Control Plane<br/>kube-scheduler
    participant reconciler as Controller Node<br/>aws-s3-csi-controller<br/>Reconciler
    participant kubelet as Workload Node<br/>kubelet
    participant csiNode as Workload Node<br/>aws-s3-csi-driver<br/>CSI Node Service

    reconciler->>apiSrv: Watch for pod changes
    Note over reconciler: aws-s3-csi-controller Reconciler watches for pod events

    user->>apiSrv: kubectl create -f pod-with-s3-volume.yaml
    apiSrv->>reconciler: Notify about new pod

    Note over reconciler: Reconciler ignores the event as the pod is not scheduled yet

    scheduler->>apiSrv: Assign Workload Pod to Workload Node

    apiSrv->>kubelet: Run Workload Pod once its ready

    loop For each S3 volume
      kubelet->>csiNode: NodePublishVolume RPC call
      csiNode->>apiSrv: Wait for MountpointS3PodAttachment to be created, and Mountpoint Pod to be running
      Note over kubelet: kubelet waits for S3 volume to be ready to run Workload Pod
    end

    Note over apiSrv: Workload Pod has been assigned to a node
    apiSrv->>reconciler: Notify about pod update

    reconciler->>reconciler: Start reconciliation process as workload is scheduled and using an S3 volume

    loop For each S3 volume
        reconciler->>apiSrv: Check for existing MountpointS3PodAttachment on the same node with the same volume
        apiSrv-->>reconciler: Return existing MountpointS3PodAttachment attachment if any

        alt If compatible Mountpoint Pod found on the same node
            reconciler->>apiSrv: Assign workload to existing Mountpoint Pod
        else If no compatible Mountpoint Pod found on the same node
            reconciler->>apiSrv: Create new MountpointS3PodAttachment
            reconciler->>apiSrv: Create new Mountpoint Pod on the same node
            Note over reconciler: New Mountpoint Pod spawned
        end

        Note over reconciler: Workload has been assigned to a Mountpoint Pod and MountpointS3PodAttachment has been created/updated
    end

    loop For each S3 volume
      apiSrv->>csiNode: Notify about MountpointS3PodAttachment/Mountpoint Pod update
      csiNode->>csiNode: Mount volume on Mountpoint Pod
      Note over csiNode: Mountpoint Pod mounted
      csiNode->>kubelet: Return a success response to NodePublishVolume RPC call
      kubelet->>apiSrv: Mark Workload Pod as Running
    end
```

## The Node Component (`aws-s3-csi-driver`)

This component is deployed as a Pod to each node in the cluster using a DaemonSet. It implements [CSI Node Service RPC](https://github.com/container-storage-interface/spec/blob/master/spec.md#node-service-rpc) and registers itself with the kubelet running in that node using [sidecars provided by Kubernetes CSI project](https://kubernetes-csi.github.io/docs/sidecar-containers.html). This component implements two important RPCs from the CSI:

* `NodePublishVolume` – Called by kubelet whenever there is a Pod running in that node that uses a volume provided by the CSI Driver. In this method we coordinate Mountpoint Pod running in the same node for that volume and perform the mount operation to spawn a Mountpoint instance. This method also provides AWS credentials for Mountpoint instances. In the subsequent calls to this function, if the mount operation already performed for that volume, this function just updates previously created Service Account Tokens (for IRSA and EKS Pod Identity) to ensure Mountpoint instance has up-to-date tokens and can exchange them for temporary AWS credentials. To support [sharing Mountpoint Pods](#MOUNTPOINT_POD_SHARING.md), we create `bind` mounts to target Mountpoint Pods from each workload in this method after ensuring Mountpoint Pod is successfully mounted.

* `NodeUnpublishVolume` – Called by kubelet whenever the Pod using the volume is descheduled and the volume is no longer needed. This method unmounts `bind` mount created for each workload.

This component also registers itself with updates from the control plane to detect when a Mountpoint Pod is no longer needed. It performs unmount operation for Mountpoint to cleanly exit, and then it cleans all credential/token files to ensure there isn't any secret left.

This is what happens when kubelet calls `NodePublishVolume` on the node component:

```mermaid
sequenceDiagram
    participant apiSrv as Control Plane<br/>api-server
    participant kubelet as Workload Node<br/>kubelet
    participant csiNode as Workload Node<br/>aws-s3-csi-driver<br/>CSI Node Service
    participant mpPod as Workload Node<br/>Mountpoint Pod
    participant kernel as Linux Kernel

    Note over kubelet: Mountpoint Pod is already running
    Note over kubelet: Workload Pod is still pending

    kubelet->>csiNode: NodePublishVolume RPC call
    Note over csiNode: Extract volume information and target path

    csiNode->>csiNode: Check if target path is already mounted

    alt If target is already mounted (just update credentials)
        csiNode->>csiNode: Find Mountpoint Pod and get it's credential path

        csiNode->>csiNode: Write up-to-date service account tokens to Mountpoint Pod's credential path
        Note over csiNode: Mountpoint Pod has up-to-date service account tokens for EKS Pod Identity and IRSA

        csiNode-->>kubelet: Return a success response to NodePublishVolume RPC call
    else Target not mounted (perform full mount)
        csiNode->>apiSrv: Get MountpointS3PodAttachment for volume
        apiSrv-->>csiNode: Return MountpointS3PodAttachment with Mountpoint Pod name

        csiNode->>apiSrv: Wait for Mountpoint Pod to be ready
        apiSrv-->>csiNode: Mountpoint Pod is running

        Note over csiNode: Lock Mountpoint Pod to prevent concurrent operations

        csiNode->>csiNode: Check if source mount point exists
        Note over csiNode: Source path: /var/lib/kubelet/plugins/s3.csi.aws.com/mounts/{pod-name}

        csiNode->>csiNode: Write up-to-date service account tokens to Mountpoint Pod's credential path

        alt If source is not already mounted
            csiNode->>kernel: Open FUSE device (/dev/fuse)
            kernel-->>csiNode: Return FUSE file descriptor

            csiNode->>kernel: Mount syscall on source path with the obtained FUSE file descriptor

            csiNode->>mpPod: Send mount options and FUSE file descriptor via Unix socket

            mpPod->>mpPod: Spawn Mountpoint process using the provided mount options and FUSE file descriptor

            csiNode->>csiNode: Poll for mount success/failure on the source path
            kernel-->>csiNode: Source path has been mounted
            csiNode->>csiNode: Close FUSE file descriptor

            Note over csiNode: New Mountpoint process has been created and the source path has been mounted
        else Source already mounted
            Note over csiNode: No-op only the credentials refreshed
        end

        csiNode->>kernel: Bind mount from the source path to the target path
        Note over csiNode: Target path: /var/lib/kubelet/pods/{pod-uuid}/volumes/kubernetes.io~csi/{volume-id}/mount
        kernel-->>csiNode: Bind mount created
        Note over csiNode: Target path has been mounted

        Note over csiNode: Unlock Mountpoint Pod

        csiNode-->>kubelet: Return a success response to NodePublishVolume RPC call
    end

    kubelet->>apiSrv: Mark workload Pod as Running
```

## The Mounter Component / Mountpoint Pod (`aws-s3-csi-mounter`)

This component is deployed to cluster as Mountpoint Pods. It’s spawned by the controller component and responsible for receiving mount options from the node component and spawning Mountpoint instances inside the Pod and monitoring them. Mountpoint Pods runs without any privilege and also as a non-root user.

This is what happens inside a Mountpoint Pod when it starts running:

```mermaid
sequenceDiagram
    participant csiNode as Workload Node<br/>aws-s3-csi-driver<br/>CSI Node Service
    participant mounter as Workload Node<br/>aws-s3-csi-mounter<br/>Mountpoint Pod

    Note over mounter: Mountpoint Pod starts

    mounter->>csiNode: Receive mount options via Unix socket
    csiNode->>mounter: Send mount options and FUSE file descriptor via Unix socket

    mounter->>mounter: Parse mount options and validate

    Note over mounter: Start Mountpoint process in foreground with the received mount options and the FUSE file descriptor

    loop Run Mountpoint until termination
        mounter->>mounter: Wait for Mountpoint process
        Note over mounter: Mountpoint runs until unmount is triggered
    end

    Note over mounter: Unmount operation triggered

    mounter-->>mounter: Mountpoint process exists

    mounter->>mounter: Check for mount.exit file
    Note over mounter: Determines clean vs restart exit

    alt If mount.exit file exists
        mounter->>mounter: Exit with success code (0)
        Note over mounter: Clean shutdown requested by CSI Node Service
    else If mount.exit file not exists
        mounter->>mounter: Write error to mount.error file
        mounter->>mounter: Exit with restart code (1)
        Note over mounter: Kubernetes will restart the pod and <br/> the CSI Node Service will read mount.error file for more details on the error
    end
```
