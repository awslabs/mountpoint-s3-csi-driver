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

	"github.com/awslabs/aws-s3-csi-driver/pkg/driver"
	"k8s.io/klog/v2"
)

var unknownVersion = "UNKNOWN"

const (
	NodeIDEnvVar = "CSI_NODE_NAME"
)

func main() {
	var (
		endpoint  = flag.String("endpoint", "unix://tmp/csi.sock", "CSI Endpoint")
		version   = flag.Bool("version", false, "Print the version and exit")
		mpVersion = flag.String("mp-version", os.Getenv("MOUNTPOINT_VERSION"), "mp version to report in service name")
		nodeID    = flag.String("node-id", os.Getenv(NodeIDEnvVar), "node-id to report in NodeGetInfo RPC")
	)
	klog.InitFlags(nil)
	flag.Parse()

	if *version {
		info, err := driver.GetVersionJSON()
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
	drv := driver.NewDriver(*endpoint, *mpVersion, *nodeID)
	if err := drv.Run(); err != nil {
		klog.Fatalln(err)
	}
}
