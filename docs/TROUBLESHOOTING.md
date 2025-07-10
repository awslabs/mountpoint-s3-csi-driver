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