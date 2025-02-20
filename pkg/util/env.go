package util

import "os"

func UsePodMounter() bool {
	return os.Getenv("MOUNTER_KIND") == "pod"
}
