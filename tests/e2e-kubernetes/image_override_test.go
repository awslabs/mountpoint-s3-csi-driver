package e2e

import (
	"os"
	"testing"

	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	imageutils "k8s.io/kubernetes/test/utils/image"
	admissionapi "k8s.io/pod-security-admission/api"
)

func TestOverrideUpstreamTestImage(t *testing.T) {
	// init() has already called overrideUpstreamTestImage(), so verify that the
	// upstream BusyBox image config reflects our TEST_POD_IMAGE (or the default).
	expectedImage := os.Getenv("TEST_POD_IMAGE")
	if expectedImage == "" {
		expectedImage = "public.ecr.aws/amazonlinux/amazonlinux:2023"
	}

	t.Run("imageutils returns overridden image", func(t *testing.T) {
		got := imageutils.GetE2EImage(imageutils.BusyBox)
		if got != expectedImage {
			t.Errorf("GetE2EImage(BusyBox) = %q, want %q", got, expectedImage)
		}
	})

	t.Run("e2epod.MakePod uses overridden image", func(t *testing.T) {
		pod := e2epod.MakePod("default", nil, nil, admissionapi.LevelBaseline, "")
		for _, container := range pod.Spec.Containers {
			if container.Image != expectedImage {
				t.Errorf("e2epod.MakePod() container image = %q, want %q", container.Image, expectedImage)
			}
		}
	})
}
