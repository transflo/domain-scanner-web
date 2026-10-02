//go:build !linux

package housekeeping

// StatfsDisk is only implemented on Linux (the container platform); elsewhere the disk guard is off.
func StatfsDisk(string) (free, total int64, ok bool) { return 0, 0, false }
