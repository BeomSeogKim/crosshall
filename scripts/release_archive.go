//go:build ignore

// The release script uses this helper to avoid host-specific tar and gzip headers.
package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

func main() {
	if len(os.Args) != 7 {
		fmt.Fprintln(os.Stderr, "usage: release_archive <out> <binary> <license> <notice> <third-party-notices> <commit>")
		os.Exit(2)
	}
	out, inputs := os.Args[1], os.Args[2:]
	if err := archive(out, inputs); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func archive(out string, inputs []string) error {
	file, err := os.Create(out)
	if err != nil {
		return err
	}
	defer file.Close()

	sum := sha256.New()
	compressed := gzip.NewWriter(io.MultiWriter(file, sum))
	tape := tar.NewWriter(compressed)
	names := []string{"crosshall", "LICENSE", "NOTICE", "THIRD_PARTY_NOTICES.txt", "COMMIT"}
	for i, name := range names {
		mode := int64(0644)
		if name == "crosshall" {
			mode = 0755
		}
		if err := appendFile(tape, inputs[i], name, mode); err != nil {
			return err
		}
	}
	if err := tape.Close(); err != nil {
		return err
	}
	if err := compressed.Close(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	checksum := fmt.Appendf(nil, "%x  %s\n", sum.Sum(nil), filepath.Base(out))
	return os.WriteFile(out+".sha256", checksum, 0644)
}

func appendFile(tape *tar.Writer, path, name string, mode int64) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	header := &tar.Header{Name: name, Mode: mode, Size: info.Size(),
		ModTime: time.Unix(0, 0).UTC(), Format: tar.FormatUSTAR}
	if err := tape.WriteHeader(header); err != nil {
		return err
	}
	_, err = io.Copy(tape, file)
	return err
}
