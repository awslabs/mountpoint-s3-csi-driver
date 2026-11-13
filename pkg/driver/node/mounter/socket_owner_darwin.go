package mounter

// listenerIsSelf always reports true: the driver only runs on Linux.
func listenerIsSelf(string) bool { return true }
