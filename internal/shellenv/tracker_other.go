//go:build !unix

package shellenv

// SetProcessRecordDir is a no-op outside unix.
//
// Windows commands are confined to a kill-on-close job object (see
// ConfigureShellCommand in shell_command_windows.go). The kernel tears the whole
// job down when the handle closes, including on abrupt process death, so there
// is no escaped-descendant class for a persisted record to recover.
func SetProcessRecordDir(string) func() { return func() {} }
