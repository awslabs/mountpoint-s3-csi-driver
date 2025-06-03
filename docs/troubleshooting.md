# Troubleshooting

This guide helps diagnose and resolve common issues with the Scality S3 CSI Driver.

## Common Issues

| Symptom | Probable Cause | Fix |
|---------|----------------|-----|
| Pod stuck in `ContainerCreating` | Mount operation failed | Check driver logs: `kubectl logs -n kube-system <driver-pod>` |
| "Permission denied" accessing files | Missing `allow-other` mount option | Add `allow-other` to PV mountOptions |
| Cannot delete files | Missing `allow-delete` mount option | Add `allow-delete` to PV mountOptions |
| Mount fails with "Transport endpoint not connected" | S3 endpoint unreachable | Verify network connectivity to S3 endpoint |

## Diagnostic Commands

### Check Driver Status

```bash
kubectl get pods -n kube-system -l app.kubernetes.io/name=scality-mountpoint-s3-csi-driver
kubectl logs -n kube-system <driver-pod-name> -c s3-plugin
```

### Check Mount Status (on node)

```bash
systemctl list-units --all | grep mount-s3
journalctl -u <mount-unit-name> -f
```

### Verify S3 Connectivity

```bash
curl -I https://s3.example.com
aws s3 ls s3://bucket-name --endpoint-url https://s3.example.com
```

## Common Error Messages

### "Failed to create mount process"

- **Cause**: Mountpoint binary not found or not executable
- **Solution**: Check initContainer logs, ensure `/opt/mountpoint-s3-csi/bin/mount-s3` exists

### "Access Denied"

- **Cause**: Invalid S3 credentials or insufficient permissions
- **Solution**: Verify secret, test credentials with AWS CLI, check bucket policy

### "InvalidBucketName"

- **Cause**: Bucket name doesn't meet S3 requirements
- **Solution**: Verify bucket name, ensure bucket exists, check for typos

!!! tip
    Enable debug logging before reproducing issues to capture detailed diagnostic information.
