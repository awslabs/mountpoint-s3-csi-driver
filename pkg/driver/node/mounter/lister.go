package mounter

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"strings"
	"time"

	"k8s.io/klog/v2"
	"k8s.io/mount-utils"
)

const (
	// Due to some reason that we haven't been able to identify, reading `/host/proc/mounts`
	// fails on newly spawned Karpenter/GPU nodes with "invalid argument".
	// It's reported that reading `/host/proc/mounts` works after some retries,
	// and we decided to add retry mechanism until we find and fix the root cause of this problem.
	// See https://github.com/awslabs/mountpoint-s3-csi-driver/issues/174.
	procMountsReadMaxRetry     = 3
	procMountsReadRetryBackoff = 100 * time.Millisecond
)

type MountLister interface {
	ListMounts() ([]mount.MountPoint, error)
}

type ProcMountLister struct {
	ProcMountPath string
}

func (pml *ProcMountLister) ListMounts() ([]mount.MountPoint, error) {
	var (
		mounts []byte
		err    error
	)

	for i := 1; i <= procMountsReadMaxRetry; i += 1 {
		mounts, err = os.ReadFile(pml.ProcMountPath)
		if err == nil {
			if i > 1 {
				klog.V(4).Infof("Successfully read %s after %d retries", pml.ProcMountPath, i)
			}
			break
		}

		klog.Errorf("Failed to read %s on try %d: %v", pml.ProcMountPath, i, err)
		time.Sleep(procMountsReadRetryBackoff)
	}

	if err != nil {
		return nil, fmt.Errorf("Failed to read %s after %d tries: %w", pml.ProcMountPath, procMountsReadMaxRetry, err)
	}

	return parseProcMounts(mounts)
}

func parseProcMounts(data []byte) ([]mount.MountPoint, error) {
	var mounts []mount.MountPoint

	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 6 {
			return nil, fmt.Errorf("Invalid line in mounts file: %s", line)
		}

		mountPoint := mount.MountPoint{
			Device: fields[0],
			Path:   fields[1],
			Type:   fields[2],
			Opts:   strings.Split(fields[3], ","),
		}

		// Fields 4 and 5 are Freq and Pass respectively. Ignoring

		mounts = append(mounts, mountPoint)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("Error reading mounts data: %w", err)
	}

	return mounts, nil
}
