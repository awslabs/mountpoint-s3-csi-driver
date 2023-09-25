/*
Copyright 2022 The Kubernetes Authors.
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

package sanity

import (
	"os"
	"testing"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	sanity "github.com/kubernetes-csi/csi-test/pkg/sanity"

	"github.com/awslabs/aws-s3-csi-driver/pkg/driver"
	"github.com/awslabs/aws-s3-csi-driver/pkg/util"
)

const (
	mountPath = "/tmp/csi/mount"
	stagePath = "/tmp/csi/stage"
	socket    = "/tmp/csi.sock"
	endpoint  = "unix://" + socket
)

var s3Driver *driver.Driver

func TestSanity(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Sanity Tests Suite")
}

var _ = BeforeSuite(func() {
	s3Driver = driver.NewFakeDriver(endpoint)
	go func() {
		Expect(s3Driver.Run()).NotTo(HaveOccurred())
	}()
})

var _ = AfterSuite(func() {
	s3Driver.Stop()
	Expect(os.RemoveAll(socket)).NotTo(HaveOccurred())
})

var _ = Describe("Amazon S3 CSI Driver", func() {
	_ = os.MkdirAll("/tmp/csi", os.ModePerm)
	config := &sanity.Config{
		Address:        endpoint,
		TargetPath:     mountPath,
		StagingPath:    stagePath,
		TestVolumeSize: 2000 * util.GiB,
		IDGen:          &sanity.DefaultIDGenerator{},
	}
	sanity.GinkgoTest(config)
})
