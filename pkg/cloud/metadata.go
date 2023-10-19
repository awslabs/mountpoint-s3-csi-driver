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

package cloud

import (
	"fmt"

	"k8s.io/klog/v2"
)

type metadata struct {
	instanceID           string
	region               string
	availabilityZone     string
	isEC2MetadataEnabled bool
}

const MP_EC2_METADATA_DISABLED_ENV_VAR = "AWS_EC2_METADATA_DISABLED"

var _ MetadataService = &metadata{}

// GetInstanceID returns the instance identification.
func (m *metadata) GetInstanceID() string {
	return m.instanceID
}

// GetRegion returns the region Zone which the instance is in.
func (m *metadata) GetRegion() string {
	return m.region
}

// GetAvailabilityZone returns the Availability Zone which the instance is in.
func (m *metadata) GetAvailabilityZone() string {
	return m.availabilityZone
}

// IsEC2MetadataAvailable returns a boolean whether ec2 metadata is available or not.
func (m *metadata) IsEC2MetadataAvailable() bool {
	return m.isEC2MetadataEnabled
}

func NewMetadataService(ec2MetadataClient EC2MetadataClient, k8sAPIClient KubernetesAPIClient, region string) (MetadataService, error) {
	klog.InfoS("Retrieving instance data from ec2 metadata")
	svc, err := ec2MetadataClient()
	if !svc.Available() {
		klog.InfoS("EC2 metadata is not available")
	} else if err != nil {
		klog.InfoS("Error creating ec2 metadata client", "err", err)
	} else {
		klog.InfoS("EC2 metadata is available")
		return EC2MetadataInstanceInfo(svc, region)
	}

	klog.InfoS("Retrieving instance data from kubernetes API")
	clientset, err := k8sAPIClient()
	if err != nil {
		klog.InfoS("Error creating kubernetes API client", "err", err)
	} else {
		klog.InfoS("Kubernetes API is available")
		return KubernetesAPIInstanceInfo(clientset)
	}

	return nil, fmt.Errorf("Error getting instance data from EC2 metadata or kubernetes API")
}
