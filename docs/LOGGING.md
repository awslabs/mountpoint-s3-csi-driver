# Logging of Mountpoint for Amazon S3 CSI Driver

The CSI Driver consists of three main components deployed to your cluster.
This document explains which component you should look at in different scenarios,
and it also explains how to obtain logs for these components.
See [Architecture of Mountpoint for Amazon S3 CSI Driver](./ARCHITECTURE.md)
for more details on the architecture of the CSI Driver.

## The Controller Component (`aws-s3-csi-controller`)

The controller component is responsible for spawning or assigning Mountpoint Pods to your workloads.
If for some reason the controller fails to perform this operation,
you might get a `Failed to find corresponding MountpointS3PodAttachment custom resource ...` error in your workload.

In that case you can get the logs of the controller component by running:

```bash
$ kubectl logs -n kube-system -lapp=s3-csi-controller
```

See [architecture of the controller component](./ARCHITECTURE.md#the-controller-component-aws-s3-csi-controller) for more details.

## The Node Component (`aws-s3-csi-driver`)

The node component is responsible for communication with kubelet running on the node
by implementing [CSI Node Service RPC](https://github.com/container-storage-interface/spec/blob/master/spec.md#node-service-rpc).
It links the Mountpoint Pod spawned by the controller component with the corresponding workloads.
The node component is also responsible for updating AWS credentials periodically.

If your workload fails to start, you can check the logs of the node component for potential errors.

You can get the logs of all node components in your cluster by running:

```bash
$ kubectl logs -n kube-system -c s3-plugin -lapp=s3-csi-node
```

Alternatively, you can get the logs on a specific node by running:

```bash
$ NODE_NAME=my-node
$ DRIVER_POD=$(kubectl get pods -n kube-system -lapp=s3-csi-node --field-selector spec.nodeName=${NODE_NAME} --no-headers -o custom-columns=":metadata.name")
$ kubectl logs -n kube-system -c s3-plugin ${DRIVER_POD}
```

See [architecture of the node component](./ARCHITECTURE.md#the-node-component-aws-s3-csi-driver) for more details.

## The Mounter Component / Mountpoint Pod (`aws-s3-csi-mounter`)

The Mountpoint Pod runs the Mountpoint instance to provide the filesystem for your Amazon S3 bucket.
If for some reason you get an error while accessing your filesystem,
for example because of an AWS credential issue or unsupported filesystem operation,
you can inspect Mountpoint logs to understand what's going on.

Due to [Mountpoint Pod Sharing feature of Mountpoint for Amazon S3 CSI Driver](./MOUNTPOINT_POD_SHARING.md),
multiple workloads can share a single Mountpoint Pod and instance.

The following script prints Mountpoint Pods and the workloads and the volumes they provide:

```bash
# This script uses `jq` and `kubectl`
echo -e "ATTACHMENT                     VOLUME NAME                   MOUNTPOINT POD       WORKLOAD POD"
echo -e "-----------------------------  ----------------------------  ------------------   ------------------------------"
pods=$(kubectl get pods --all-namespaces -o json)
kubectl get s3pa -o json | jq -r '.items[] | .metadata.name as $s3paName | .spec.persistentVolumeName as $volumeName | .spec.mountpointS3PodAttachments | to_entries[] |
    $s3paName as $attachment | $volumeName as $volume | .key as $mppod |
    .value[] | [$attachment, $volume, $mppod, .workloadPodUID] | @tsv' |
while IFS=$'\t' read -r s3pa_name volume_name mppod_name workload_uid; do
  workload_info=$(echo -E "$pods" | jq -r ".items[] | select(.metadata.uid==\"$workload_uid\") | .metadata.namespace + \"/\" + .metadata.name")
  printf "%-30s %-30s %-20s %s\n" "$s3pa_name" "$volume_name" "$mppod_name" "$workload_info"
done
```

You can find the name of the Mountpoint Pod you want to get logs for from the output and use the following to get Mountpoint logs for that specific instance:

```bash
$ kubectl logs -n mount-s3 mp-6ldhl
```

For more details about Mountpoint logging and the configuration options available, please refer to [Mountpoint's logging documentation](https://github.com/awslabs/mountpoint-s3/blob/main/doc/LOGGING.md).
