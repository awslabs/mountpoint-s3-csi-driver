# Configuring the Mountpoint for Amazon S3 CSI Driver

See [the Mountpoint documentation](https://github.com/awslabs/mountpoint-s3/blob/main/doc/CONFIGURATION.md) for
Mountpoint specific configuration.

## AWS Credentials

The Mountpoint CSI Driver can be configured to ingest credentials via two approaches: globally for the entire 
Kubernetes cluster, or using credentials assigned to pods.

### Driver-Level Credentials

By setting driver-level credentials, the whole cluster uses the same set of credentials. 

Using this configuration, the credentials that are used are set at installation time, either using Service
Accounts, or using K8s secrets. 

The CSI Driver uses the following load order for credentials:

1. K8s secrets (not recommended)
2. Driver-Level IRSA
3. Instance profiles

See [install.md](./install.md) for more details on this approach.

**TODO** - Migrate driver level setup here.

### Driver-Level Credentials with IRSA

Configuring [IAM Roles for Service Accounts](https://docs.aws.amazon.com/eks/latest/userguide/iam-roles-for-service-accounts.html) (IRSA)
is the recommended way to set up the CSI Driver if you want to use Driver-Level credentials.

This approach associates your AWS role to a Service Account used by the CSI Driver. 
The role is then assumed by the CSI Driver for all volumes with a `driver` authentication source.

```mermaid
graph LR;
    CSI[CSI Driver]

    P["`Application Pod
    *Driver IAM Credentials*`"]

    SA_D[Service Account - Driver]
    SA_P[Service Account - Pod]

    IAM_D[IAM Credentials - Driver]
    IAM_P[IAM Credentials - Pod]

    PV[Persistent Volume]
    PVC[Persistent Volume Claim]

    P --> PVC

    PVC --> PV

    PV --> CSI

    CSI --> SA_D
    P --> SA_P

    SA_D --> IAM_D
    SA_P --> IAM_P

    style IAM_D stroke:#0000ff,fill:#ccccff,color:#0000ff
    style P stroke:#0000ff,fill:#ccccff,color:#0000ff
```

### Driver-Level Credentials with K8s Secrets

Where IAM Roles for Service Accounts (IRSA) isn't a viable option, Mountpoint CSI Driver also supports sourcing static 
AWS credentials from K8s secrets.

> [!WARNING]
> We do not recommend using long-term AWS credentials. Instead, we recommend using short-term credentials with IRSA.


```mermaid
graph LR;
    CSI[CSI Driver]

    P["`Application Pod
    *K8s Secret Credentials*`"]

    K8sS[K8s Secret]

    IAM_LT["Long Term IAM Credentials"]

    PV[Persistent Volume]
    PVC[Persistent Volume Claim]

    P --> PVC

    PVC --> PV

    PV --> CSI

    CSI --> K8sS

    K8sS --> IAM_LT
    
    
    style IAM_LT stroke:#0000ff,fill:#ccccff,color:#0000ff
    style P stroke:#0000ff,fill:#ccccff,color:#0000ff
```

By default, the CSI Driver checks the existence of a secret `aws-secret` in the installation namespace 
(default `kube-system`). 
The secret name configurable if installing with helm: `awsAccessSecret.name`, and the installation namespace is 
configurable with the `--namespace` helm parameter.

```bash
kubectl create secret generic aws-secret \
    --namespace kube-system \
    --from-literal "key_id=${AWS_ACCESS_KEY_ID}" \
    --from-literal "access_key=${AWS_SECRET_ACCESS_KEY}"
```

To use K8s secrets for authentication, the secret must exist before installation, or the CSI Driver pods must be 
restarted to use the secret.

> [!WARNING]
> K8s secrets are not refreshed once read. To update long term credentials stored in K8s secrets, restart the CSI Driver pods.


### Driver-Level Credentials with Node IAM Profiles

To use an IAM [instance profile](https://docs.aws.amazon.com/IAM/latest/UserGuide/id_roles_use_switch-role-ec2_instance-profiles.html),
attach the policy to the instance profile IAM role and turn on access to [instance metadata](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/ec2-instance-metadata.html)
for the instance(s) on which the driver will run.

```mermaid
graph LR;
    CSI[CSI Driver]

    P["`Application Pod
    *Node Instance Profile*`"]

    IAM_Instance["Instance IAM Credentials"]

    PV[Persistent Volume]
    PVC[PVC]

    P --> PVC

    PVC --> PV

    PV --> CSI

    CSI --> IAM_Instance


    style IAM_Instance stroke:#0000ff,fill:#ccccff,color:#0000ff
    style P stroke:#0000ff,fill:#ccccff,color:#0000ff
```


## Pod-Level Credentials

You can configure Mountpoint CSI Driver to use the credentials associated with the pod's Service Account rather than the
driver's own credentials.

With this approach, a multi-tenant architecture is possible using [IAM Roles for Service Accounts](https://docs.aws.amazon.com/eks/latest/userguide/iam-roles-for-service-accounts.html) (IRSA).
Using Pod-Level Credentials with IRSA authentication allows the Mountpoint CSI Driver to use multiple credentials for
each pod. 

```mermaid
graph LR;
    CSI[CSI Driver]

    P["`Application Pod
    *Pod IAM Credentials*`"]

    SA_D[Service Account - Driver]
    SA_P[Service Account - Pod]

    IAM_D[IAM Credentials - Driver]
    IAM_P[IAM Credentials - Pod]

    PV[Persistent Volume]
    PVC[PVC]

    P --> PVC

    PVC --> PV

    PV --> CSI

    CSI --> SA_D
    P --> SA_P

    SA_D --> IAM_D
    SA_P --> IAM_P


    style IAM_P stroke:#0000ff,fill:#ccccff,color:#0000ff
    style P stroke:#0000ff,fill:#ccccff,color:#0000ff
```

To configure the Mountpoint CSI Driver to use Pod-Level Credentials, configure your PV using `authenticationSource: pod`
in the `volumeAttributes` section:
```yaml
apiVersion: v1
kind: PersistentVolume
metadata:
  name: example-s3-pv
spec:
  capacity:
    storage: 1Gi
  accessModes:
    - ReadWriteMany
  mountOptions:
    - region us-east-1
  csi:
    driver: s3.csi.aws.com
    volumeHandle: example-s3-pv
    volumeAttributes:
      bucketName: amzn-s3-demo-bucket
      authenticationSource: pod
```

Pods mounting the specified PV will use the pod's own Service Account for IRSA authentication.

