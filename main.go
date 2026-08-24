package main

import (
	_ "embed"
	"fmt"
	"os"

	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/provider/artifact"
	"github.com/codefly-dev/provider-unleash/internal/provider"
)

//go:embed provider.codefly.yaml
var manifestBytes []byte

func main() {
	server, err := newServer()
	if err != nil {
		fmt.Fprintln(os.Stderr, "provider-unleash:", err)
		os.Exit(1)
	}
	agents.Serve(agents.PluginRegistration{Provider: server})
}

func newServer() (*provider.Server, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(executable + artifact.InstalledDescriptorSuffix)
	if err != nil {
		return nil, fmt.Errorf("provider must run from an installed artifact layout: %w", err)
	}
	descriptor, err := artifact.ParseDescriptor(data)
	if err != nil {
		return nil, err
	}
	return provider.NewServer(manifestBytes, provider.Identity{
		Publisher: descriptor.Publisher, Name: descriptor.Name, Version: descriptor.Version,
		ArtifactDigest: descriptor.ArtifactDigest, ManifestDigest: descriptor.ManifestDigest,
	})
}
