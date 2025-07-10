// Package credentialprovider provides utilities for obtaining AWS credentials to use.
// Depending on the configuration, it either uses Pod-level or Driver-level credentials.
//
//go:generate mockgen -source=provider.go -destination=./mocks/mock_provider.go -package=mock_credentialprovider
package credentialprovider

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	k8sv1 "k8s.io/client-go/kubernetes/typed/core/v1"
	k8sstrings "k8s.io/utils/strings"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/envprovider"
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

// MountKind represents the type of mount being used
type MountKind string

const (
	MountKindUnspecified MountKind = ""
	// MountKindPod indicates the mount is managed by PodMounter
	MountKindPod MountKind = "pod"
	// MountKindSystemd indicates the mount is managed by systemd
	MountKindSystemd MountKind = "systemd"
)

// A Provider provides methods for accessing AWS credentials.
type Provider struct {
	client         k8sv1.CoreV1Interface
	regionFromIMDS func() (string, error)
}

// ProviderInterface
type ProviderInterface interface {
	Provide(ctx context.Context, provideCtx ProvideContext) (envprovider.Environment, AuthenticationSource, error)
	Cleanup(cleanupCtx CleanupContext) error
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

	// WorkloadPodID is workload Pod UID
	WorkloadPodID string
	// MountpointPodID is Mountpoint Pod UID
	MountpointPodID string
	VolumeID        string

	// MountKind indicates whether the mount is managed by systemd or pod mounter
	MountKind MountKind

	// The following values are provided from CSI volume context.
	AuthenticationSource     AuthenticationSource
	PodNamespace             string
	ServiceAccountTokens     string
	ServiceAccountName       string
	ServiceAccountEKSRoleARN string
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

// SetServiceAccountEKSRoleARN sets `ServiceAccountEKSRoleARN` for `ctx`.
func (ctx *ProvideContext) SetServiceAccountEKSRoleARN(roleArn string) {
	ctx.ServiceAccountEKSRoleARN = roleArn
}

// SetMountpointPodID sets `MountpointPodID` for `ctx`.
func (ctx *ProvideContext) SetMountpointPodID(mpPodUID string) {
	ctx.MountpointPodID = mpPodUID
}

// SetAsSystemDMountpoint marks this context as managed by systemd instead of pod mounter.
func (ctx *ProvideContext) SetAsSystemDMountpoint() {
	ctx.MountKind = MountKindSystemd
}

// SetAsPodMountpoint marks this context as managed by pod mounter instead of systemd.
func (ctx *ProvideContext) SetAsPodMountpoint() {
	ctx.MountKind = MountKindPod
}

// IsSystemDMountpoint returns true if this context is managed by systemd mounter.
func (ctx *ProvideContext) IsSystemDMountpoint() bool {
	return ctx.MountKind == MountKindSystemd
}

// IsPodMountpoint returns true if this context is managed by pod mounter.
func (ctx *ProvideContext) IsPodMountpoint() bool {
	return ctx.MountKind == MountKindPod
}

// GetCredentialPodID returns the appropriate Pod ID for credential operations.
// When MountpointPodID is not empty string it returns MountpointPodID (for pod mounter mounts),
// otherwise returns workload Pod ID (for systemd mounts).
func (p *ProvideContext) GetCredentialPodID() string {
	if p.MountpointPodID != "" {
		return p.MountpointPodID
	}
	return p.WorkloadPodID
}

// A CleanupContext contains parameters needed to clean up credentials after volume unmount.
type CleanupContext struct {
	// WritePath is basepath where credentials previously written into.
	WritePath string
	PodID     string
	VolumeID  string

	// MountKind indicates whether the mount is managed by systemd or pod mounter
	MountKind MountKind
}

// SetAsSystemDMountpoint marks this context as managed by systemd instead of pod mounter.
func (ctx *CleanupContext) SetAsSystemDMountpoint() {
	ctx.MountKind = MountKindSystemd
}

// SetAsPodMountpoint marks this context as managed by pod mounter instead of systemd.
func (ctx *CleanupContext) SetAsPodMountpoint() {
	ctx.MountKind = MountKindPod
}

// IsSystemDMountpoint returns true if this context is managed by systemd mounter.
func (ctx *CleanupContext) IsSystemDMountpoint() bool {
	return ctx.MountKind == MountKindSystemd
}

// IsPodMountpoint returns true if this context is managed by pod mounter.
func (ctx *CleanupContext) IsPodMountpoint() bool {
	return ctx.MountKind == MountKindPod
}

// New creates a new [Provider] with given client.
func New(client k8sv1.CoreV1Interface, regionFromIMDS func() (string, error)) *Provider {
	return &Provider{client, regionFromIMDS}
}

// Provide provides credentials for given context.
// Depending on the configuration, it either returns driver-level or pod-level credentials.
func (c *Provider) Provide(ctx context.Context, provideCtx ProvideContext) (envprovider.Environment, AuthenticationSource, error) {
	if provideCtx.MountKind == MountKindUnspecified {
		return nil, "", fmt.Errorf("MountKind must be specified on credential ProvideContext struct.")
	}

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
	if cleanupCtx.MountKind == MountKindUnspecified {
		return fmt.Errorf("MountKind must be specified on credential CleanupContext struct.")
	}

	errPod := c.cleanupFromPod(cleanupCtx)
	errDriver := c.cleanupFromDriver(cleanupCtx)
	return errors.Join(errPod, errDriver)
}

// cleanupToken removes a token file from the filesystem. If the file doesn't exist, it's not considered
// an error. This helper is used by both cleanupFromPod and cleanupFromDriver.
func (c *Provider) cleanupToken(basePath, tokenName string) error {
	tokenPath := filepath.Join(basePath, tokenName)
	err := os.Remove(tokenPath)
	if err != nil && errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
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
