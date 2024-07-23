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

package driver

import (
	"fmt"
	"reflect"
	"runtime"
	"testing"
)

func TestGetVersion(t *testing.T) {
	version := GetVersion()

	expected := VersionInfo{
		DriverVersion: "",
		GitCommit:     "",
		BuildDate:     "",
		GoVersion:     runtime.Version(),
		Compiler:      runtime.Compiler,
		Platform:      fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH),
	}

	if !reflect.DeepEqual(version, expected) {
		t.Fatalf("structs not equal\ngot:\n%+v\nexpected: \n%+v", version, expected)
	}
}

func TestGetVersionJSON(t *testing.T) {
	version, err := GetVersionJSON()
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	expected := fmt.Sprintf(`{
  "driverVersion": "",
  "gitCommit": "",
  "buildDate": "",
  "goVersion": "%s",
  "compiler": "%s",
  "platform": "%s"
}`, runtime.Version(), runtime.Compiler, fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH))

	if version != expected {
		t.Fatalf("json not equal\ngot:\n%s\nexpected:\n%s", version, expected)
	}
}

func TestUserAgent(t *testing.T) {
	tests := map[string]struct {
		k8sVersion string
		result     string
	}{
		"empty k8s version": {
			k8sVersion: "",
			result:     "s3-csi-driver/",
		},
		"stock k8s version": {
			k8sVersion: "v1.29.6",
			result:     "s3-csi-driver/ k8s/v1.29.6",
		},
		"eks k8s version": {
			k8sVersion: "v1.30.2-eks-db838b0",
			result:     "s3-csi-driver/ k8s/v1.30.2-eks-db838b0",
		},
	}

	for name, test := range tests {
		test := test
		t.Run(name, func(t *testing.T) {
			if got, expected := UserAgent(test.k8sVersion), test.result; got != expected {
				t.Fatalf("UserAgent(%q) returned %q; expected %q", test.k8sVersion, got, expected)
			}
		})
	}
}
