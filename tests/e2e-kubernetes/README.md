
# Usage
## Prerequisites
AWS credentials in ENVs with the following policies attached:
```
AmazonEC2FullAccess
AmazonRoute53FullAccess
AmazonS3FullAccess
IAMFullAccess
AmazonVPCFullAccess
AmazonSQSFullAccess
AmazonEventBridgeFullAccess
```

## Setting up the environment
All of the following commands are expected to be executed from repo root:

```bash
ACTION=install_tools tests/e2e-kubernetes/run.sh

ACTION=create_cluster CLUSTER_TYPE=kops tests/e2e-kubernetes/run.sh # set KOPS_STATE_FILE to your bucket when running locally

ACTION=install_driver CLUSTER_TYPE=kops IMAGE_NAME=s3-csi-driver TAG=v0.1.0 tests/e2e-kubernetes/run.sh

ACTION=uninstall_driver CLUSTER_TYPE=kops tests/e2e-kubernetes/run.sh

ACTION=delete_cluster CLUSTER_TYPE=kops tests/e2e-kubernetes/run.sh
```

## Running tests
`run_tests` command is expected to be executed from tests/e2e-kubernetes directory:
```bash
pushd tests/e2e-kubernetes
ACTION=run_tests CLUSTER_TYPE=kops TAG=v0.1.0 ./run.sh
popd
```
