package telemetrysend

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// The binary every hook starts links no TLS or HTTP client: on darwin
// crypto/x509 loads the Security framework at every process start. Hooks,
// member commands and the client never reach the sender.
func TestImportGraph(t *testing.T) {
	const module = "github.com/BeomSeogKim/crosshall/"
	for _, goos := range []string{"darwin", "linux"} {
		deps := func(pkg string) []string {
			cmd := exec.Command("go", "list", "-deps", module+pkg)
			cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH=arm64")
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("go list %s: %v", pkg, err)
			}
			return strings.Fields(string(out))
		}
		binary := deps("cmd/crosshall")
		if !slices.Contains(binary, module+"internal/telemetrysend") {
			t.Fatalf("%s: the binary does not link the sender", goos)
		}
		for _, banned := range []string{"net/http", "crypto/tls", "crypto/x509"} {
			if slices.Contains(binary, banned) {
				t.Errorf("%s: cmd/crosshall depends on %s", goos, banned)
			}
		}
		for _, pkg := range []string{"internal/hook", "internal/cli", "internal/client"} {
			if slices.Contains(deps(pkg), module+"internal/telemetrysend") {
				t.Errorf("%s: %s depends on the sender", goos, pkg)
			}
		}
	}
}
