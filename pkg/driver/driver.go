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

	crdv1beta "github.com/awslabs/aws-s3-csi-driver/pkg/api/v1beta"
	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node"
	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/credentialprovider"
	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/mounter"
	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/version"
	"github.com/awslabs/aws-s3-csi-driver/pkg/podmounter/mppod/watcher"
	"github.com/awslabs/aws-s3-csi-driver/pkg/util"
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
	"k8s.io/mount-utils"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"
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
	utilruntime.Must(crdv1beta.AddToScheme(scheme))
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
	if util.UsePodMounter() {
		mountUtil := mount.New("")
		podWatcher := watcher.New(clientset, mountpointPodNamespace, nodeID, podWatcherResyncPeriod)
		err = podWatcher.Start(stopCh)
		if err != nil {
			klog.Fatalf("Failed to start Pod watcher: %v\n", err)
		}

		s3paCache, err := ctrlcache.New(config, ctrlcache.Options{
			Scheme:                      scheme,
			SyncPeriod:                  &podWatcherResyncPeriod,
			ReaderFailOnMissingInformer: true,
			ByObject: map[client.Object]ctrlcache.ByObject{
				&crdv1beta.MountpointS3PodAttachment{}: {
					Field: fields.OneTermEqualSelector("spec.nodeName", nodeID),
				},
			},
		})
		if err != nil {
			klog.Fatalf("Failed to create cache: %v\n", err)
		}

		indexMountpointS3PodAttachmentFields(s3paCache)

		s3podAttachmentInformer, err := s3paCache.GetInformer(context.Background(), &crdv1beta.MountpointS3PodAttachment{})
		if err != nil {
			klog.Fatalf("Failed to create informer for MountpointS3PodAttachment: %v\n", err)
		}

		go func() {
			if err := s3paCache.Start(signals.SetupSignalHandler()); err != nil {
				klog.Fatalf("Failed to start cache: %v\n", err)
			}
		}()

		unmounter := mounter.NewPodUnmounter(nodeID, mountUtil, podWatcher, s3paCache, credProvider)

		s3podAttachmentInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
			UpdateFunc: unmounter.HandleS3PodAttachmentUpdate,
		})

		if !cache.WaitForCacheSync(stopCh, s3podAttachmentInformer.HasSynced) {
			klog.Fatalf("Failed to sync informer cache within the timeout: %v\n", err)
		}

		unmounter.CleanupDanglingMounts()

		mounterImpl, err = mounter.NewPodMounter(podWatcher, s3paCache, credProvider, mountUtil, nil, kubernetesVersion)
		if err != nil {
			klog.Fatalln(err)
		}
		klog.Infoln("Using pod mounter")
	} else {
		mounterImpl, err = mounter.NewSystemdMounter(credProvider, mpVersion, kubernetesVersion)
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

// TODO: This is duplicated multiple times
func indexMountpointS3PodAttachmentFields(s3paCache ctrlcache.Cache) {
	indexField(s3paCache, crdv1beta.FieldNodeName, func(cr *crdv1beta.MountpointS3PodAttachment) string { return cr.Spec.NodeName })
	indexField(s3paCache, crdv1beta.FieldPersistentVolumeName, func(cr *crdv1beta.MountpointS3PodAttachment) string { return cr.Spec.PersistentVolumeName })
	indexField(s3paCache, crdv1beta.FieldVolumeID, func(cr *crdv1beta.MountpointS3PodAttachment) string { return cr.Spec.VolumeID })
	indexField(s3paCache, crdv1beta.FieldMountOptions, func(cr *crdv1beta.MountpointS3PodAttachment) string { return cr.Spec.MountOptions })
	indexField(s3paCache, crdv1beta.FieldAuthenticationSource, func(cr *crdv1beta.MountpointS3PodAttachment) string { return cr.Spec.AuthenticationSource })
	indexField(s3paCache, crdv1beta.FieldWorkloadFSGroup, func(cr *crdv1beta.MountpointS3PodAttachment) string { return cr.Spec.WorkloadFSGroup })
	indexField(s3paCache, crdv1beta.FieldWorkloadServiceAccountName, func(cr *crdv1beta.MountpointS3PodAttachment) string { return cr.Spec.WorkloadServiceAccountName })
	indexField(s3paCache, crdv1beta.FieldWorkloadNamespace, func(cr *crdv1beta.MountpointS3PodAttachment) string { return cr.Spec.WorkloadNamespace })
	indexField(s3paCache, crdv1beta.FieldWorkloadServiceAccountIAMRoleARN, func(cr *crdv1beta.MountpointS3PodAttachment) string { return cr.Spec.WorkloadServiceAccountIAMRoleARN })
}

func indexField(cache ctrlcache.Cache, field string, extractor func(*crdv1beta.MountpointS3PodAttachment) string) {
	err := cache.IndexField(context.Background(), &crdv1beta.MountpointS3PodAttachment{}, field, func(obj client.Object) []string {
		return []string{extractor(obj.(*crdv1beta.MountpointS3PodAttachment))}
	})
	if err != nil {
		klog.Fatalf("Failed to create a %s field indexer: %v", field, err)
	}
}
