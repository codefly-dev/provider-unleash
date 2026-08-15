package provider

import (
	"context"

	providerv0 "github.com/codefly-dev/core/generated/go/codefly/services/provider/v0"
)

func (s *Server) Validate(_ context.Context, request *providerv0.ValidateRequest) (*providerv0.ValidateResponse, error) {
	diagnostics := validateRawInputs(request.GetContext().GetInput())
	diagnostics = append(diagnostics, parseInputs(request.GetContext().GetInput()).validate()...)
	return &providerv0.ValidateResponse{Valid: !hasErrors(diagnostics), Diagnostics: diagnostics}, nil
}
