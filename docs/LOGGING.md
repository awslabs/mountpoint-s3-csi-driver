# Logging

There are two types of logging you can use for troubleshooting. The first set of logs are from the CSI Driver, and the other are from Mountpoint.

## CSI Driver logs

By default, CSI Driver logs are written to the driver pod’s `stderr` and those are captured by Kubernetes and may be retrieved with a corresponding API call:

    POD_NAME=s3-app
    NODE_NAME=$(kubectl get pod ${POD_NAME} -o=custom-columns=NODE:.spec.nodeName | awk 'FNR == 2 {print}')
    DRIVER_POD=$(kubectl get pods -n kube-system --field-selector spec.nodeName=${NODE_NAME} -o=custom-columns=NAME:.metadata.name | grep s3-csi-node)
    kubectl logs ${DRIVER_POD} -n kube-system --container s3-plugin

## Mountpoint logs

Mountpoint logs are written in a node which your application pods are running. The location of the logs may vary by your operating systems, but they usually are written to host’s systemd journal. To fetch these logs, first you need to find the pod UID and node name for the pod.

    POD_NAME=s3-app
    kubectl get pods ${POD_NAME} -o jsonpath='{.metadata.uid}'
    kubectl get pods ${POD_NAME} -o=custom-columns=NODE:.spec.nodeName

Note down the pod UID and node name.

Next, connect to the Kubernetes node with the name discovered in the previous step. If you are using EKS with EC2 instances as worker nodes, you can find more details about how to connect to your nodes in the [AWS documentation](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/connect-to-linux-instance.html).

Once you have a session on the node itself, use the UID noted earlier to find the corresponding Mountpoint logs.

    POD_UID=f1015626-32ed-46e6-9634-c739c3a31312
    PV_NAME=s3-pv
    MOUNT_PID=$(ps -ef | grep "$POD_UID.*$PV_NAME" | grep -v "grep" | awk '{print $2}')
    UNIT=$(systemctl status $MOUNT_PID | grep --only-matching "mount-s3-.*\.service" | tail -1)
    journalctl --unit $UNIT

For more details about Mountpoint logging and the configuration options available, please refer to [Mountpoint's logging documentation](https://github.com/awslabs/mountpoint-s3/blob/main/doc/LOGGING.md).