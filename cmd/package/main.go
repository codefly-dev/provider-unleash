package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/codefly-dev/core/provider/artifact"
)

func main() {
	binary := flag.String("binary", "provider-unleash", "provider binary")
	manifestPath := flag.String("manifest", "provider.codefly.yaml", "provider manifest")
	out := flag.String("out", "dist", "artifact directory")
	flag.Parse()
	if err := run(*binary, *manifestPath, *out); err != nil {
		fmt.Fprintln(os.Stderr, "package:", err)
		os.Exit(1)
	}
}

func run(binaryPath, manifestPath, out string) error {
	binaryBytes, err := os.ReadFile(binaryPath)
	if err != nil {
		return err
	}
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	descriptor, err := artifact.BuildDescriptor(filepath.Base(binaryPath), binaryBytes, manifestBytes)
	if err != nil {
		return err
	}
	descriptorBytes, err := artifact.MarshalDescriptor(descriptor)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	for name, contents := range map[string][]byte{
		filepath.Base(binaryPath): binaryBytes,
		"provider.codefly.yaml":   manifestBytes,
		"provider.artifact.json":  descriptorBytes,
	} {
		mode := os.FileMode(0o644)
		if name == filepath.Base(binaryPath) {
			mode = 0o755
		}
		if err := os.WriteFile(filepath.Join(out, name), contents, mode); err != nil {
			return err
		}
	}
	_, err = artifact.VerifyLayout(out, nil)
	return err
}
