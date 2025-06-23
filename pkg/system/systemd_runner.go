package system

import (
	"context"
	"sync"
	"sync/atomic"

	"k8s.io/klog/v2"
)

// AtomicSystemdSupervisor is a concurrency-safe container
// for a SystemdSupervisor interface value.
type AtomicSystemdSupervisor struct {
	v atomic.Value // stores a Supervisor
}

// Store sets the supervisor. The first call defines the allowed dynamic type.
func (a *AtomicSystemdSupervisor) Store(s SystemdSupervisor) {
	a.v.Store(s)
}

// Load returns the current SystemdSupervisor or nil if unset.
func (a *AtomicSystemdSupervisor) Load() SystemdSupervisor {
	val := a.v.Load()
	if val == nil {
		return nil
	}
	return val.(SystemdSupervisor)
}

// SystemdRunner is a concurrency-safe wrapper around SystemdSupervisor,
// designed to ensure resilience against D-Bus connection loss.
//
// It holds an atomic reference to the current SystemdSupervisor, and ensures that
// if the connection to systemd is lost (e.g. due to systemd restart or transient errors),
// it will transparently create a new SystemdSupervisor instance via the provided factory.
//
// ### Concurrency Model
//   - Public methods (StartService and RunOneshot) are safe for concurrent use.
//   - Reads of the supervisor are lock-free via atomic.Value.
//   - Reconnection is synchronized via an internal mutex to prevent duplicate reconnection
//     attempts or resource leaks. Only one reconnection attempt will proceed at a time.
//
// ### Failure Handling
// The systemd D-Bus connection may break under several conditions:
//
//  1. **Just before the delegate's StartService/RunOneshot is invoked**.
//     - in this case the error will be reported immediately
//     - the caller is expected to retry
//  2. **In the middle of a StartService or RunOneshot call**:
//     - the error will be reported either after the next Node(Un)PublishVolume when
//     current systemdSupervisor will get closed or when the provided ctx gets cancelled (with 30s timeout).
//     - the caller is expected to retry
//  3. **When no service operations are running**:
//     - If the connection is lost during idle time (i.e., no active StartService/RunOneshot),
//     it will be detected and recovered during the next volume operation.
//     - If reconnection is successful, no error is returned to the caller.
type SystemdRunner struct {
	systemdSupervisorMutex sync.Mutex // protects write path only
	systemdSupervisor      AtomicSystemdSupervisor
	supervisorFactory      SystemdSupervisorFactory
}

func StartSystemdRunner(supervisorFactory SystemdSupervisorFactory) (*SystemdRunner, error) {
	runner := &SystemdRunner{supervisorFactory: supervisorFactory}
	supervisor, err := supervisorFactory.StartSupervisor()
	if err != nil {
		return nil, err
	}
	runner.systemdSupervisor.Store(supervisor)
	return runner, nil
}

func (s *SystemdRunner) StartService(ctx context.Context, config *ExecConfig) (string, error) {
	systemdSupervisor, err := s.ensureConnectionOpen()
	if err != nil {
		return "", err
	}

	return systemdSupervisor.StartService(ctx, config)
}

func (s *SystemdRunner) RunOneshot(ctx context.Context, config *ExecConfig) (string, error) {
	systemdSupervisor, err := s.ensureConnectionOpen()
	if err != nil {
		return "", err
	}

	return systemdSupervisor.RunOneshot(ctx, config)
}

func (s *SystemdRunner) ensureConnectionOpen() (SystemdSupervisor, error) {
	current := s.systemdSupervisor.Load()

	if !current.IsConnectionClosed() {
		return current, nil // fast path, connection is open
	}

	// Slow path: recheck under lock and replace if needed
	s.systemdSupervisorMutex.Lock()
	defer s.systemdSupervisorMutex.Unlock()

	// Reload to avoid race: someone else may have swapped it already
	current = s.systemdSupervisor.Load()
	if !current.IsConnectionClosed() {
		return current, nil // another goroutine already fixed it
	}

	klog.V(4).Info("SystemdRunner: systemd connection was closed, re-creating systemd supervisor")
	current.Stop()

	newSupervisor, err := s.supervisorFactory.StartSupervisor()
	if err != nil {
		return nil, err
	}
	s.systemdSupervisor.Store(newSupervisor)

	return newSupervisor, nil
}
