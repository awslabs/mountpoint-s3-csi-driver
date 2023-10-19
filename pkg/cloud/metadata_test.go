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
	"os"
	"testing"

	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/awslabs/aws-s3-csi-driver/pkg/cloud/mocks"
	"github.com/golang/mock/gomock"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

const (
	stdInstanceID       = "i-123456789abc"
	stdRegion           = "region-1"
	stdAvailabilityZone = "az-1"
	nodeName            = "ip-123-45-67-890.us-west-2.compute.internal"
)

func TestNewMetadataService(t *testing.T) {
	testCases := []struct {
		name             string
		isEC2Available   bool
		isPartial        bool
		identityDocument ec2metadata.EC2InstanceIdentityDocument
		node             v1.Node
		err              error
	}{
		{
			name:           "success: ec2 metadata is available",
			isEC2Available: true,
			identityDocument: ec2metadata.EC2InstanceIdentityDocument{
				InstanceID:       stdInstanceID,
				Region:           stdRegion,
				AvailabilityZone: stdAvailabilityZone,
			},
			err: nil,
		},
		{
			name:           "success: ec2 metadata not available, used k8s api",
			isEC2Available: false,
			node: v1.Node{
				TypeMeta: metav1.TypeMeta{
					Kind:       "Node",
					APIVersion: "v1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"topology.kubernetes.io/region": stdRegion,
						"topology.kubernetes.io/zone":   stdAvailabilityZone,
					},
					Name: nodeName,
				},
				Spec: v1.NodeSpec{
					ProviderID: "aws:///" + stdAvailabilityZone + "/" + stdInstanceID,
				},
				Status: v1.NodeStatus{},
			},
			err: nil,
		},
		{
			name:           "fail: metadata not available, no provider ID",
			isEC2Available: false,
			node: v1.Node{
				TypeMeta: metav1.TypeMeta{
					Kind:       "Node",
					APIVersion: "v1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"topology.kubernetes.io/region": stdRegion,
						"topology.kubernetes.io/zone":   stdAvailabilityZone,
					},
					Name: nodeName,
				},
				Spec: v1.NodeSpec{
					ProviderID: "",
				},
				Status: v1.NodeStatus{},
			},
			err: fmt.Errorf("Node providerID empty, cannot parse"),
		},
		// {
		// 	name:           "fail: GetInstanceIdentityDocument returned error",
		// 	isEC2Available: true,
		// 	identityDocument: ec2metadata.EC2InstanceIdentityDocument{
		// 		InstanceID:       stdInstanceID,
		// 		Region:           stdRegion,
		// 		AvailabilityZone: stdAvailabilityZone,
		// 	},
		// 	err: fmt.Errorf("Could not get EC2 instance identity metadata: "),
		// },
		{
			name:           "fail: GetInstanceIdentityDocument returned empty instance",
			isEC2Available: true,
			isPartial:      true,
			identityDocument: ec2metadata.EC2InstanceIdentityDocument{
				InstanceID:       "",
				Region:           stdRegion,
				AvailabilityZone: stdAvailabilityZone,
			},
			err: nil,
		},
		{
			name:           "fail: GetInstanceIdentityDocument returned empty region",
			isEC2Available: true,
			isPartial:      true,
			identityDocument: ec2metadata.EC2InstanceIdentityDocument{
				InstanceID:       stdInstanceID,
				Region:           "",
				AvailabilityZone: stdAvailabilityZone,
			},
			err: nil,
		},
		{
			name:           "fail: GetInstanceIdentityDocument returned empty az",
			isEC2Available: true,
			isPartial:      true,
			identityDocument: ec2metadata.EC2InstanceIdentityDocument{
				InstanceID:       stdInstanceID,
				Region:           stdRegion,
				AvailabilityZone: "",
			},
			err: nil,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			clientset := fake.NewSimpleClientset(&tc.node)
			clientsetInitialized := false

			mockCtrl := gomock.NewController(t)
			mockEC2Metadata := mocks.NewMockEC2Metadata(mockCtrl)

			ec2MetadataClient := func() (EC2Metadata, error) { return mockEC2Metadata, nil }
			k8sAPIClient := func() (kubernetes.Interface, error) { clientsetInitialized = true; return clientset, nil }

			mockEC2Metadata.EXPECT().Available().Return(tc.isEC2Available)
			if tc.isEC2Available {
				mockEC2Metadata.EXPECT().GetInstanceIdentityDocument().Return(tc.identityDocument, tc.err)
				if clientsetInitialized == true {
					t.Errorf("kubernetes client was unexpectedly initialized when metadata is available!")
					if len(clientset.Actions()) > 0 {
						t.Errorf("kubernetes client was unexpectedly called! %v", clientset.Actions())
					}
				}
			}
			if !tc.isEC2Available {
				os.Setenv("CSI_NODE_NAME", nodeName)
			}
			m, err := NewMetadataService(ec2MetadataClient, k8sAPIClient, stdRegion)
			if err == nil && !tc.isPartial {
				if tc.err != nil {
					t.Fatalf("NewMetadataService() failed: expected no error, got %v", err)
				}

				if m.GetInstanceID() != stdInstanceID {
					t.Fatalf("GetInstanceID() failed: expected %v, got %v", stdInstanceID, m.GetInstanceID())
				}

				if m.GetRegion() != stdRegion {
					t.Fatalf("GetRegion() failed: expected %v, got %v", stdRegion, m.GetRegion())
				}

				if m.GetAvailabilityZone() != stdAvailabilityZone {
					t.Fatalf("GetAvailabilityZone() failed: expected %v, got %v", stdAvailabilityZone, m.GetAvailabilityZone())
				}
			} else if err != nil && !tc.isPartial {
				if err == nil {
					t.Errorf("Got error %q, expected no error", err)
				} else if err.Error() != tc.err.Error() {
					t.Errorf("Got error %q, expected %q", err, tc.err)
				}
			} else {
				if err == nil {
					t.Fatal("NewMetadataService() failed: expected error when GetInstanceIdentityDocument returns partial data, got nothing")
				}
			}

			mockCtrl.Finish()
		})
	}
}
