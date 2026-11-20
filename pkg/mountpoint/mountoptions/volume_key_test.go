package mountoptions_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint/mountoptions"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

func TestVolumeKey(t *testing.T) {
	hex64 := regexp.MustCompile(`^[0-9a-f]{64}$`)

	for _, volumeID := range []string{"a", "s3-pv", "mount.sock", strings.Repeat("a", 253)} {
		key := mountoptions.VolumeKey(volumeID)
		assert.Equals(t, true, hex64.MatchString(key))
		assert.Equals(t, key, mountoptions.VolumeKey(volumeID))
		assert.Equals(t, key+".error", mountoptions.ErrorFileName(volumeID))
	}

	assert.Equals(t, false, mountoptions.VolumeKey("pv-a") == mountoptions.VolumeKey("pv-b"))
	// Pinned so a change to the scheme, which would orphan files on upgraded nodes, is deliberate.
	assert.Equals(t, "1ed674821b06d136dd4c3579b9f11d0b6ea6a7ef8ab8203cab18c1235ac5a5e8", mountoptions.VolumeKey("s3-pv"))
}
