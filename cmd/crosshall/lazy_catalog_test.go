package main

import (
	"os"
	"os/exec"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/hook/manifests"
)

// Every hook starts this binary, so package initialization must not decode
// the manifest catalog: a hook that never asks a capability would pay for
// it. A fresh process of this test binary links every package the binary
// does; it checks the catalog before anything else runs.
func TestPackageInitDoesNotDecodeTheManifestCatalog(t *testing.T) {
	if os.Getenv("CROSSHALL_TEST_INIT_CHILD") == "1" {
		if manifests.Decoded() {
			t.Fatal("package initialization decoded the manifest catalog")
		}
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestPackageInitDoesNotDecodeTheManifestCatalog$", "-test.count=1")
	cmd.Env = append(os.Environ(), "CROSSHALL_TEST_INIT_CHILD=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
}
