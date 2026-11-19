# Binary distribution notices

`modules.txt` records the Go module versions linked into the four release
targets. The release build compares it with `go version -m` on each binary.
Use `CGO_ENABLED=0 GOOS=<os> GOARCH=<arch> go list -deps ./cmd/crosshall` to
review the package graph when updating a dependency. The license file names
refer to the exact checked module sources selected by `go.mod` and `go.sum`.

The Go standard library is covered by `go-LICENSE` and `go-PATENTS` from
Go 1.27.4. Its vendored `golang.org/x/net/dns/dnsmessage` package carries
the notices copied from that toolchain's `src/vendor/golang.org/x/net`.
Review these notices when upgrading Go.

The release archive contains the project's LICENSE and NOTICE and an
assembled copy of each listed upstream notice. The SQLite and libc notices
cover the underlying generated C translations, including SQLite's
public-domain dedication and musl's MIT terms. The full upstream notices
also describe optional packages such as sqlite-vec and dependencies that are
used only by upstream tests or tooling.
