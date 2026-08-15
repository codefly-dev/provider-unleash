package provider

import (
	"fmt"

	providerv0 "github.com/codefly-dev/core/generated/go/codefly/services/provider/v0"
	"github.com/codefly-dev/core/provider/manifest"
	"github.com/codefly-dev/core/provider/sdk"
)

const (
	diagnosticNamespace  = "provider.unleash."
	diagnosticInvalid    = diagnosticNamespace + "invalid-input"
	diagnosticConflict   = diagnosticNamespace + "unmanaged-conflict"
	diagnosticIncomplete = diagnosticNamespace + "incomplete-observation"
	diagnosticUnknown    = diagnosticNamespace + "outcome-unknown"
	diagnosticRemote     = diagnosticNamespace + "remote-error"
	stateVersion         = 1
)

var diagnosticCodes = []string{
	diagnosticInvalid,
	diagnosticConflict,
	diagnosticIncomplete,
	diagnosticUnknown,
	diagnosticRemote,
}

type Identity struct {
	Publisher      string
	Name           string
	Version        string
	ArtifactDigest string
	ManifestDigest string
}

type Host = providerv0.ProviderHostClient

type Option func(*Server)

func WithHost(host Host) Option {
	return func(server *Server) { server.host = host }
}

type Server struct {
	*sdk.Base
	manifest       *manifest.Manifest
	host           Host
	artifactDigest string
	manifestDigest string
	catalogDigest  string
}

var _ providerv0.ProviderServer = (*Server)(nil)

func NewServer(manifestBytes []byte, identity Identity, options ...Option) (*Server, error) {
	providerManifest, err := manifest.Load(manifestBytes)
	if err != nil {
		return nil, fmt.Errorf("packaged manifest is invalid: %w", err)
	}
	digest, err := providerManifest.Digest()
	if err != nil {
		return nil, err
	}
	if identity.ManifestDigest != digest {
		return nil, fmt.Errorf("artifact manifest digest does not match packaged manifest")
	}
	if identity.Publisher != providerManifest.Agent.Publisher || identity.Name != providerManifest.Agent.Name || identity.Version != providerManifest.Agent.Version {
		return nil, fmt.Errorf("artifact identity does not match packaged manifest agent")
	}
	catalog, err := buildCatalog(providerManifest)
	if err != nil {
		return nil, err
	}
	base, err := sdk.NewBase(&providerv0.GetProviderInformationResponse{
		Artifact: &providerv0.AgentArtifactIdentity{
			Publisher: identity.Publisher, Name: identity.Name, Version: identity.Version,
			ArtifactDigest: identity.ArtifactDigest, ManifestDigest: identity.ManifestDigest,
		},
		Catalog: catalog,
		Capabilities: &providerv0.ProviderCapabilities{
			SupportsImport: true, SupportsDelete: true, SupportsStateUpgrade: false,
		},
		Readiness: &providerv0.ProviderReadiness{ProductionObserve: true, ProductionMutation: false},
	})
	if err != nil {
		return nil, err
	}
	server := &Server{
		Base: base, manifest: providerManifest, artifactDigest: identity.ArtifactDigest,
		manifestDigest: identity.ManifestDigest, catalogDigest: catalog.GetDigest(),
	}
	for _, option := range options {
		option(server)
	}
	return server, nil
}

func buildCatalog(providerManifest *manifest.Manifest) (*providerv0.RuntimeCatalog, error) {
	local := &manifest.Catalog{
		SchemaVersion: providerManifest.SchemaVersion, ProtocolVersion: providerManifest.ProtocolVersion,
		StateSchemaVersions: append([]uint32(nil), providerManifest.StateSchemaVersions...),
		DiagnosticCodes:     append([]string(nil), diagnosticCodes...),
	}
	runtime := &providerv0.RuntimeCatalog{
		ProtocolVersion: providerManifest.ProtocolVersion, ManifestSchemaVersion: providerManifest.SchemaVersion,
		StateSchemaVersions: append([]uint32(nil), providerManifest.StateSchemaVersions...),
		DiagnosticCodes:     append([]string(nil), diagnosticCodes...),
	}
	for _, request := range providerManifest.Requests {
		digest, err := manifest.RequestDescriptorDigest(request)
		if err != nil {
			return nil, err
		}
		local.Requests = append(local.Requests, manifest.CatalogRequest{ID: request.ID, Digest: digest})
		runtime.Requests = append(runtime.Requests, &providerv0.RuntimeCatalogRequest{Id: request.ID, Digest: digest})
	}
	for _, resource := range providerManifest.ResourceTypes {
		actions := append([]string(nil), resource.Actions...)
		local.ResourceTypes = append(local.ResourceTypes, manifest.CatalogResource{ID: resource.ID, Actions: actions})
		runtime.ResourceTypes = append(runtime.ResourceTypes, &providerv0.RuntimeCatalogResource{Id: resource.ID, Actions: actions})
	}
	for _, projection := range providerManifest.Projections {
		local.ProjectionContracts = append(local.ProjectionContracts, projection.Contract)
		runtime.ProjectionContracts = append(runtime.ProjectionContracts, projection.Contract)
	}
	digest, err := local.Digest()
	if err != nil {
		return nil, err
	}
	runtime.Digest = digest
	if _, err := providerManifest.AdmitRuntimeCatalog(runtime); err != nil {
		return nil, fmt.Errorf("derived runtime catalog is not admissible: %w", err)
	}
	return runtime, nil
}
