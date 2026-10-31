//go:build !darwin

package profiles

// Instances lists the Claude app's running instances; the app runs only on
// macOS.
func Instances(mainDir string) map[int]string { return nil }

// InstancesChecked is Instances, with ok false when the processes couldn't
// be listed.
func InstancesChecked(mainDir string) (map[int]string, bool) { return nil, true }
