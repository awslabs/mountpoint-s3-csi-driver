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
// It wraps an atomic reference to a SystemdSupervisor instance, and
// transparently replaces it if the underlying systemd D-Bus connection is
// found to be closed. This allows callers to invoke methods like StartService
// or RunOneshot without needing to manage connection state themselves.
//
// The replacement of the supervisor is synchronized via an internal mutex
// to ensure only one reconnection attempt occurs at a time, while allowing
// lock-free reads for all other callers.
//
// Method calls on the underlying SystemdSupervisor are safe to invoke
// concurrently; however, reconnection logic is protected to avoid redundant
// supervisor creation or resource leaks.
type SystemdRunner struct {
	mu                sync.Mutex // protects write path only
	systemdSupervisor AtomicSystemdSupervisor
	supervisorFactory SystemdSupervisorFactory
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
	err := s.ensureConnectionOpen()
	if err != nil {
		return "", nil
	}

	return s.systemdSupervisor.Load().StartService(ctx, config)
}

func (s *SystemdRunner) RunOneshot(ctx context.Context, config *ExecConfig) (string, error) {
	err := s.ensureConnectionOpen()
	if err != nil {
		return "", nil
	}

	return s.systemdSupervisor.Load().RunOneshot(ctx, config)
}

func (s *SystemdRunner) ensureConnectionOpen() error {
	current := s.systemdSupervisor.Load()

	if !current.IsConnectionClosed() {
		return nil // fast path, connection is open
	}

	// Slow path: recheck under lock and replace if needed
	s.mu.Lock()
	defer s.mu.Unlock()

	// Reload to avoid race: someone else may have swapped it already
	current = s.systemdSupervisor.Load()
	if !current.IsConnectionClosed() {
		return nil // another goroutine already fixed it
	}

	klog.V(5).Info("SystemdRunner: systemd connection was closed, re-creating systemd supervisor")
	current.Stop()

	newSup, err := s.supervisorFactory.StartSupervisor()
	if err != nil {
		return err
	}
	s.systemdSupervisor.Store(newSup)

	return nil
}
