//go:build !testvendorhome

package dispatch

func dispatcherSubcommand() string { return "__dispatch" }

func appendTestDispatcherEnv(env []string) []string { return env }

func RunTestDispatcherGuardian([]string) (bool, int) { return false, 0 }
