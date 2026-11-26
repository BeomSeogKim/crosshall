//go:build testvendorhome

package link

import "os"

const sshOverrideEnv = "CROSSHALL_LINK_SSH"

func sshTestOverride() string { return os.Getenv(sshOverrideEnv) }

func AppendSSHTestEnv(env []string) []string {
	if value := sshTestOverride(); value != "" {
		env = append(env, sshOverrideEnv+"="+value)
	}
	return env
}
