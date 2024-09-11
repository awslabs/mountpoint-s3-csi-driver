package driver

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const (
	awsProfileName                = "s3-csi"
	awsProfileConfigFilename      = "s3-csi-config"
	awsProfileCredentialsFilename = "s3-csi-credentials"
	awsProfileFilePerm            = fs.FileMode(0400) // only owner readable
)

// An AWSProfile represents an AWS profile with it's credentials and config files.
type AWSProfile struct {
	Name            string
	ConfigPath      string
	CredentialsPath string
}

// CreateAWSProfile creates an AWS Profile with credentials and config files from given credentials.
// Created credentials and config files can be clean up with `CleanupAWSProfile`.
func CreateAWSProfile(basepath string, accessKeyID string, secretAccessKey string, sessionToken string) (AWSProfile, error) {
	name := awsProfileName

	configPath := filepath.Join(basepath, awsProfileConfigFilename)
	err := writeAWSProfileFile(configPath, configFileContents(name))
	if err != nil {
		return AWSProfile{}, fmt.Errorf("aws-profile: Failed to create config file %s: %v", configPath, err)
	}

	credentialsPath := filepath.Join(basepath, awsProfileCredentialsFilename)
	err = writeAWSProfileFile(credentialsPath, credentialsFileContents(name, accessKeyID, secretAccessKey, sessionToken))
	if err != nil {
		return AWSProfile{}, fmt.Errorf("aws-profile: Failed to create credentials file %s: %v", credentialsPath, err)
	}

	return AWSProfile{
		Name:            name,
		ConfigPath:      configPath,
		CredentialsPath: credentialsPath,
	}, nil
}

// CleanupAWSProfile cleans up credentials and config files created in given `basepath` via `CreateAWSProfile`.
func CleanupAWSProfile(basepath string) error {
	configPath := filepath.Join(basepath, awsProfileConfigFilename)
	if err := os.Remove(configPath); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("aws-profile: Failed to remove config file %s: %v", configPath, err)
		}
	}

	credentialsPath := filepath.Join(basepath, awsProfileCredentialsFilename)
	if err := os.Remove(credentialsPath); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("aws-profile: Failed to credentials config file %s: %v", credentialsPath, err)
		}
	}

	return nil
}

func writeAWSProfileFile(path string, content string) error {
	err := os.WriteFile(path, []byte(content), awsProfileFilePerm)
	if err != nil {
		return err
	}
	// If the given file exists, `os.WriteFile` just truncates it without changing it's permissions,
	// so we need to ensure it always has the correct permissions.
	return os.Chmod(path, awsProfileFilePerm)
}

var credentialFileSpecialValueReplacer = strings.NewReplacer(
	// Newlines and whitespaces
	"\n", "",
	"\t", "",
	"\r", "",
	" ", "",
	// Comment characters
	";", "",
	"#", "",
	// Section start-end characters
	"[", "",
	"]", "",
)

func sanitizeCredentialFileValue(value string) string {
	return credentialFileSpecialValueReplacer.Replace(value)
}

func credentialsFileContents(profile string, accessKeyID string, secretAccessKey string, sessionToken string) string {
	var b strings.Builder
	b.Grow(128)
	b.WriteRune('[')
	b.WriteString(sanitizeCredentialFileValue(profile))
	b.WriteRune(']')
	b.WriteRune('\n')

	b.WriteString("aws_access_key_id=")
	b.WriteString(sanitizeCredentialFileValue(accessKeyID))
	b.WriteRune('\n')

	b.WriteString("aws_secret_access_key=")
	b.WriteString(sanitizeCredentialFileValue(secretAccessKey))
	b.WriteRune('\n')

	if sessionToken != "" {
		b.WriteString("aws_session_token=")
		b.WriteString(sanitizeCredentialFileValue(sessionToken))
		b.WriteRune('\n')
	}

	return b.String()
}

func configFileContents(profile string) string {
	return fmt.Sprintf("[profile %s]\n", sanitizeCredentialFileValue(profile))
}
