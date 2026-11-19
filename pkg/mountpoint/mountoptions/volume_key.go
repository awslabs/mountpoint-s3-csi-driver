package mountoptions

import (
	"crypto/sha256"
	"encoding/hex"
)

// errorFileSuffix is appended to a volume's key to name the file the mounter writes when a
// Mountpoint fails to start.
const errorFileSuffix = ".error"

// VolumeKey returns the name to use for a volume's files and directories on the node: the hex
// SHA-256 of the PV name, always 64 characters from [0-9a-f].
//
// Kubernetes allows PV names up to 253 bytes and Linux allows a file name up to 255, so the PV name
// plus any suffix (".meta.json.tmp", ".error") can exceed the limit. A fixed-length key with a fixed
// character set also cannot collide with a name the driver reserves, such as "mount.sock".
func VolumeKey(volumeID string) string {
	sum := sha256.Sum256([]byte(volumeID))
	return hex.EncodeToString(sum[:])
}

// ErrorFileName returns the name of the error file for a volume, relative to the comm directory.
// The mounter writes it and s3-csi-node reads it, so both must use this function.
func ErrorFileName(volumeID string) string {
	return VolumeKey(volumeID) + errorFileSuffix
}
