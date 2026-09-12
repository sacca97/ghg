//go:build !darwin && !linux

package sys

import "os"

func OwnedBy(os.FileInfo, uint32) bool { return true }
func RootOwned(os.FileInfo) bool       { return false }
