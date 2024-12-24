package credentialprovider

import (
	"io/fs"

	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/envprovider"
)

// CredentialFilePerm is the default permissions to be used for credential files.
// It's only readable and writeable by the owner.
const CredentialFilePerm = fs.FileMode(0600)

// CredentialDirPerm is the default permissions to be used for credential directories.
// It's only readable, listable (execute bit), and writeable by the owner.
const CredentialDirPerm = fs.FileMode(0700)

// Credentials is the interface implemented by credential providers.
type Credentials interface {
	// Source returns the source of these credentials.
	Source() AuthenticationSource

	// Dump dumps credentials into `writePath` and returns environment variables
	// relative to `envPath` to pass to Mountpoint during mount.
	//
	// The environment variables will only passed to Mountpoint once during mount operation,
	// in subsequent calls, this method will update previously written credentials on disk.
	Dump(writePath string, envPath string) (envprovider.Environment, error)
}
