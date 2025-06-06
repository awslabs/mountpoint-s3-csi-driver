# [WIP] Upgrading Mountpoint for Amazon S3 CSI Driver from v1 to v2

Mountpoint CSI Driver v2 contains some breaking changes compared to v1 depending on the use-case,
we kindly ask you to go over this list before upgrading to v2.
Prior to v2, Mountpoint processes were spawned on the host using `systemd`,
with v2, Mountpoint processes will be spawned on dedicated and unprivileged Mountpoint Pods.
This architectural shift is the main reason for some breaking changes with v2.

## Changes

### [Instance Metadata Service (IMDS)](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/configuring-instance-metadata-service.html) might not be accessible if the IMDS hop limit is not configured properly

- How do I know if I'm affected?
  - You can check IMDS hop limit of your launch template for your nodes, it needs to be `2` or greater for Pods to be able to access to IMDS. You can also use the following script to create a temporary Pod to test if it can access to IMDS:
    - ```bash
      $ kubectl run imds-test --restart=Never --rm -it --image=amazonlinux:2 -- bash -c '
        IMDSv1="NOT accessible"
        IMDSv2="NOT accessible"

        # Check IMDSv1
        if curl -s --connect-timeout 3 -f http://169.254.169.254/latest/meta-data/ &>/dev/null; then
          IMDSv1="accessible"
        fi

        # Check IMDSv2
        TOKEN=$(curl -s -X PUT -f -H "X-aws-ec2-metadata-token-ttl-seconds: 21600" http://169.254.169.254/latest/api/token 2>/dev/null)
        if [ ! -z "$TOKEN" ] && curl -s -f -H "X-aws-ec2-metadata-token: $TOKEN" http://169.254.169.254/latest/meta-data/ &>/dev/null; then
          IMDSv2="accessible"
        fi

        echo "IMDSv1: $IMDSv1"
        echo "IMDSv2: $IMDSv2"'
        IMDSv1: NOT accessible
        IMDSv2: accessible
        pod "imds-test" deleted
      ```
    - As long as IMDSv1 or IMDSv2 is available you're not affected.

- What's affected?
  - [Driver-Level Credentials with Node IAM Profiles](./CONFIGURATION.md#driver-level-credentials-with-node-iam-profiles) might not work
  - Automatic AWS region and network bandwidth detection might not work

- How can I fix it?
  - You can enable IMDS by following [Configure the Instance Metadata Service](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/configuring-instance-metadata-options.html) options if you wish
  - You can provide AWS region (`--region`) and network bandwidth (`--maximum-throughput-gbps`) parameters via `mountOptions` field in your PersistentVolume definition explicitly

### Cache folder is now within the Mountpoint container and not on the host filesystem

- What's affected?
  - Although [it was discouraged by Mountpoint](https://github.com/awslabs/mountpoint-s3/blob/main/doc/CONFIGURATION.md#using-multiple-mountpoint-processes-on-a-host), it was possible to share local cache between multiple Mountpoint processes by pointing them to same cache folder due to fact they were all running on the host filesystem. Starting with v2, using the same cache folder in multiple volumes will result in unique cache folder in each container.
  - This might cause disk usage to grow much faster compared to v1 as now each Mountpoint process will maintain its own cache

- How can I fix it?
  - The CSI Driver v2 adds support for [Mountpoint Pod Sharing](MOUNTPOINT_POD_SHARING.md), which allows multiple workloads to share a single Mountpoint instance when appropriate. This will prevent duplicate cache folder for the same volume if Pod sharing is possible for the workloads.
  - You can configure maximum cache size for each volume to prevent cache grow:
    - ```yaml
      apiVersion: v1
      kind: PersistentVolume
      metadata:
        name: s3-pv
      spec:
        # ...
        csi:
          driver: s3.csi.aws.com
          volumeAttributes:
            # ...
            cache: emptyDir
            cacheEmptyDirSizeLimit: 1Gi
        ```
  - Also see [the new caching configuration](CACHING.md) the CSI Driver v2 provides.

### Mountpoint CSI Driver now supports `VOLUME_MOUNT_GROUP` and will respect `fsGroup` configured in `securityContext`

See [Delegating volume permission and ownership change to CSI driver](https://kubernetes.io/docs/tasks/configure-pod-container/security-context/#delegating-volume-permission-and-ownership-change-to-csi-driver) for more details.

### Mountpoint logs now accessible via `kubectl logs`

- What's affected?
  - Accessing Mountpoint logs via `journalctl` or log file by using `--log-directory` is no longer supported

- How can I fix it?
  - You can now access to Mountpoint logs just using `kubectl logs -n mount-s3 ...`. For example, `kubectl logs -n mount-s3 -ls3.csi.aws.com/volume-name=s3-pv` would print the logs for `s3-pv` volume.
  - Using `--log-directory` to write logs to a file is not recommended, and instead users should just rely on `kubectl logs` or Kubernetes' mechanism to redirect logs to somewhere else.

### Mountpoint processes and Pods will run as non-root

- What's affected?
  - If you were relied on Mountpoint process to run as root (`0`), it won't be the case anymore

- How can I fix it?
  - Mountpoint CSI Driver will correctly set permissions for cache folder, respect `fsGroup` and configure `mountOptions` automatically. You shouldn't have to do anything unless you're relying on Mountpoint process to run as root, which shouldn't be needed if you previously had to set up that way. Feel free to create an issue if you still need to rely on Mountpoint process' `uid`.

## FAQ

### How can I upgrade to v2 from v1?

- Do I need to uninstall Mountpoint CSI Driver v1?
  - No, just upgrading Mountpoint CSI Driver to v2 should be enough. The new workloads created after the upgrade will use the new mechanism.

- Do I need to terminate the workloads using Mountpoint CSI Driver v1?
  - No, Mountpoint CSI Driver v2 will keep providing and managing Mountpoint processes spawned by v1.

### How can I downgrade to v1 from v2?

- You would need to terminate the workloads spawned with Mountpoint CSI Driver v2 before downgrading to v1.
  - You can see the running Mountpoint Pods with:
    - `kubectl -n mount-s3 get pods`
  - You can see the associated workloads with:
    - ```bash
      # This script uses `jq` and `kubectl`
      echo -e "ATTACHMENT                     MOUNTPOINT POD       WORKLOAD POD"
      echo -e "-----------------------------  ------------------   ------------------------------"
      pods=$(kubectl get pods --all-namespaces -o json)
      kubectl get s3pa -o json | jq -r '.items[] | .metadata.name as $s3paName | .spec.mountpointS3PodAttachments | to_entries[] | [$s3paName, .key, .value[0].workloadPodUID] | @tsv' |
      while IFS=$'\t' read -r s3pa_name mppod_name workload_uid; do
        workload_info=$(echo -E "$pods" | jq -r ".items[] | select(.metadata.uid==\"$workload_uid\") | .metadata.namespace + \"/\" + .metadata.name")
        printf "%-30s %-20s %s\n" "$s3pa_name" "$mppod_name" "$workload_info"
      done
      ```
