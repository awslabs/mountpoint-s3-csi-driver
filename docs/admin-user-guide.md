# Administrator and User Guide

This guide defines roles and responsibilities for administrators managing the Scality S3 CSI Driver and users consuming S3 storage in Kubernetes.

## Administrator Responsibilities

- Install and configure the driver (Helm, global settings, upgrades)
- Manage network connectivity to S3
- Create PersistentVolumes for S3 buckets
- Set mount options and manage credentials
- Rotate credentials, implement least-privilege, monitor logs

## User Responsibilities

- Create PVCs referencing provided PVs
- Mount volumes in pods
- Understand S3 consistency and error handling
- Follow naming conventions and data management best practices

## Communication Workflows

1. User requests storage (bucket, access, requirements)
2. Admin reviews, creates bucket/PV, provides PV name
3. User creates PVC and deploys application

## Troubleshooting

- User: Check pod logs, PVC binding, file ops
- Admin: Review driver logs, S3 connectivity, credentials
- Storage admin: Collect Scality S3 diagnostics, open support ticket
