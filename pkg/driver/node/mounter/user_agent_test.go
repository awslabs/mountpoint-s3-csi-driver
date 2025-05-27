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

package mounter

import (
	"testing"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider"
)

func TestUserAgent(t *testing.T) {
	tests := map[string]struct {
		k8sVersion           string
		authenticationSource string
		result               string
	}{
		"empty versions": {
			result: "s3-csi-driver/ credential-source#",
		},
		"stock k8s version": {
			k8sVersion: "v1.29.6",
			result:     "s3-csi-driver/ credential-source# k8s/v1.29.6",
		},
		"eks k8s version": {
			k8sVersion: "v1.30.2-eks-db838b0",
			result:     "s3-csi-driver/ credential-source# k8s/v1.30.2-eks-db838b0",
		},
		"driver authentication source": {
			k8sVersion:           "v1.30.2-eks-db838b0",
			authenticationSource: credentialprovider.AuthenticationSourceDriver,
			result:               "s3-csi-driver/ credential-source#driver k8s/v1.30.2-eks-db838b0",
		},
		"pod authentication source": {
			k8sVersion:           "v1.30.2-eks-db838b0",
			authenticationSource: credentialprovider.AuthenticationSourcePod,
			result:               "s3-csi-driver/ credential-source#pod k8s/v1.30.2-eks-db838b0",
		},
	}

	for name, test := range tests {
		test := test
		t.Run(name, func(t *testing.T) {
			if got, expected := UserAgent(test.authenticationSource, test.k8sVersion), test.result; got != expected {
				t.Fatalf("UserAgent(%q, %q) returned %q; expected %q", test.authenticationSource, test.k8sVersion, got, expected)
			}
		})
	}
}
