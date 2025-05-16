# Credential Provider for Mountpoint S3 CSI Driver

This package provides mechanisms for obtaining and managing AWS credentials for the Mountpoint S3 CSI Driver. It acts as a credential broker between various authentication sources and the Mountpoint S3 binary.

## Overview

The credential provider system is responsible for:

1. Obtaining AWS credentials from various sources
2. Formatting them in a way Mountpoint S3 can understand
3. Making them available to the Mountpoint process (whether via systemd or pod)
4. Cleaning up credentials after unmounting

## Authentication Sources

The driver supports two authentication sources, controlled via the `authenticationSource` volume attribute:

- `driver` (default): Uses credentials from the CSI driver's environment
- `secret`: Uses credentials from a Kubernetes secret

## Key Components

### Provider

The main entry point is the `Provider` struct, which offers two primary methods:

- `Provide(ctx, provideCtx)`: Obtains credentials based on the authentication source
- `Cleanup(cleanupCtx)`: Cleans up credential files after unmounting

### AWS Profile Package

The `awsprofile` subpackage handles creating and managing AWS credential files in the standard format:

```
[profile-name]
aws_access_key_id=XXXX
aws_secret_access_key=YYYY
aws_session_token=ZZZZ (optional)
```

It creates unique profile names and file paths for each volume mount to ensure isolation.

## Credential Flow

### SystemdMounter

1. Writes credentials to `/csi` in the CSI driver pod (maps to host path)
2. Passes environment variables to the systemd service with paths to credential files
3. The systemd service runs Mountpoint with access to these credentials

## Implementation Details

- Credentials are stored in files with 0640 permissions (CredentialFilePerm)
- Directory permissions are set to 0750 (CredentialDirPerm)
- Each mount gets a unique credential file path based on pod ID and volume ID
- Volume-level AWS profile flags (--profile) are not supported and stripped from mount options

## Cleanup Process

On unmount:
1. The driver calls `credProvider.Cleanup()` 
2. This removes any credential files created for that specific volume
3. This prevents credential leakage between mounts

## Important Notes

- The AWS profile mechanism is used internally by the CSI driver even though `--profile` as a mount option is not supported
- When using the SystemdMounter, credential files are created in the CSI driver pod but accessible from the host system 