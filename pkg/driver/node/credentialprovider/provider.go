// Package credentialprovider provides utilities for obtaining AWS credentials to use.
// Depending on the configuration, it either uses Pod-level or Driver-level credentials.
package credentialprovider

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	k8sv1 "k8s.io/client-go/kubernetes/typed/core/v1"
	k8sstrings "k8s.io/utils/strings"

	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/envprovider"
)

// CredentialFilePerm is the default permissions to be used for credential files.
// It's only readable and writeable by the owner and group.
// Group access is needed as Mountpoint Pod is run as non-root user
const CredentialFilePerm = fs.FileMode(0640)

// CredentialDirPerm is the default permissions to be used for credential directories.
// It's only readable, listable (execute bit), and writeable by the owner and group.
// Group access is needed as Mountpoint Pod is run as non-root user
const CredentialDirPerm = fs.FileMode(0750)

// An AuthenticationSource represents the source (i.e., driver-level or pod-level) where the credentials was obtained.
type AuthenticationSource = string

const (
	// This is when users don't provide a `authenticationSource` option in their volume attributes.
	// We're defaulting to `driver` in this case.
	AuthenticationSourceUnspecified AuthenticationSource = ""
	AuthenticationSourceDriver      AuthenticationSource = "driver"
	AuthenticationSourcePod         AuthenticationSource = "pod"
	AuthenticationSourceSecrets     AuthenticationSource = "secrets"
)

// A Provider provides methods for accessing AWS credentials.
type Provider struct {
	client         k8sv1.CoreV1Interface
	regionFromIMDS func() (string, error)
}

// A ProvideContext contains parameters needed to provide credentials for a volume mount.
//
// Here, [WritePath] and [EnvPath] are used together to provide credential files to Mountpoint.
// The [Provider.Provide] method decides on filenames for credentials (e.g., `token` for driver-level service account token)
// and writes credentials with these filenames in [WritePath], and returns environment variables to pass Mountpoint
// with these filenames in [EnvPath].
// This is due to fact that Mountpoint and the CSI Driver Node Pod - caller of this method - runs with different
// views of the filesystems, and to communicate with each other, the CSI Driver Node Pod uses `hostPath` volume to gain
// access some path visible from both the CSI Driver Node Pod and Mountpoint, and setups files in that volume
// using [WritePath] and returns paths to these files in [EnvPath], so Mountpoint can correctly read these files.
type ProvideContext struct {
	// WritePath is basepath to write credentials into.
	WritePath string
	// EnvPath is basepath to use while creating environment variables to pass Mountpoint.
	EnvPath string

	PodID    string
	VolumeID string

	// The following values are provided from CSI volume context.
	AuthenticationSource AuthenticationSource
	PodNamespace         string
	ServiceAccountTokens string
	ServiceAccountName   string
	// StsRegion is the `stsRegion` parameter passed via volume attribute.
	StsRegion string
	// BucketRegion is the `--region` parameter passed via mount options.
	BucketRegion string
	// Secrets is a map of secret names to their values.
	Secrets map[string]string
}

// SetWriteAndEnvPath sets `WritePath` and `EnvPath` for `ctx`.
func (ctx *ProvideContext) SetWriteAndEnvPath(writePath, envPath string) {
	ctx.WritePath = writePath
	ctx.EnvPath = envPath
}

// A CleanupContext contains parameters needed to clean up credentials after volume unmount.
type CleanupContext struct {
	// WritePath is basepath where credentials previously written into.
	WritePath string
	PodID     string
	VolumeID  string
}

// New creates a new [Provider] with given client.
func New(client k8sv1.CoreV1Interface, regionFromIMDS func() (string, error)) *Provider {
	return &Provider{client, regionFromIMDS}
}

// Provide provides credentials for given context.
// Depending on the configuration, it either returns driver-level or pod-level credentials.
func (c *Provider) Provide(ctx context.Context, provideCtx ProvideContext) (envprovider.Environment, AuthenticationSource, error) {
	authenticationSource := provideCtx.AuthenticationSource
	switch authenticationSource {
	case AuthenticationSourcePod:
		env, err := c.provideFromPod(ctx, provideCtx)
		return env, AuthenticationSourcePod, err
	case AuthenticationSourceSecrets:
		env, err := c.provideFromSecrets(ctx, provideCtx)
		return env, AuthenticationSourceSecrets, err
	case AuthenticationSourceUnspecified, AuthenticationSourceDriver:
		env, err := c.provideFromDriver(provideCtx)
		return env, AuthenticationSourceDriver, err
	default:
		return nil, AuthenticationSourceUnspecified, fmt.Errorf("unknown `authenticationSource`: %s, only `driver` (default option if not specified) and `pod` supported", authenticationSource)
	}
}

// Cleanup cleans any previously created credential files for given context.
func (c *Provider) Cleanup(cleanupCtx CleanupContext) error {
	errPod := c.cleanupFromPod(cleanupCtx)
	errDriver := c.cleanupFromDriver(cleanupCtx)
	return errors.Join(errPod, errDriver)
}

// escapedVolumeIdentifier returns "{podID}-{volumeID}" as a unique identifier for this volume.
// It also escapes slashes to make this identifier path-safe.
func escapedVolumeIdentifier(podID string, volumeID string) string {
	var filename strings.Builder
	// `podID` is a UUID, but escape it to ensure it doesn't contain `/`
	filename.WriteString(k8sstrings.EscapeQualifiedName(podID))
	filename.WriteRune('-')
	// `volumeID` might contain `/`, we need to escape it
	filename.WriteString(k8sstrings.EscapeQualifiedName(volumeID))
	return filename.String()
}
