//go:build !testvendorhome

package link

func sshTestOverride() string { return "" }

func AppendSSHTestEnv(env []string) []string { return env }
