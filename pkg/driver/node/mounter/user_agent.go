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
	"strings"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/version"
)

const (
	userAgentCsiDriverPrefix        = "s3-csi-driver/"
	userAgentK8sPrefix              = "k8s/"
	userAgentCredentialSourcePrefix = "credential-source#"
)

// UserAgent returns user-agent for the CSI driver.
func UserAgent(authenticationSource string, kubernetesVersion string) string {
	var b strings.Builder

	// s3-csi-driver/v0.0.0
	b.WriteString(userAgentCsiDriverPrefix)
	b.WriteString(version.GetVersion().DriverVersion)

	// credential-source#pod
	b.WriteRune(' ')
	b.WriteString(userAgentCredentialSourcePrefix)
	b.WriteString(authenticationSource)

	if kubernetesVersion != "" {
		// k8s/v0.0.0
		b.WriteRune(' ')
		b.WriteString(userAgentK8sPrefix)
		b.WriteString(kubernetesVersion)
	}

	return b.String()
}
