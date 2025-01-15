package util_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/awslabs/aws-s3-csi-driver/pkg/util"
	"github.com/awslabs/aws-s3-csi-driver/pkg/util/testutil/assert"
)

func TestWaitingForUnixSocket(t *testing.T) {
	const retryPeriod = 500 * time.Microsecond

	t.Run("file already exists", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "test.sock")

		_, err := os.Create(path)
		assert.NoError(t, err)

		err = util.WaitForUnixSocket(1*time.Millisecond, retryPeriod, path)
		assert.NoError(t, err)
	})

	t.Run("file appears after a while", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "test.sock")

		go func() {
			time.Sleep(1 * time.Millisecond)
			_, err := os.Create(path)
			assert.NoError(t, err)
		}()

		err := util.WaitForUnixSocket(10*time.Millisecond, retryPeriod, path)
		assert.NoError(t, err)
	})

	t.Run("file does not appear within the timeout", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "test.sock")

		err := util.WaitForUnixSocket(1*time.Millisecond, retryPeriod, path)
		assert.Equals(t, util.ErrUnixSocketNotExists, err)
	})
}
