# Metrics in Mountpoint S3 CSI Driver

Since Mountpoint for Amazon S3 CSI Driver V2.2,
metrics can be published from the Mountpoint filesystem over OpenTelemetry Protocol (OTLP).

To learn more about the metrics emitted,
visit [Mountpoint's metric documentation](https://github.com/awslabs/mountpoint-s3/blob/main/doc/METRICS.md).

To use the metrics emitted over OTLP, you'll need an OpenTelemetry receiver.
The next section will show you how to get started with CloudWatch Agent,
deployed as a daemonset using the Amazon CloudWatch Observability Add-on.

## Getting started using Amazon CloudWatch Observability Add-on

The goal of this guide is to provide a quickstart with Mountpoint metrics.
We encourage you to review options for metric collection for your Kubernetes clusters.
In the meantime, here's a quick way to explore Mountpoint metrics.

This guide will assume you already have Mountpoint S3 CSI Driver v2.2 or later installed.

### Installing Amazon CloudWatch Observability Add-on

You can install the add-on using the AWS CLI.

First, you should prepare the CloudWatch Observability Add-on config in the file `cw-observability-conf.json`.
The one below is a minimal configuration that instructs CloudWatch Agent to listen for HTTP OTLP requests.

```json
{
    "agent": {
        "config": {
              "metrics": {
                  "metrics_collected": {
                      "otlp": {
                          "http_endpoint": "0.0.0.0:4318"
                      }
                  }
              }
         }
    }
}
```

Next, create the EKS add-on with the given configration JSON.

```
aws eks create-addon \
  --cluster-name my-cluster \
  --addon-name amazon-cloudwatch-observability \
  --configuration-values file://cw-observability-conf.json
```

EKS will ensure that the add-on is installed and that CloudWatch Agent will now be running on each node.

### Running your workload which uses Mountpoint with metrics

To configure Mountpoint to emit metrics,
you need to pass an OTLP HTTP endpoint to Mountpoint when defining the persistent volume.

The add-on installation started CloudWatch Agent on the node
and configured a Kubernetes service for the OTLP HTTP endpoint.
Under `mountOptions`, you should configure the endpoint with the correct port
as specified when configuring the CloudWatch Observability Add-on.
An example `mountOptions` entry is shown below.

```yaml
mountOptions:
    # ...
    - otlp-endpoint=http://cloudwatch-agent.amazon-cloudwatch.svc.cluster.local:4318
```

There is an example static provisioning YAML defining a PV, PVC, and Pod spec that uses Mountpoint's OTLP metrics
at `examples/kubernetes/static-provisioning/mountpoint-metrics.yaml`.

Apply this to your cluster using `kubectl apply -f examples/kubernetes/static-provisioning/mountpoint-metrics.yaml`.

At this point, you should get metrics published to CloudWatch Metrics.
It may take some time for them to appear.
They should be published in the `CWAgent` metric namespace by default.
You can view the [`CWAgent` namespace on the AWS Console](https://console.aws.amazon.com/cloudwatch/home#metricsV2?graph=~()&namespace=~'CWAgent).

You may also try visualizing the new metrics using the
[example dashboard in Mountpoint's metric documentation](https://github.com/awslabs/mountpoint-s3/blob/main/doc/METRICS.md).
