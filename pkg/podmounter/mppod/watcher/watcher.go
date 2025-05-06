// Package watcher provides utilities for watching Mountpoint Pods in the cluster.
package watcher

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	listerv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

// ErrPodNotFound returned when the Mountpoint Pod could not be found in the cluster.
var ErrPodNotFound = errors.New("mppod/watcher: mountpoint pod not found")

// ErrPodNotReady returned when the Mountpoint Pod was found but is not ready.
var ErrPodNotReady = errors.New("mppod/watcher: mountpoint pod not ready")

// ErrCacheDesync returned when the Pod informer cache failed to synchronize within the specified timeout.
var ErrCacheDesync = errors.New("mppod/watcher: failed to sync pod informer cache within the timeout")

// Watcher provides functionality to watch and wait for Mountpoint Pods in the cluster.
// It uses the Kubernetes informer to watch and cache Pod events.
type Watcher struct {
	informer cache.SharedIndexInformer
	lister   listerv1.PodNamespaceLister
}

// New creates a new [Watcher] with the given Kubernetes client, Mountpoint Pod namespace, and resync duration.
func New(client kubernetes.Interface, namespace, nodeName string, defaultResync time.Duration) *Watcher {
	factory := informers.NewSharedInformerFactoryWithOptions(
		client,
		defaultResync,
		informers.WithNamespace(namespace),
		informers.WithTweakListOptions(func(options *metav1.ListOptions) {
			options.FieldSelector = fields.OneTermEqualSelector("spec.nodeName", nodeName).String()
		}),
	)
	informer := factory.Core().V1().Pods().Informer()
	lister := factory.Core().V1().Pods().Lister().Pods(namespace)
	return &Watcher{informer, lister}
}

// Start begins watching for Pod events in the cluster.
// It returns [ErrCacheDesync] if the informer cache fails to sync before [stopCh] is cancalled.
// The provided [stopCh] can be used to stop the watching process.
func (w *Watcher) Start(stopCh <-chan struct{}) error {
	go w.informer.Run(stopCh)
	if !cache.WaitForCacheSync(stopCh, w.informer.HasSynced) {
		return ErrCacheDesync
	}
	return nil
}

// Get returns pod from watcher's cache.
func (w *Watcher) Get(name string) (*corev1.Pod, error) {
	return w.lister.Get(name)
}

// List returns all pods from watcher's cache.
func (w *Watcher) List() ([]*corev1.Pod, error) {
	return w.lister.List(labels.Everything())
}

// AddEventHandler adds pod event handler.
func (w *Watcher) AddEventHandler(handler cache.ResourceEventHandler) (cache.ResourceEventHandlerRegistration, error) {
	return w.informer.AddEventHandler(handler)
}

// Wait blocks until the specified Mountpoint Pod is found and ready, or until the context is cancelled.
func (w *Watcher) Wait(ctx context.Context, name string) (*corev1.Pod, error) {
	// Set a watcher for Pod create & update events
	var podFound atomic.Bool
	podChan := make(chan *corev1.Pod, 1)
	handle, err := w.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			pod := obj.(*corev1.Pod)
			if pod.Name == name {
				podFound.Store(true)
				if w.isPodReady(pod) {
					podChan <- pod
				}
			}
		},
		UpdateFunc: func(old, new any) {
			pod := new.(*corev1.Pod)
			if pod.Name == name {
				podFound.Store(true)
				if w.isPodReady(pod) {
					podChan <- pod
				}
			}
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to add event handler for %s: %w", name, err)
	}

	// Ensure to remove event handler at the end
	defer w.informer.RemoveEventHandler(handle)

	// Check if the Pod already exists
	pod, err := w.lister.Get(name)
	if err == nil {
		podFound.Store(true)
		if w.isPodReady(pod) {
			// Pod already exists and ready
			return pod, nil
		}
	}

	if err != nil && !apierrors.IsNotFound(err) {
		// We got a different error than "not found", just propagate it
		return nil, fmt.Errorf("failed to get pod %s: %w", name, err)
	}

	// Pod does not exists or not ready yet. We set a watcher for create & update events,
	// and will receive the Pod from `podChan` once its ready.
	select {
	case pod := <-podChan:
		// Pod found and ready
		return pod, nil
	case <-ctx.Done():
		// We didn't received the Pod within the timeout

		if podFound.Load() {
			// Pod was found, but was not ready
			return nil, ErrPodNotReady
		}

		return nil, ErrPodNotFound
	}
}

// isPodReady returns whether the given Mountpoint Pod is ready.
func (w *Watcher) isPodReady(pod *corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodRunning
}
