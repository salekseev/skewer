// +build !linux

package sys

type NotLinuxError struct{}

var CapabilitiesSupported bool = false

func (e NotLinuxError) Error() string {
	return "Only available on Linux"
}

func SetNonDumpable() error {
	return NotLinuxError{}
}

func NeedFixLinuxPrivileges(uid, gid string) (bool, error) {
	return false, nil
}

func FixLinuxPrivileges(uid string, gid string) error {
	return nil
}

func DropNetBind() error {
	return nil
}

func GetCaps() string {
	return ""
}

func CanReadAuditLogs() bool {
	return false
}

func Predrop() error {
	return nil
}

func NoNewPriv() error {
	return nil
}
