// Package conformance drives a real vendor agent binary through the
// Crosshall adapter in a temporary home, against a local stand-in for the
// model endpoint, and records what was observed as evidence.
//
// It is a local command, not part of CI, because CI has no vendor binary:
//
//	go test -tags conformance -run Claude -v ./test/conformance/
//
// Every home, configuration directory and state root is temporary. The run
// needs no account, credential or network beyond loopback. It never reads
// or writes the real ~/.claude. CROSSHALL_CONFORMANCE_OUT names a directory
// to keep the evidence file in: an absolute, existing, unlinked directory
// outside the login home, which is resolved from the account, not HOME.
package conformance
