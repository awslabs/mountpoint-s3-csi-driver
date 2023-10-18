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
	"testing"

	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/awslabs/aws-s3-csi-driver/pkg/cloud/mocks"
	"github.com/golang/mock/gomock"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

var (
	stdInstanceID       = "instance-1"
	stdRegion           = "region-1"
	stdAvailabilityZone = "az-1"
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
			name:           "success: normal",
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
				},
				Spec: v1.NodeSpec{
					ProviderID: "aws:///" + stdAvailabilityZone + "/" + stdInstanceID,
				},
				Status: v1.NodeStatus{},
			},
			err: nil,
		},
		{
			name:           "fail: metadata not available",
			isEC2Available: false,
			identityDocument: ec2metadata.EC2InstanceIdentityDocument{
				InstanceID:       stdInstanceID,
				Region:           stdRegion,
				AvailabilityZone: stdAvailabilityZone,
			},
			err: nil,
		},
		{
			name:           "fail: GetInstanceIdentityDocument returned error",
			isEC2Available: true,
			identityDocument: ec2metadata.EC2InstanceIdentityDocument{
				InstanceID:       stdInstanceID,
				Region:           stdRegion,
				AvailabilityZone: stdAvailabilityZone,
			},
			err: fmt.Errorf(""),
		},
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

			m, err := NewMetadataService(ec2MetadataClient, k8sAPIClient, stdRegion)
			if tc.isEC2Available && tc.err == nil && !tc.isPartial {
				if err != nil {
					t.Fatalf("NewMetadataService() failed: expected no error, got %v", err)
				}

				if m.GetInstanceID() != tc.identityDocument.InstanceID {
					t.Fatalf("GetInstanceID() failed: expected %v, got %v", tc.identityDocument.InstanceID, m.GetInstanceID())
				}

				if m.GetRegion() != tc.identityDocument.Region {
					t.Fatalf("GetRegion() failed: expected %v, got %v", tc.identityDocument.Region, m.GetRegion())
				}

				if m.GetAvailabilityZone() != tc.identityDocument.AvailabilityZone {
					t.Fatalf("GetAvailabilityZone() failed: expected %v, got %v", tc.identityDocument.AvailabilityZone, m.GetAvailabilityZone())
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
