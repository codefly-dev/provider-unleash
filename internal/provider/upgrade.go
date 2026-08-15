package provider

import (
	"context"

	providerv0 "github.com/codefly-dev/core/generated/go/codefly/services/provider/v0"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *Server) UpgradeState(_ context.Context, request *providerv0.UpgradeStateRequest) (*providerv0.UpgradeStateResponse, error) {
	return nil, status.Errorf(codes.FailedPrecondition, "no state upgrade is defined from %d to %d", request.GetFromVersion(), request.GetToVersion())
}
