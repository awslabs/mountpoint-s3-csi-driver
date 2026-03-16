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

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/cluster"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/version"
)

const (
	userAgentCsiDriverPrefix        = "s3-csi-driver/"
	userAgentK8sPrefix              = "k8s/"
	userAgentCredentialSourcePrefix = "credential-source#"
	userAgentOpenShiftPrefix        = "md/openshift"
	userAgentInstallPrefix          = "md/install#"
)

// UserAgent returns user-agent for the CSI driver.
// The format is: s3-csi-driver/VERSION credential-source#SOURCE k8s/VERSION [md/openshift] md/install#METHOD
func UserAgent(authenticationSource string, kubernetesVersion string, variant cluster.Variant) string {
	var b strings.Builder
	installMethod := cluster.InstallationMethod()

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

	if variant == cluster.OpenShift {
		// md/openshift (version will be added in a follow-up)
		b.WriteRune(' ')
		b.WriteString(userAgentOpenShiftPrefix)
	}

	// md/install#helm
	b.WriteRune(' ')
	b.WriteString(userAgentInstallPrefix)
	b.WriteString(installMethod)

	return b.String()
}
