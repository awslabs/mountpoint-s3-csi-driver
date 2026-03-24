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

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/cluster"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider"
)

func TestUserAgent(t *testing.T) {
	tests := map[string]struct {
		k8sVersion           string
		authenticationSource string
		variant              cluster.Variant
		installationType     string
		result               string
	}{
		"empty versions": {
			installationType: "unknown",
			result:           "s3-csi-driver/ credential-source# md/install#unknown",
		},
		"empty installation type fallback": {
			installationType: "",
			result:           "s3-csi-driver/ credential-source# md/install#unknown",
		},
		"stock k8s version": {
			k8sVersion:       "v1.29.6",
			installationType: "unknown",
			result:           "s3-csi-driver/ credential-source# k8s/v1.29.6 md/install#unknown",
		},
		"eks k8s version": {
			k8sVersion:       "v1.30.2-eks-db838b0",
			installationType: "unknown",
			result:           "s3-csi-driver/ credential-source# k8s/v1.30.2-eks-db838b0 md/install#unknown",
		},
		"driver authentication source": {
			k8sVersion:           "v1.30.2-eks-db838b0",
			authenticationSource: credentialprovider.AuthenticationSourceDriver,
			installationType:     "unknown",
			result:               "s3-csi-driver/ credential-source#driver k8s/v1.30.2-eks-db838b0 md/install#unknown",
		},
		"pod authentication source": {
			k8sVersion:           "v1.30.2-eks-db838b0",
			authenticationSource: credentialprovider.AuthenticationSourcePod,
			installationType:     "unknown",
			result:               "s3-csi-driver/ credential-source#pod k8s/v1.30.2-eks-db838b0 md/install#unknown",
		},
		"eks addon installation method": {
			k8sVersion:           "v1.30.2-eks-db838b0",
			authenticationSource: credentialprovider.AuthenticationSourcePod,
			installationType:     "eks-addon",
			result:               "s3-csi-driver/ credential-source#pod k8s/v1.30.2-eks-db838b0 md/install#eks-addon",
		},
		"helm installation method": {
			k8sVersion:           "v1.28.0",
			authenticationSource: credentialprovider.AuthenticationSourceDriver,
			installationType:     "helm",
			result:               "s3-csi-driver/ credential-source#driver k8s/v1.28.0 md/install#helm",
		},
		"openshift with helm": {
			k8sVersion:           "v1.33.6",
			authenticationSource: credentialprovider.AuthenticationSourcePod,
			variant:              cluster.OpenShift,
			installationType:     "helm",
			result:               "s3-csi-driver/ credential-source#pod k8s/v1.33.6 md/openshift md/install#helm",
		},
		"openshift with kustomize": {
			k8sVersion:           "v1.33.6",
			authenticationSource: credentialprovider.AuthenticationSourcePod,
			variant:              cluster.OpenShift,
			installationType:     "kustomize",
			result:               "s3-csi-driver/ credential-source#pod k8s/v1.33.6 md/openshift md/install#kustomize",
		},
		"invalid installation method": {
			k8sVersion:           "v1.30.0",
			authenticationSource: credentialprovider.AuthenticationSourceDriver,
			installationType:     "operator",
			result:               "s3-csi-driver/ credential-source#driver k8s/v1.30.0 md/install#unknown",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("INSTALLATION_TYPE", test.installationType)

			if got, expected := UserAgent(test.authenticationSource, test.k8sVersion, test.variant), test.result; got != expected {
				t.Fatalf("UserAgent(%q, %q, %q) returned %q; expected %q", test.authenticationSource, test.k8sVersion, test.variant.String(), got, expected)
			}
		})
	}
}
