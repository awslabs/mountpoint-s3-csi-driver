/*
Copyright 2022 The Kubernetes Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

   http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package driver

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"

	crdv2beta "github.com/awslabs/mountpoint-s3-csi-driver/pkg/api/v2beta"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/cluster"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/mounter"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/version"
	mpmounter "github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint/mounter"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/podmounter/mppod/watcher"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util"
)

const (
	driverName = "s3.csi.aws.com"

	grpcServerMaxReceiveMessageSize = 1024 * 1024 * 2 // 2MB

	unixSocketPerm = os.FileMode(0700) // only owner can write and read.
)

var (
	mountpointPodNamespace = os.Getenv("MOUNTPOINT_NAMESPACE")
	podWatcherResyncPeriod = time.Minute
	scheme                 = runtime.NewScheme()
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(crdv2beta.AddToScheme(scheme))
}

type Driver struct {
	Endpoint string
	Srv      *grpc.Server
	NodeID   string

	NodeServer *node.S3NodeServer

	stopCh chan struct{}
}

func NewDriver(endpoint string, mpVersion string, nodeID string) (*Driver, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("cannot create in-cluster config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("cannot create kubernetes clientset: %w", err)
	}

	kubernetesVersion, err := kubernetesVersion(clientset)
	if err != nil {
		klog.Errorf("failed to get kubernetes version: %v", err)
	}

	version := version.GetVersion()
	klog.Infof("Driver version: %v, Git commit: %v, build date: %v, nodeID: %v, mount-s3 version: %v, kubernetes version: %v",
		version.DriverVersion, version.GitCommit, version.BuildDate, nodeID, mpVersion, kubernetesVersion)

	// `credentialprovider.RegionFromIMDSOnce` is a `sync.OnceValues` and it only makes request to IMDS once,
	// this call is basically here to pre-warm the cache of IMDS call.
	go func() {
		_, _ = credentialprovider.RegionFromIMDSOnce()
	}()

	credProvider := credentialprovider.New(clientset.CoreV1(), credentialprovider.RegionFromIMDSOnce)

	stopCh := make(chan struct{})

	var mounterImpl mounter.Mounter
	mpMounter := mpmounter.New()
	if util.UsePodMounter() {
		podWatcher := watcher.New(clientset, mountpointPodNamespace, nodeID, podWatcherResyncPeriod)
		err = podWatcher.Start(stopCh)
		if err != nil {
			klog.Fatalf("Failed to start Pod watcher: %v\n", err)
		}

		s3paCache := setupS3PodAttachmentCache(config, stopCh, nodeID, kubernetesVersion)

		unmounter := mounter.NewPodUnmounter(nodeID, mpMounter, podWatcher, credProvider)

		podWatcher.AddEventHandler(cache.ResourceEventHandlerFuncs{UpdateFunc: unmounter.HandleMountpointPodUpdate})

		go unmounter.StartPeriodicCleanup(stopCh)

		mounterImpl, err = mounter.NewPodMounter(podWatcher, s3paCache, credProvider, mpMounter, nil, nil,
			kubernetesVersion, nodeID)
		if err != nil {
			klog.Fatalln(err)
		}
		klog.Infoln("Using pod mounter")
	} else {
		mounterImpl, err = mounter.NewSystemdMounter(credProvider, mpMounter, mpVersion, kubernetesVersion)
		if err != nil {
			klog.Fatalln(err)
		}
		klog.Infoln("Using systemd mounter")
	}

	nodeServer := node.NewS3NodeServer(nodeID, mounterImpl)

	return &Driver{
		Endpoint:   endpoint,
		NodeID:     nodeID,
		NodeServer: nodeServer,
		stopCh:     stopCh,
	}, nil
}

func (d *Driver) Run() error {
	scheme, addr, err := ParseEndpoint(d.Endpoint)
	if err != nil {
		return err
	}

	listener, err := net.Listen(scheme, addr)
	if err != nil {
		return err
	}

	if scheme == "unix" {
		// Go's `net` package does not support specifying permissions on Unix sockets it creates.
		// There are two ways to change permissions:
		// 	 - Using `syscall.Umask` before `net.Listen`
		//   - Calling `os.Chmod` after `net.Listen`
		// The first one is not nice because it affects all files created in the process,
		// the second one has a time-window where the permissions of Unix socket would depend on `umask`
		// between `net.Listen` and `os.Chmod`. Since we don't start accepting connections on the socket until
		// `grpc.Serve` call, we should be fine with `os.Chmod` option.
		// See https://github.com/golang/go/issues/11822#issuecomment-123850227.
		if err := os.Chmod(addr, unixSocketPerm); err != nil {
			klog.Errorf("Failed to change permissions on unix socket %s: %v", addr, err)
			return fmt.Errorf("Failed to change permissions on unix socket %s: %v", addr, err)
		}
	}

	logErr := func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		resp, err := handler(ctx, req)
		if err != nil {
			klog.Errorf("GRPC error: %v", err)
		}
		return resp, err
	}
	opts := []grpc.ServerOption{
		grpc.UnaryInterceptor(logErr),
		grpc.MaxRecvMsgSize(grpcServerMaxReceiveMessageSize),
	}
	d.Srv = grpc.NewServer(opts...)

	csi.RegisterIdentityServer(d.Srv, d)
	csi.RegisterControllerServer(d.Srv, d)
	csi.RegisterNodeServer(d.Srv, d.NodeServer)

	klog.Infof("Listening for connections on address: %#v", listener.Addr())
	return d.Srv.Serve(listener)
}

func (d *Driver) Stop() {
	klog.Infof("Stopping server")
	if d.stopCh != nil {
		close(d.stopCh)
		d.stopCh = nil
	}
	d.Srv.Stop()
}

func kubernetesVersion(clientset *kubernetes.Clientset) (string, error) {
	version, err := clientset.ServerVersion()
	if err != nil {
		return "", fmt.Errorf("cannot get kubernetes server version: %w", err)
	}

	return version.String(), nil
}

// setupS3PodAttachmentCache sets up cache for MountpointS3PodAttachment custom resource
func setupS3PodAttachmentCache(config *rest.Config, stopCh <-chan struct{}, nodeID, kubernetesVersion string) ctrlcache.Cache {
	options := ctrlcache.Options{
		Scheme:                      scheme,
		SyncPeriod:                  &podWatcherResyncPeriod,
		ReaderFailOnMissingInformer: true,
	}
	isSelectFieldsSupported, err := cluster.IsSelectableFieldsSupported(kubernetesVersion)
	if err != nil {
		klog.Fatalf("Failed to check support for selectable fields in the cluster %v\n", err)
	}
	if isSelectFieldsSupported {
		options.ByObject = map[client.Object]ctrlcache.ByObject{
			&crdv2beta.MountpointS3PodAttachment{}: {
				Field: fields.OneTermEqualSelector("spec.nodeName", nodeID),
			},
		}
	} else {
		// TODO: We can potentially use label filter hash of nodeId for old clusters instead of field selector
		options.ByObject = map[client.Object]ctrlcache.ByObject{
			&crdv2beta.MountpointS3PodAttachment{}: {},
		}
	}

	s3paCache, err := ctrlcache.New(config, options)
	if err != nil {
		klog.Fatalf("Failed to create cache: %v\n", err)
	}

	if err := crdv2beta.SetupCacheIndices(s3paCache); err != nil {
		klog.Fatalf("Failed to setup field indexers: %v", err)
	}

	s3podAttachmentInformer, err := s3paCache.GetInformer(context.Background(), &crdv2beta.MountpointS3PodAttachment{})
	if err != nil {
		klog.Fatalf("Failed to create informer for MountpointS3PodAttachment: %v\n", err)
	}

	go func() {
		if err := s3paCache.Start(signals.SetupSignalHandler()); err != nil {
			klog.Fatalf("Failed to start cache: %v\n", err)
		}
	}()

	if !cache.WaitForCacheSync(stopCh, s3podAttachmentInformer.HasSynced) {
		klog.Fatalf("Failed to sync informer cache within the timeout: %v\n", err)
	}

	return s3paCache
}
