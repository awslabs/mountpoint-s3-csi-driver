# Cluster tag isolation for S3 buckets

A policy that names buckets, such as
[Mountpoint's suggested IAM permission policy](https://github.com/awslabs/mountpoint-s3/blob/main/doc/CONFIGURATION.md#iam-permissions),
is the right tool when a workload needs specific buckets, including buckets that several clusters share. As a general
policy for every cluster, though, it has to list every bucket, and a role shared by several clusters can then access
all of their buckets. This page describes a tag-based IAM policy for
[EKS Pod Identity](https://docs.aws.amazon.com/eks/latest/userguide/pod-identities.html), the recommended default, that
limits the Mountpoint for Amazon S3 CSI Driver to the S3 buckets tagged for its own EKS cluster. Policies that name
specific buckets can be added to it. It applies to driver-level and pod-level credentials.

The driver itself makes no AWS API calls (it only reads the region from instance metadata) and creates no AWS
resources. The Mountpoint process that the driver starts for each volume reads and writes objects with the credentials
described in [AWS Credentials](./CONFIGURATION.md#aws-credentials). Isolation therefore depends only on the IAM policy
attached to those credentials and on the tags an administrator puts on each bucket. There are no driver settings to
change.

* [Policy options](#policy-options)
* [How the tag-based policy works](#how-the-tag-based-policy-works)
* [Isolation covers the whole account](#isolation-covers-the-whole-account)
* [Roles per cluster](#roles-per-cluster)
* [New clusters](#new-clusters)
* [Existing clusters](#existing-clusters)
* [What each mismatch does](#what-each-mismatch-does)
* [How to confirm onboarding is complete](#how-to-confirm-onboarding-is-complete)
* [Settings](#settings)

## Policy options

### Tag-based policy (recommended default)

[`docs/iam/cluster-tag-policy.json`](./iam/cluster-tag-policy.json) allows the Mountpoint actions only
on buckets, directory buckets, access points and AWS KMS keys whose `eks:eks-cluster-name` tag equals the name of the
caller's cluster. Use it with the
[EKS Pod Identity trust policy](https://docs.aws.amazon.com/eks/latest/userguide/pod-id-role.html). The file is valid
JSON that you can attach as is.

### Policies that name specific buckets (additive)

When a workload needs a specific bucket that the tag-based policy does not allow, you can create a custom policy that
names the bucket and attach it to the same role as the tag-based policy.
[Mountpoint's suggested IAM permission policy](https://github.com/awslabs/mountpoint-s3/blob/main/doc/CONFIGURATION.md#iam-permissions)
is an example that lists bucket ARNs explicitly. Cases where this is needed include:

* a bucket that several clusters mount, since a bucket carries one cluster tag value;
* a bucket in another region (see [Region and account](#region-and-account));
* a bucket you cannot tag or enable ABAC on.

IAM policies are additive, so the role can access its cluster's tagged buckets and the buckets the custom policy
names. Isolation still holds for every other bucket. The listed buckets are reachable by every cluster whose role has
the policy, whatever their tags.

| Policy | Credentials | Allows |
|---|---|---|
| [`docs/iam/cluster-tag-policy.json`](./iam/cluster-tag-policy.json) | EKS Pod Identity | buckets tagged `eks:eks-cluster-name=<cluster name>` |
| A custom policy that names buckets | any | the buckets it lists |

### Migrating off AmazonS3FullAccess

The AWS managed policy `AmazonS3FullAccess` allows access to every bucket in the account. If your driver's role uses
it, follow [Existing clusters](#existing-clusters) to replace it with the tag-based policy.

## How the tag-based policy works

EKS Pod Identity adds the session tags `eks-cluster-name` and `eks-cluster-arn` to the credentials of every pod. The
policy compares them with the resource:

| Statement | Actions | Resources | Allowed when |
|---|---|---|---|
| `ListBucketsTaggedForThisCluster` | `s3:ListBucket` | general purpose buckets, access points | `aws:ResourceTag/eks:eks-cluster-name` equals `${aws:PrincipalTag/eks-cluster-name}` |
| `ObjectsInBucketsTaggedForThisCluster` | `s3:GetObject`, `s3:PutObject`, `s3:AbortMultipartUpload`, `s3:DeleteObject` | objects, objects through access points | the bucket's `eks:eks-cluster-name` tag equals the caller's cluster |
| `DirectoryBucketSessionsTaggedForThisCluster` | `s3express:CreateSession` | directory buckets and their access points | same |
| `SseKmsKeysTaggedForThisClusterThroughS3` | `kms:Decrypt`, `kms:GenerateDataKey` | KMS keys | same, and only when S3 or S3 Express calls KMS on the caller's behalf (`kms:ViaService`) |

Every statement also requires the request's region to be the region of the caller's cluster, as described in
[Region and account](#region-and-account).

What the bucket needs:

* **General purpose buckets** need the tag `eks:eks-cluster-name=<cluster name>` and
  [attribute-based access control (ABAC)](https://docs.aws.amazon.com/AmazonS3/latest/userguide/buckets-tagging-enable-abac.html)
  enabled with `PutBucketAbac`. Without ABAC, S3 does not evaluate bucket tags in IAM policies, and the policy denies the
  bucket. With ABAC enabled, S3 evaluates the bucket's tags for object actions too, so object tags cannot be used to
  claim an object in another cluster's bucket.
* **Enabling ABAC disables `PutBucketTagging` on that bucket.** S3 then rejects `PutBucketTagging` with
  `This S3 general purpose bucket has attribute-based access control (ABAC) enabled. To add tags to this bucket,
  initiate a TagResource request.` Update any tooling or infrastructure as code that manages bucket tags to use
  `TagResource` and `UntagResource`. Also check other policies in the account that use `aws:ResourceTag` on S3: they
  start to apply to the bucket.
* **Directory buckets** evaluate tags by default. Tag them with `s3control tag-resource` on the
  `arn:aws:s3express:<region>:<account>:bucket/<name>` ARN.
* **Access points** need the tag too. Tag the access point and its bucket for the same cluster. An access point tagged
  for `my-cluster` on a bucket tagged for `other-cluster` is denied.
* **Customer managed KMS keys** used for SSE-KMS on a bucket need the tag `eks:eks-cluster-name=<cluster name>` too.
  Without it, the bucket mounts but reading or writing encrypted objects fails with a KMS `AccessDenied`. The tag-based
  policy only applies if the key policy delegates access to IAM, as the default key policy's
  `Enable IAM User Permissions` statement does. The AWS managed key `aws/s3` cannot be tagged and does not need the tag:
  its key policy already allows use through S3.
* **A KMS key shared by buckets of several clusters** can only carry one cluster's tag. Attach a custom policy that
  allows `kms:Decrypt` and `kms:GenerateDataKey` on that key to the roles of the other clusters, as described in
  [Policies that name specific buckets](#policies-that-name-specific-buckets-additive).

### What the policy cannot scope by tag

Every action the Mountpoint process calls is scoped by a tag condition, and no statement uses `"Resource": "*"`. The
policy grants nothing that creates buckets, access points or keys, or that changes tags or ABAC status.

Two limits remain:

* **Resource policies.** A bucket policy or key policy that names the role directly grants access whatever the identity
  policy says. Do not grant cluster roles access through bucket or key policies if you rely on isolation.
* **Who can tag.** The tags decide which cluster owns a bucket. Allow only administrators to call `s3:TagResource`,
  `s3:UntagResource`, `s3:PutBucketAbac`, `s3express:TagResource` and `kms:TagResource`.

### Region and account

* **Partition.** ARNs use `*` for the partition, so the same file works in every partition.
* **Region.** Each statement requires `aws:PrincipalTag/eks-cluster-arn` to match
  `arn:*:eks:${aws:RequestedRegion}:*:cluster/*`. The request is allowed only in the region of the caller's cluster.
  Buckets, directory buckets and keys in other regions are denied. If a workload must mount a bucket in another region,
  add a policy that names that bucket, as described in
  [Policies that name specific buckets](#policies-that-name-specific-buckets-additive).
* **Account.** The policy does not restrict the bucket's account, because the driver supports
  [cross-account bucket access](./CONFIGURATION.md#cross-account-bucket-access). A bucket in another account still needs
  the `eks:eks-cluster-name` tag, and the bucket owner's bucket policy must grant access. This leaves one gap: a bucket
  in another account that carries your cluster's tag and allows your role in its bucket policy is reachable from your
  cluster. If you do not use cross-account access, add this condition to the `StringEquals` block of each statement:

  ```json
  "aws:ResourceAccount": "${aws:PrincipalAccount}"
  ```

  To allow specific accounts instead, use a list of account IDs, for example `["111122223333", "444455556666"]`.

### IRSA and other credentials

The tag-based policy relies on the session tags that EKS Pod Identity adds. IAM roles for service accounts (IRSA), node
instance profiles and Kubernetes secrets do not carry them, so the policy allows nothing for those credentials. Clusters
that use them use a policy that names their buckets, such as Mountpoint's suggested policy, or adapt
`cluster-tag-policy.json` to a tag they control, for example a tag on the IAM role read with `aws:PrincipalTag`.

## Isolation covers the whole account

A cluster's buckets are protected only when no role that the driver uses in the account, for any cluster, has a policy
that allows them by other means. A policy that names buckets only opens the buckets it lists, to every role that has it.

## Roles per cluster

One IAM role with `cluster-tag-policy.json` can be shared by every cluster in the account and still isolate them. The trust policy
names no cluster, and the session tags come from whichever cluster assumes the role. Repeat only the Pod Identity
association for each cluster.

## New clusters

> [!IMPORTANT]
> These steps are for new clusters, where every bucket is tagged before it is mounted. For a cluster that already
> mounts buckets, follow [Existing clusters](#existing-clusters) instead.

1. Install the [Amazon EKS Pod Identity Agent](https://docs.aws.amazon.com/eks/latest/userguide/pod-id-agent-setup.html)
   add-on. The agent must be installed as an EKS add-on; the driver itself can be installed any way (Helm, the
   manifests in this repository or the EKS add-on).

2. Create the role and attach the policy. Download `docs/iam/cluster-tag-policy.json` from this repository, and save the
   [EKS Pod Identity trust policy](https://docs.aws.amazon.com/eks/latest/userguide/pod-id-role.html) as
   `pod-identity-trust-policy.json`.

   ```bash
   aws iam create-role --role-name s3-csi \
     --assume-role-policy-document file://pod-identity-trust-policy.json
   aws iam create-policy --policy-name s3-csi-cluster-tag-isolation \
     --policy-document file://docs/iam/cluster-tag-policy.json
   aws iam attach-role-policy --role-name s3-csi \
     --policy-arn arn:aws:iam::111122223333:policy/s3-csi-cluster-tag-isolation
   ```

3. Associate the role with a service account.

   For [driver-level credentials](./CONFIGURATION.md#driver-level-credentials-with-eks-pod-identity), use the driver's
   service account:

   ```bash
   aws eks create-pod-identity-association --cluster-name my-cluster \
     --namespace kube-system --service-account s3-csi-driver-sa \
     --role-arn arn:aws:iam::111122223333:role/s3-csi
   ```

   For [pod-level credentials](./CONFIGURATION.md#pod-level-credentials) (`authenticationSource: pod`), associate each
   workload's service account with a role that has `cluster-tag-policy.json`.

4. Tag each bucket for the cluster, then enable ABAC.

   ```bash
   aws s3control tag-resource --account-id 111122223333 \
     --resource-arn arn:aws:s3:::amzn-s3-demo-bucket \
     --tags Key=eks:eks-cluster-name,Value=my-cluster
   aws s3api put-bucket-abac --bucket amzn-s3-demo-bucket --abac-status Status=Enabled
   ```

   Tag directory buckets, access points and customer managed KMS keys with the same key and value.

5. Install the driver as described in [Driver Installation](./INSTALL.md). No extra Helm values are needed:

   ```bash
   helm upgrade --install aws-mountpoint-s3-csi-driver \
     --namespace kube-system \
     aws-mountpoint-s3-csi-driver/aws-mountpoint-s3-csi-driver
   ```

6. Create PersistentVolumes as described in [Static Provisioning](./CONFIGURATION.md#static-provisioning).

## Existing clusters

The driver cannot tag buckets, and its role must not be able to: a role that could add the cluster tag could claim any
untagged bucket in the account. An administrator onboards each bucket instead. There is no temporary policy for this
step; the cluster keeps its current policy until every bucket is onboarded, so mounts keep working at every step.

1. **Inventory.** List the buckets the cluster mounts:

   ```bash
   kubectl get pv -o jsonpath='{range .items[?(@.spec.csi.driver=="s3.csi.aws.com")]}{.spec.csi.volumeAttributes.bucketName}{"\n"}{end}' | sort -u
   ```

   Add the directory buckets, access points and customer managed KMS keys that these volumes use.

2. **Switch to EKS Pod Identity** if the cluster uses IRSA, node instance profiles or secrets. See
   [AWS Credentials](./CONFIGURATION.md#aws-credentials).

3. **Onboard each bucket.** Tag it with `TagResource`, then enable ABAC, as in step 4 of [New clusters](#new-clusters).
   Use `TagResource` rather than `PutBucketTagging`: once ABAC is enabled, S3 rejects `PutBucketTagging` for that bucket.

   A bucket that is already tagged for another cluster, or that several clusters mount, cannot be onboarded for this
   cluster. Do not re-tag it: either stop mounting it here, or keep it in a policy that names it, alongside
   `cluster-tag-policy.json`.

4. **Confirm** that every bucket is onboarded, as described in
   [How to confirm onboarding is complete](#how-to-confirm-onboarding-is-complete).

5. **Attach `cluster-tag-policy.json`** to each role the driver uses for this cluster, alongside the existing policy.

6. **Replace the existing policy.** Detach `AmazonS3FullAccess`. Reduce a policy that names buckets to the buckets
   you could not onboard in step 3, or detach it if there are none. Isolation starts here. Restart one workload and
   check that its volume mounts.

| Step | Policies attached to the driver's roles | Isolated |
|---|---|---|
| Before step 5 | Existing policy | No |
| Steps 5 to 6 | Existing policy and `cluster-tag-policy.json` | No |
| After step 6 | `cluster-tag-policy.json`, and a policy that names the buckets you could not onboard, if any | Yes, except for the named buckets |

If you later adapt `cluster-tag-policy.json` to another tag key, the buckets must be tagged again with that key.

## What each mismatch does

The policy cannot check the state of a bucket in advance. This table shows what happens when the policy and the bucket
or credentials do not match:

| Setup | Policy attached | Result |
|---|---|---|
| Bucket tagged for this cluster, ABAC enabled, Pod Identity credentials | `cluster-tag-policy.json` | The volume mounts, and reads, writes and deletes work. |
| Bucket untagged, or tagged for another cluster | `cluster-tag-policy.json` | The mount fails. The Mountpoint Pod logs `not authorized to perform: s3:ListBucket`. |
| Bucket tagged for this cluster, ABAC not enabled | `cluster-tag-policy.json` | The mount fails with the same `s3:ListBucket` error, because S3 does not evaluate the tag. |
| Bucket tagged with another key, or with a name that differs from the cluster's | `cluster-tag-policy.json` | The mount fails with the same `s3:ListBucket` error. |
| Bucket in another region | `cluster-tag-policy.json` | Every request is denied. |
| IRSA, node instance profile or secret credentials (no cluster session tags) | `cluster-tag-policy.json` | Every request is denied with `AccessDenied`. |
| Any bucket state | `cluster-tag-policy.json` and a policy that names buckets | The named buckets work whatever their tags. Other buckets behave as in the rows above. |
| Any bucket state | A policy that names buckets only | The named buckets work, and no other bucket does. |
| Any bucket state | `AmazonS3FullAccess`, alone or with other policies | Every bucket works, so nothing is isolated. |

When a mount fails this way, the workload pod stays in `ContainerCreating`, and the Mountpoint Pod in the
`mountpointPod.namespace` namespace (`mount-s3` by default) logs a line like this one:

```
WARN ThreadId(05) list_objects{id=0 bucket="amzn-s3-demo-bucket-other" continued=false delimiter="" max_keys="0" prefix=""}: mountpoint_s3_client::s3_crt_client: duration=25.273237ms request_id=GTNR5BH8QHVEXQ51 error=ClientError(Forbidden("User: arn:aws:sts::111122223333:assumed-role/s3-csi/eks-my-cluster-s3-csi-nod-2df14b1f-d518-40ae-adef-29dfd2af2c71 is not authorized to perform: s3:ListBucket on resource: \"arn:aws:s3:::amzn-s3-demo-bucket-other\" because no identity-based policy allows the s3:ListBucket action", ...
```

## How to confirm onboarding is complete

The driver does not tag buckets, so it reports no progress. The administrator checks each bucket instead. Run this
command with the cluster's kubeconfig and administrator AWS credentials. It prints only the buckets that still need
onboarding; empty output means every bucket is done.

```bash
TAG_KEY=eks:eks-cluster-name
CLUSTER=my-cluster
ACCOUNT_ID=111122223333
kubectl get pv -o jsonpath='{range .items[?(@.spec.csi.driver=="s3.csi.aws.com")]}{.spec.csi.volumeAttributes.bucketName}{"\n"}{end}' | sort -u |
while read -r b; do
  abac="$(aws s3api get-bucket-abac --bucket "$b" --query AbacStatus.Status --output text)"
  tag="$(aws s3control list-tags-for-resource --account-id "$ACCOUNT_ID" --resource-arn "arn:aws:s3:::$b" \
    --query "Tags[?Key=='$TAG_KEY']|[0].Value" --output text)"
  if [ "$abac" != Enabled ] || [ "$tag" != "$CLUSTER" ]; then
    printf '%s abac=%s cluster-tag=%s\n' "$b" "$abac" "$tag"
  fi
done
```

Example output for a bucket that still needs onboarding:

```
amzn-s3-demo-bucket abac=Disabled cluster-tag=None
```

A missing tag prints `None`. A bucket that shows another cluster's name is owned by that cluster; see step 3 of
[Existing clusters](#existing-clusters). For buckets in other regions, add `--region` to both AWS CLI calls.

## Settings

This feature adds no driver settings: no command-line flags, environment variables or Helm values. No existing settings
are deprecated. The tag key is fixed by the policy: `eks:eks-cluster-name`, with the cluster name as the value.
