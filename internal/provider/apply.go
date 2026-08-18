package provider

import (
	"context"
	"fmt"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	providerv0 "github.com/codefly-dev/core/generated/go/codefly/services/provider/v0"
	"github.com/codefly-dev/core/provider/sdk"
	providerstate "github.com/codefly-dev/core/provider/state"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func (s *Server) ApplyAction(ctx context.Context, request *providerv0.ApplyActionRequest) (*providerv0.ApplyActionResponse, error) {
	if s.host == nil {
		return nil, status.Error(codes.FailedPrecondition, "provider host callback channel is not attached")
	}
	action := request.GetAction()
	if action == nil || !actionInPlan(request.GetPlan(), action) {
		return nil, status.Error(codes.FailedPrecondition, "apply action is not part of the bound plan")
	}
	switch action.GetType() {
	case providerv0.ActionType_ACTION_TYPE_CREATE,
		providerv0.ActionType_ACTION_TYPE_UPDATE,
		providerv0.ActionType_ACTION_TYPE_IMPORT,
		providerv0.ActionType_ACTION_TYPE_DELETE:
		return s.applyMutation(ctx, request)
	case providerv0.ActionType_ACTION_TYPE_PROJECT_OUTPUT:
		return s.applyOutput(ctx, request)
	case providerv0.ActionType_ACTION_TYPE_NO_OP:
		return s.applyInert(request), nil
	default:
		return nil, status.Errorf(codes.FailedPrecondition, "action type %s cannot be applied", action.GetType())
	}
}

func (s *Server) applyMutation(ctx context.Context, request *providerv0.ApplyActionRequest) (*providerv0.ApplyActionResponse, error) {
	if request.GetPriorCheckpoint().GetDelivery() == providerv0.DeliveryState_DELIVERY_STATE_SENT_OUTCOME_UNKNOWN {
		value := diagnostic(basev0.FailureDiagnostic_ERROR, diagnosticUnknown, "a prior mutation may have completed; observe before retrying")
		return &providerv0.ApplyActionResponse{
			Receipt: s.receipt(request, &providerv0.ExecuteRequestResponse{
				Delivery:  providerv0.DeliveryState_DELIVERY_STATE_NOT_SENT,
				Certainty: providerv0.OutcomeCertainty_OUTCOME_CERTAINTY_UNCERTAIN,
			}, nil, nil, []*basev0.FailureDiagnostic{value}),
			NextState: s.unchangedState(request),
		}, nil
	}
	providerContext := request.GetContext()
	origins, err := endpointOrigins(providerContext)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	in := parseInputs(providerContext.GetOffline().GetInput())
	if in.Intent == "" {
		in.Intent = "apply"
	}
	marker := ownershipMarker(providerContext.GetOffline().GetBinding())
	action := request.GetAction()
	descriptorID, path, body, err := mutationRequest(action, in, marker)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	key := idempotencyKey(providerContext, action)
	if err := s.checkpoint(ctx, providerContext, action.GetActionId(), key, action.GetProspectiveRemoteId()); err != nil {
		return nil, err
	}
	planned, err := s.plannedRequest(descriptorID, origins["admin"], path, nil, body, key)
	if err != nil {
		return nil, err
	}
	response, err := s.execute(ctx, providerContext, origins["admin"], planned, "apply-"+action.GetActionId())
	if err != nil {
		return nil, err
	}
	if value := responseDiagnostic(response); value != nil {
		return &providerv0.ApplyActionResponse{
			Receipt:   s.receipt(request, response, nil, nil, []*basev0.FailureDiagnostic{value}),
			NextState: s.unchangedState(request),
		}, nil
	}
	fields, err := sdk.DecodeFilteredResponse(response)
	if err != nil {
		return nil, err
	}
	var captures []*providerv0.OpaqueReference
	if action.GetActionId() == "token-server-create" || action.GetActionId() == "token-browser-create" {
		if len(response.GetCaptures()) != 1 {
			return nil, status.Error(codes.FailedPrecondition, "token creation did not produce exactly one opaque capture")
		}
		reference, captureErr := sdk.HandleCaptureResult(response.GetCaptures()[0])
		if captureErr != nil {
			return nil, captureErr
		}
		if reference.GetPurpose() != providerv0.CredentialPurpose_CREDENTIAL_PURPOSE_RUNTIME {
			return nil, status.Error(codes.FailedPrecondition, "token capture is not runtime-scoped")
		}
		captures = append(captures, reference)
	}
	next := s.nextState(request, captures)
	if action.GetActionId() == "token-server-create" || action.GetActionId() == "token-browser-create" {
		identity := filteredFields(fields).tokenIdentity("$")
		if identity == "" {
			return nil, status.Error(codes.FailedPrecondition, "token creation returned no durable remote identity")
		}
		identityField := "server_remote_identity"
		if action.GetActionId() == "token-browser-create" {
			identityField = "browser_remote_identity"
		}
		next.GetV1().ProviderOwnedFields[identityField] = publicString(identity)
	}
	if err := validateState(next); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "constructed provider state is invalid: %v", err)
	}
	return &providerv0.ApplyActionResponse{Receipt: s.receipt(request, response, fields, captures, nil), NextState: next}, nil
}

func mutationRequest(action *providerv0.PlanAction, in inputs, marker string) (string, map[string]*providerv0.PublicValue, map[string]*providerv0.PublicValue, error) {
	projectPath := map[string]*providerv0.PublicValue{"resource_id": publicString(in.ProjectID)}
	applicationPath := map[string]*providerv0.PublicValue{"resource_id": publicString(in.ApplicationID)}
	switch action.GetActionId() {
	case "project-create":
		return "project.create", nil, map[string]*providerv0.PublicValue{
			"id": publicString(in.ProjectID), "name": publicString(in.ProjectName),
			"description": publicString(marker), "mode": publicString("open"),
		}, nil
	case "project-update", "project-import":
		if action.GetActionId() == "project-import" && action.GetRemoteIdentity().GetRemoteId() != in.ProjectID {
			return "", nil, nil, fmt.Errorf("import does not name the exact declared project")
		}
		return "project.update", projectPath, map[string]*providerv0.PublicValue{
			"name": publicString(in.ProjectName), "description": publicString(marker), "mode": publicString("open"),
		}, nil
	case "project-delete":
		if action.GetOwnership() != providerv0.Ownership_OWNERSHIP_OWNED || action.GetRemoteIdentity().GetRemoteId() != in.ProjectID {
			return "", nil, nil, fmt.Errorf("delete does not name the exact owned project")
		}
		return "project.delete", projectPath, nil, nil
	case "environment-create":
		return "environment.create", nil, map[string]*providerv0.PublicValue{
			"name": publicString(in.EnvironmentID), "type": publicString(in.EnvironmentType), "enabled": publicBool(true),
		}, nil
	case "project-environment-create":
		return "project-environment.create", projectPath, map[string]*providerv0.PublicValue{"environment": publicString(in.EnvironmentID)}, nil
	case "application-create", "application-update", "application-import":
		if action.GetActionId() == "application-import" && action.GetRemoteIdentity().GetRemoteId() != in.ApplicationID {
			return "", nil, nil, fmt.Errorf("import does not name the exact declared application")
		}
		descriptor := "application.create"
		if action.GetActionId() != "application-create" {
			descriptor = "application.update"
		}
		return descriptor, applicationPath, map[string]*providerv0.PublicValue{
			"strategies": publicStrings(), "url": publicString(applicationOwnershipURL(marker)),
		}, nil
	case "application-delete":
		if action.GetOwnership() != providerv0.Ownership_OWNERSHIP_OWNED || action.GetRemoteIdentity().GetRemoteId() != in.ApplicationID {
			return "", nil, nil, fmt.Errorf("delete does not name the exact owned application")
		}
		return "application.delete", applicationPath, nil, nil
	case "token-server-create":
		return tokenCreateRequest(in, marker, "server", "backend")
	case "token-browser-create":
		return tokenCreateRequest(in, marker, "browser", "frontend")
	default:
		return "", nil, nil, fmt.Errorf("unknown mutation action %q", action.GetActionId())
	}
}

func tokenCreateRequest(in inputs, marker, kind, tokenType string) (string, map[string]*providerv0.PublicValue, map[string]*providerv0.PublicValue, error) {
	return "token.create", nil, map[string]*providerv0.PublicValue{
		"type": publicString(tokenType), "tokenName": publicString(tokenName(marker, kind)),
		"environment": publicString(in.EnvironmentID), "projects": publicStrings(in.ProjectID),
	}, nil
}

func (s *Server) applyOutput(ctx context.Context, request *providerv0.ApplyActionRequest) (*providerv0.ApplyActionResponse, error) {
	proposal := request.GetAction().GetOutput()
	if proposal == nil || (proposal.GetContract() != featureFlagsContract && proposal.GetContract() != featureFlagsBrowserContract) {
		return nil, status.Error(codes.FailedPrecondition, "project output is not an admitted feature flags contract")
	}
	response, err := s.host.ProposeOutput(ctx, &providerv0.ProposeOutputRequest{
		Operation: request.GetContext().GetOperation(), Proposal: proposal,
	})
	if err != nil {
		return nil, err
	}
	if !response.GetDurable() {
		return nil, status.Error(codes.FailedPrecondition, "host did not durably commit the feature flags output")
	}
	next := s.nextState(request, nil)
	next.GetV1().OutputContract = proposal.GetContract()
	next.GetV1().OutputGeneration = response.GetGeneration()
	next.GetV1().OutputDigest = response.GetDigest()
	if err := validateState(next); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "constructed provider state is invalid: %v", err)
	}
	return &providerv0.ApplyActionResponse{Receipt: s.receipt(request, nil, nil, nil, nil), NextState: next}, nil
}

func (s *Server) applyInert(request *providerv0.ApplyActionRequest) *providerv0.ApplyActionResponse {
	return &providerv0.ApplyActionResponse{Receipt: s.receipt(request, nil, nil, nil, nil), NextState: s.unchangedState(request)}
}

func actionInPlan(plan *providerv0.OrderedPlan, action *providerv0.PlanAction) bool {
	for _, candidate := range plan.GetActions() {
		if proto.Equal(candidate, action) {
			return true
		}
	}
	return false
}

func (s *Server) receipt(request *providerv0.ApplyActionRequest, response *providerv0.ExecuteRequestResponse, safe map[string]*providerv0.PublicValue, captures []*providerv0.OpaqueReference, diagnostics []*basev0.FailureDiagnostic) *providerv0.ActionReceipt {
	delivery := providerv0.DeliveryState_DELIVERY_STATE_NOT_SENT
	certainty := providerv0.OutcomeCertainty_OUTCOME_CERTAINTY_COMPLETE
	if response != nil {
		delivery, certainty = response.GetDelivery(), response.GetCertainty()
	}
	operation := request.GetContext().GetOperation()
	return &providerv0.ActionReceipt{
		ReceiptId: "receipt-" + operation.GetOperationId() + "-" + request.GetAction().GetActionId(),
		Operation: operation, Action: request.GetAction(), Delivery: delivery, Certainty: certainty,
		ArtifactDigest: s.artifactDigest, SafeResult: safe, CaptureReferences: captures, Diagnostics: diagnostics,
	}
}

func (s *Server) unchangedState(request *providerv0.ApplyActionRequest) *providerv0.ProviderState {
	if request.GetState().GetV1() != nil {
		return request.GetState()
	}
	return s.freshState(request)
}

func (s *Server) nextState(request *providerv0.ApplyActionRequest, captures []*providerv0.OpaqueReference) *providerv0.ProviderState {
	var next *providerv0.ProviderState
	if request.GetState().GetV1() != nil {
		next = proto.Clone(request.GetState()).(*providerv0.ProviderState)
	} else {
		next = s.freshState(request)
	}
	v1 := next.GetV1()
	v1.Generation++
	v1.PlanDigest = request.GetPlan().GetPlanDigest()
	v1.Operation = request.GetContext().GetOperation()
	if v1.ProviderOwnedFields == nil {
		v1.ProviderOwnedFields = map[string]*providerv0.PublicValue{}
	}
	switch request.GetAction().GetActionId() {
	case "project-create", "project-update", "project-import":
		v1.ProviderOwnedFields["project_resource_name"] = publicString(parseInputs(request.GetContext().GetOffline().GetInput()).ProjectID)
	case "project-delete":
		delete(v1.ProviderOwnedFields, "project_resource_name")
	case "application-create", "application-update", "application-import":
		v1.ProviderOwnedFields["application_resource_name"] = publicString(parseInputs(request.GetContext().GetOffline().GetInput()).ApplicationID)
	case "application-delete":
		delete(v1.ProviderOwnedFields, "application_resource_name")
	}
	if request.GetAction().GetActionId() == "token-server-create" {
		v1.ProviderOwnedFields["server_resource_name"] = publicString(tokenName(ownershipMarker(request.GetContext().GetOffline().GetBinding()), "server"))
	}
	if request.GetAction().GetActionId() == "token-browser-create" {
		v1.ProviderOwnedFields["browser_resource_name"] = publicString(tokenName(ownershipMarker(request.GetContext().GetOffline().GetBinding()), "browser"))
	}
	for _, capture := range captures {
		found := false
		for _, current := range v1.SecretReferences {
			if current.GetReference() == capture.GetReference() {
				found = true
			}
		}
		if !found {
			v1.SecretReferences = append(v1.SecretReferences, capture)
		}
	}
	return next
}

func (s *Server) freshState(request *providerv0.ApplyActionRequest) *providerv0.ProviderState {
	return providerstate.WrapV1(&providerv0.ProviderStateV1{
		StateSchemaVersion: stateVersion, Binding: request.GetContext().GetOffline().GetBinding(),
		ProviderId:      s.manifest.Agent.Publisher + "/" + s.manifest.Agent.Name,
		ProviderVersion: s.manifest.Agent.Version, ManifestSchemaVersion: s.manifest.SchemaVersion,
		ManifestDigest: s.manifestDigest, ArtifactDigest: s.artifactDigest,
		Ownership: providerv0.Ownership_OWNERSHIP_OBSERVED,
	})
}

func validateState(state *providerv0.ProviderState) error {
	return providerstate.ValidateVersioned(state, stateVersion)
}
