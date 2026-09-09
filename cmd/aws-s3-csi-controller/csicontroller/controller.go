package csicontroller

import (
	"time"

	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// Name of this component, this needs to be unique as its used as an identifier in logs and metrics.
const Name = "aws-s3-csi-controller"

func ConfigureLeaderElection(opts *manager.Options) {
	opts.LeaderElection = true
	opts.LeaderElectionID = Name
	opts.LeaderElectionResourceLock = "leases"
	opts.LeaderElectionReleaseOnCancel = true
}

func SetLeaderElectionTimings(opts *manager.Options, leaseDuration, renewDeadline, retryPeriod time.Duration) {
	opts.LeaseDuration = &leaseDuration
	opts.RenewDeadline = &renewDeadline
	opts.RetryPeriod = &retryPeriod
}
