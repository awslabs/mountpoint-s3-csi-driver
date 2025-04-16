# E2E Scripts for Scality CSI Driver

This directory contains scripts for end-to-end testing of the Scality CSI driver.

## Current Structure

The main entry point is `run.sh` which supports the following commands:
- `install`: Installs and verifies the CSI driver 
- `test`: Runs end-to-end tests
- `all`: Installs the driver and runs tests
- `uninstall`: Uninstalls the CSI driver (placeholder)
- `help`: Shows usage information

## Future Scripts

This directory will contain additional scripts for:
- Test setup and teardown
- Test data generation
- Performance testing
- Cleanup utilities
- Custom test scenarios

## Usage

Scripts in this directory are intended to be called from the Makefile targets.

Example:
```bash
# Run from project root
make csi-install
make e2e-scality
make e2e-scality-all
``` 