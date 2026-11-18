package cli

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
)

func TestOwnerAdapterEvidenceSendsTheFileWithItsDigest(t *testing.T) {
	body := `{"vendor":"claude","vendor_version":"2.1.327","platform":"darwin-arm64","mode":"print","manifest_rev":"claude-1",` +
		`"run_at_ms":1,"results":[{"cap":"enroll","level":"tested","check":"hint in the first request"}]}`
	path := filepath.Join(t.TempDir(), "evidence.json")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	ft := &fakeTransport{result: okResult(t, domain.AdapterEvidenceResult{Vendor: "claude", Rows: 1})}
	env := Env{Transport: ft, OwnerGate: func() string { return "" }, NewKey: func() string { return "k" }}
	if _, stderr, code := run(t, env, "owner", "adapter", "evidence", path); code != core.ExitOK {
		t.Fatalf("exit %d %s", code, stderr)
	}
	cmd, ok := ft.calls[0].Command.(domain.AdapterEvidence)
	if !ok || !ft.calls[0].Owner || ft.calls[0].OpKey == "" || cmd.VendorVersion != "2.1.327" || len(cmd.Results) != 1 ||
		cmd.ResultRef != fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(body))) {
		t.Fatalf("call %+v", ft.calls[0])
	}
	trailing := filepath.Join(t.TempDir(), "trailing.json")
	if err := os.WriteFile(trailing, []byte(body+"\n{}"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"owner", "adapter", "evidence"}, {"owner", "adapter", "evidence", filepath.Join(t.TempDir(), "missing.json")},
		{"owner", "adapter", "evidence", trailing}} {
		if _, _, code := run(t, env, args...); code == core.ExitOK {
			t.Errorf("%v accepted", args)
		}
	}
}
