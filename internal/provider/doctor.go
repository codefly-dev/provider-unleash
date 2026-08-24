package provider

import (
	"context"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	providerv0 "github.com/codefly-dev/core/generated/go/codefly/services/provider/v0"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *Server) Doctor(ctx context.Context, request *providerv0.DoctorRequest) (*providerv0.DoctorResponse, error) {
	if s.host == nil {
		return nil, status.Error(codes.FailedPrecondition, "provider host callback channel is not attached")
	}
	origins, err := endpointOrigins(request.GetContext())
	if err != nil {
		return &providerv0.DoctorResponse{Healthy: false}, nil
	}
	if err := s.checkpoint(ctx, request.GetContext(), "doctor", "", ""); err != nil {
		return nil, err
	}
	marker := ownershipMarker(request.GetContext().GetOffline().GetBinding())
	planned, err := s.plannedRequest("token.get", origins["admin"], map[string]*providerv0.PublicValue{
		"resource_id": publicString(tokenName(marker, "server")),
	}, nil, nil, "")
	if err != nil {
		return nil, err
	}
	response, err := s.execute(ctx, request.GetContext(), origins["admin"], planned, "doctor-token")
	if err != nil {
		return nil, err
	}
	diagnostic := responseDiagnostic(response)
	if diagnostic != nil {
		return &providerv0.DoctorResponse{Healthy: false, Diagnostics: []*basev0.FailureDiagnostic{diagnostic}}, nil
	}
	return &providerv0.DoctorResponse{Healthy: true}, nil
}
