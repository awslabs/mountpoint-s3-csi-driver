// Package awsprofile provides utilities for creating and deleting AWS Profile (i.e., credentials & config files).
package awsprofile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/google/renameio"
)

const (
	awsProfileNameSuffix                = "s3-csi"
	awsProfileConfigFilenameSuffix      = "s3-csi-config"
	awsProfileCredentialsFilenameSuffix = "s3-csi-credentials"
)

// ErrInvalidCredentials is returned when given AWS Credentials contains invalid characters.
var ErrInvalidCredentials = errors.New("aws-profile: Invalid AWS Credentials")

// Profile represents an AWS profile with it's credentials and config filenames.
type Profile struct {
	// Name is the AWS profile name
	Name string
	// ConfigFilename is the name of the AWS config file
	ConfigFilename string
	// CredentialsFilename is the name of the AWS credentials file
	CredentialsFilename string
}

// Credentials represents long-term AWS credentials used to create an AWS Profile.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

// isValid checks if all credential fields contain only printable characters
func (c *Credentials) isValid() bool {
	return isValidCredential(c.AccessKeyID) &&
		isValidCredential(c.SecretAccessKey) &&
		isValidCredential(c.SessionToken)
}

// Settings contains configuration for AWS profile creation and management.
type Settings struct {
	// Basepath is the directory path where AWS profile files will be created
	Basepath string
	// Prefix is prepended to generated filenames for uniqueness
	Prefix string
	// FilePerm specifies the file permissions for created profile files
	FilePerm fs.FileMode
}

// prefixed prepends the Settings prefix to the given suffix
func (s *Settings) prefixed(suffix string) string {
	return s.Prefix + suffix
}

// path joins the basepath with the given filename
func (s *Settings) path(filename string) string {
	return filepath.Join(s.Basepath, filename)
}

// prefixedPath returns the full path for a prefixed filename
func (s *Settings) prefixedPath(filename string) string {
	return s.path(s.prefixed(filename))
}

// Create creates an AWS Profile with credentials and config files from given credentials.
// Created credentials and config files can be clean up with [Cleanup].
func Create(settings Settings, credentials Credentials) (Profile, error) {
	if !credentials.isValid() {
		return Profile{}, ErrInvalidCredentials
	}

	name := settings.prefixed(awsProfileNameSuffix)

	configFilename := settings.prefixed(awsProfileConfigFilenameSuffix)
	configPath := settings.path(configFilename)
	err := writeAWSProfileFile(configPath, configFileContents(name), settings.FilePerm)
	if err != nil {
		return Profile{}, fmt.Errorf("aws-profile: Failed to create config file %s: %v", configPath, err)
	}

	credentialsFilename := settings.prefixed(awsProfileCredentialsFilenameSuffix)
	credentialsPath := settings.path(credentialsFilename)
	err = writeAWSProfileFile(credentialsPath, credentialsFileContents(name, credentials), settings.FilePerm)
	if err != nil {
		return Profile{}, fmt.Errorf("aws-profile: Failed to create credentials file %s: %v", credentialsPath, err)
	}

	return Profile{
		Name:                name,
		ConfigFilename:      configFilename,
		CredentialsFilename: credentialsFilename,
	}, nil
}

// Cleanup cleans up credentials and config files created via [Create].
func Cleanup(settings Settings) error {
	configPath := settings.prefixedPath(awsProfileConfigFilenameSuffix)
	if err := os.Remove(configPath); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("aws-profile: Failed to remove config file %s: %v", configPath, err)
		}
	}

	credentialsPath := settings.prefixedPath(awsProfileCredentialsFilenameSuffix)
	if err := os.Remove(credentialsPath); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("aws-profile: Failed to remove credentials file %s: %v", credentialsPath, err)
		}
	}

	return nil
}

// writeAWSProfileFile safely writes AWS profile content to a file with given permissions
func writeAWSProfileFile(path string, content string, filePerm os.FileMode) error {
	return renameio.WriteFile(path, []byte(content), filePerm)
}

// credentialsFileContents generates the contents for an AWS credentials file
func credentialsFileContents(profile string, credentials Credentials) string {
	var b strings.Builder
	b.Grow(128)
	b.WriteRune('[')
	b.WriteString(profile)
	b.WriteRune(']')
	b.WriteRune('\n')

	b.WriteString("aws_access_key_id=")
	b.WriteString(credentials.AccessKeyID)
	b.WriteRune('\n')

	b.WriteString("aws_secret_access_key=")
	b.WriteString(credentials.SecretAccessKey)
	b.WriteRune('\n')

	if credentials.SessionToken != "" {
		b.WriteString("aws_session_token=")
		b.WriteString(credentials.SessionToken)
		b.WriteRune('\n')
	}

	return b.String()
}

// configFileContents generates the contents for an AWS config file
func configFileContents(profile string) string {
	return fmt.Sprintf("[profile %s]\n", profile)
}

// isValidCredential checks whether given credential file contains any non-printable characters.
func isValidCredential(s string) bool {
	return !strings.ContainsFunc(s, func(r rune) bool { return !unicode.IsPrint(r) })
}
