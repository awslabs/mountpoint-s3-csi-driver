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

package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/version"
	utillog "github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/log"
	"k8s.io/klog/v2"
)

var unknownVersion = "UNKNOWN"

const (
	NodeIDEnvVar = "CSI_NODE_NAME"
)

func main() {
	var (
		endpoint     = flag.String("endpoint", "unix://tmp/csi.sock", "CSI Endpoint")
		printVersion = flag.Bool("version", false, "Print the version and exit")
		mpVersion    = flag.String("mp-version", os.Getenv("MOUNTPOINT_VERSION"), "mp version to report in service name")
		nodeID       = flag.String("node-id", os.Getenv(NodeIDEnvVar), "node-id to report in NodeGetInfo RPC")
	)
	utillog.InitKlog()
	flag.Parse()

	if *printVersion {
		info, err := version.GetVersionJSON()
		if err != nil {
			klog.Fatalln(err)
		}
		fmt.Println(info)
		os.Exit(0)
	}

	if mpVersion == nil {
		mpVersion = &unknownVersion
	}
	if *nodeID == "" {
		klog.Fatalln("node-id is required")
	}

	drv, err := driver.NewDriver(*endpoint, *mpVersion, *nodeID)
	if err != nil {
		klog.Fatalf("failed to create driver: %s", err)
	}

	// Handle shutdown signals
	stopCh := make(chan os.Signal, 1)
	signal.Notify(stopCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-stopCh
		klog.Infof("Received signal %s, shutting down...", sig)
		drv.Stop()
	}()

	if err := drv.Run(); err != nil {
		klog.Fatalln(err)
	}
}
