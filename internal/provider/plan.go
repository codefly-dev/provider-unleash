package provider

import (
	"context"
	"fmt"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	providerv0 "github.com/codefly-dev/core/generated/go/codefly/services/provider/v0"
	"github.com/codefly-dev/core/provider/canonical"
	"github.com/codefly-dev/core/provider/sdk"
)

type observations map[string]*providerv0.MaterialResourceObservation

func indexedObservations(material *providerv0.MaterialObservation) observations {
	result := observations{}
	for _, resource := range material.GetResources() {
		identity := resource.GetIdentity()
		result[identity.GetResourceType()+"\x00"+identity.GetRemoteId()] = resource
	}
	return result
}

func (values observations) find(resourceType, remoteID string) *providerv0.MaterialResourceObservation {
	return values[resourceType+"\x00"+remoteID]
}

func (s *Server) Plan(_ context.Context, request *providerv0.PlanRequest) (*providerv0.PlanResponse, error) {
	desired := request.GetDesired()
	inputValues := request.GetContext().GetInput()
	if desired != nil {
		inputValues = desired.GetInput()
	}
	in := parseInputs(inputValues)
	if in.Intent == "" {
		in.Intent = "apply"
	}
	diagnostics := validateRawInputs(inputValues)
	diagnostics = append(diagnostics, in.validate()...)
	if hasErrors(diagnostics) {
		return &providerv0.PlanResponse{Diagnostics: diagnostics}, nil
	}
	binding := request.GetContext().GetBinding()
	if desired.GetBinding() != nil {
		binding = desired.GetBinding()
	}
	if request.GetObservation() == nil || !request.GetObservation().GetComplete() {
		action, err := sdk.NewBlockedAction("observation-incomplete", 0, actionResource)
		if err != nil {
			return nil, err
		}
		action.Ownership = providerv0.Ownership_OWNERSHIP_OBSERVED
		action.Summary = "retry the bounded observation before any mutation"
		plan, err := s.assemblePlan(request, binding, []*providerv0.PlanAction{action})
		if err != nil {
			return nil, err
		}
		return &providerv0.PlanResponse{Plan: plan, Diagnostics: []*basev0.FailureDiagnostic{
			diagnostic(basev0.FailureDiagnostic_ERROR, diagnosticIncomplete, "the observation is incomplete; no mutation is safe"),
		}}, nil
	}
	observed := indexedObservations(request.GetObservation())
	if in.Intent == "destroy" {
		return s.planDestroy(request, binding, in, observed)
	}
	actions, planningDiagnostics, err := s.planApply(request, binding, in, observed)
	if err != nil {
		return nil, err
	}
	diagnostics = append(diagnostics, planningDiagnostics...)
	plan, err := s.assemblePlan(request, binding, actions)
	if err != nil {
		return nil, err
	}
	return &providerv0.PlanResponse{Plan: plan, Diagnostics: diagnostics}, nil
}

func (s *Server) planApply(request *providerv0.PlanRequest, binding *providerv0.BindingAddress, in inputs, observed observations) ([]*providerv0.PlanAction, []*basev0.FailureDiagnostic, error) {
	marker := ownershipMarker(binding)
	accountID := request.GetObservation().GetAccountIdentity()
	var actions []*providerv0.PlanAction
	var diagnostics []*basev0.FailureDiagnostic
	blocked := false
	appendAction := func(action *providerv0.PlanAction) {
		actions = append(actions, action)
		if action.GetType() == providerv0.ActionType_ACTION_TYPE_BLOCKED {
			blocked = true
		}
	}

	project := observed.find(resourceProject, in.ProjectID)
	switch {
	case project == nil:
		action, err := sdk.NewCreateAction("project-create", 0, actionResource, in.ProjectID)
		if err != nil {
			return nil, nil, err
		}
		action.Ownership = providerv0.Ownership_OWNERSHIP_OWNED
		action.Summary = "create the declared project with the binding ownership marker"
		appendAction(action)
	case project.GetOwnership() == providerv0.Ownership_OWNERSHIP_UNMANAGED:
		if exactImport(in, resourceProject, in.ProjectID) {
			action, err := sdk.NewImportAction("project-import", 0, actionResource)
			if err != nil {
				return nil, nil, err
			}
			action.RemoteIdentity = remoteIdentity(accountID, actionResource, in.ProjectID)
			action.Ownership = providerv0.Ownership_OWNERSHIP_ADOPTED
			action.Summary = "import the exact declared project and stamp ownership"
			appendAction(action)
		} else {
			action, err := blockedConflict("project-conflict", "an unmanaged project already has the declared id; import it by exact type and id")
			if err != nil {
				return nil, nil, err
			}
			appendAction(action)
			diagnostics = append(diagnostics, diagnostic(basev0.FailureDiagnostic_ERROR, diagnosticConflict, action.GetSummary()))
		}
	case project.GetProviderOwnedFields()["name"].GetStringValue() != in.ProjectName || project.GetProviderOwnedFields()["description"].GetStringValue() != marker:
		action, err := sdk.NewUpdateAction("project-update", 0, actionResource)
		if err != nil {
			return nil, nil, err
		}
		action.RemoteIdentity = remoteIdentity(accountID, actionResource, in.ProjectID)
		action.Ownership = project.GetOwnership()
		action.Summary = "converge the owned project name and ownership marker"
		appendAction(action)
	}

	environment := observed.find(resourceEnvironment, in.EnvironmentID)
	switch {
	case environment == nil:
		action, err := sdk.NewCreateAction("environment-create", 0, actionResource, in.EnvironmentID)
		if err != nil {
			return nil, nil, err
		}
		action.Ownership = providerv0.Ownership_OWNERSHIP_OWNED
		action.Summary = "create the declared environment"
		appendAction(action)
	case environment.GetProviderOwnedFields()["type"].GetStringValue() != in.EnvironmentType:
		action, err := blockedConflict("environment-conflict", "the declared environment exists with a different type")
		if err != nil {
			return nil, nil, err
		}
		appendAction(action)
		diagnostics = append(diagnostics, diagnostic(basev0.FailureDiagnostic_ERROR, diagnosticConflict, action.GetSummary()))
	}

	associationID := in.ProjectID + ":" + in.EnvironmentID
	if observed.find(resourceProjectEnvironment, associationID) == nil {
		action, err := sdk.NewCreateAction("project-environment-create", 0, actionResource, in.ProjectID)
		if err != nil {
			return nil, nil, err
		}
		action.Ownership = providerv0.Ownership_OWNERSHIP_OWNED
		action.Summary = "enable the declared environment for the declared project"
		appendAction(action)
	}

	application := observed.find(resourceApplication, in.ApplicationID)
	switch {
	case application == nil:
		action, err := sdk.NewCreateAction("application-create", 0, actionResource, in.ApplicationID)
		if err != nil {
			return nil, nil, err
		}
		action.Ownership = providerv0.Ownership_OWNERSHIP_OWNED
		action.Summary = "create the declared application with the binding ownership URL"
		appendAction(action)
	case application.GetOwnership() == providerv0.Ownership_OWNERSHIP_UNMANAGED:
		if exactImport(in, resourceApplication, in.ApplicationID) {
			action, err := sdk.NewImportAction("application-import", 0, actionResource)
			if err != nil {
				return nil, nil, err
			}
			action.RemoteIdentity = remoteIdentity(accountID, actionResource, in.ApplicationID)
			action.Ownership = providerv0.Ownership_OWNERSHIP_ADOPTED
			action.Summary = "import the exact declared application and stamp ownership"
			appendAction(action)
		} else {
			action, err := blockedConflict("application-conflict", "an unmanaged application already has the declared id; import it by exact type and id")
			if err != nil {
				return nil, nil, err
			}
			appendAction(action)
			diagnostics = append(diagnostics, diagnostic(basev0.FailureDiagnostic_ERROR, diagnosticConflict, action.GetSummary()))
		}
	case application.GetProviderOwnedFields()["url"].GetStringValue() != applicationOwnershipURL(marker):
		action, err := sdk.NewUpdateAction("application-update", 0, actionResource)
		if err != nil {
			return nil, nil, err
		}
		action.RemoteIdentity = remoteIdentity(accountID, actionResource, in.ApplicationID)
		action.Ownership = application.GetOwnership()
		action.Summary = "restore the application ownership URL"
		appendAction(action)
	}

	for _, token := range []struct{ resourceType, kind, tokenType string }{
		{resourceServerToken, "server", "backend"},
		{resourceBrowserToken, "browser", "frontend"},
	} {
		name := tokenName(marker, token.kind)
		resource := observed.find(token.resourceType, name)
		if resource == nil {
			action, err := sdk.NewCreateAction("token-"+token.kind+"-create", 0, actionResource, name)
			if err != nil {
				return nil, nil, err
			}
			action.Ownership = providerv0.Ownership_OWNERSHIP_OWNED
			action.Summary = "create the consumer-scoped " + token.kind + " token and capture it to the secret sink"
			appendAction(action)
			continue
		}
		if resource.GetOwnership() != providerv0.Ownership_OWNERSHIP_OWNED {
			action, err := blockedConflict("token-"+token.kind+"-conflict", "the deterministic "+token.kind+" token name is not recorded as owned by this binding")
			if err != nil {
				return nil, nil, err
			}
			appendAction(action)
			diagnostics = append(diagnostics, diagnostic(basev0.FailureDiagnostic_ERROR, diagnosticConflict, action.GetSummary()))
			continue
		}
		fields := resource.GetProviderOwnedFields()
		if fields["type"].GetStringValue() != token.tokenType || fields["environment"].GetStringValue() != in.EnvironmentID || !containsString(fields["projects"], in.ProjectID) {
			action, err := blockedConflict("token-"+token.kind+"-conflict", "the deterministic "+token.kind+" token name exists with incompatible scope or classification")
			if err != nil {
				return nil, nil, err
			}
			appendAction(action)
			diagnostics = append(diagnostics, diagnostic(basev0.FailureDiagnostic_ERROR, diagnosticConflict, action.GetSummary()))
		}
	}

	if !blocked {
		output, err := s.planOutput(request, binding, in, observed)
		if err != nil {
			return nil, nil, err
		}
		if output != nil {
			actions = append(actions, output)
		}
	}
	for index, action := range actions {
		action.Position = uint32(index)
	}
	return actions, diagnostics, nil
}

func (s *Server) planOutput(request *providerv0.PlanRequest, binding *providerv0.BindingAddress, in inputs, observed observations) (*providerv0.PlanAction, error) {
	target := request.GetOutputTarget()
	if target.GetTargetGeneration() == 0 {
		return nil, nil
	}
	marker := ownershipMarker(binding)
	if observed.find(resourceServerToken, tokenName(marker, "server")) == nil || observed.find(resourceBrowserToken, tokenName(marker, "browser")) == nil {
		return nil, nil
	}
	serverReference := referenceByFingerprint(request.GetDesired().GetCredentialReferences(), in.ServerCredentialFingerprint)
	browserReference := referenceByFingerprint(request.GetDesired().GetCredentialReferences(), in.BrowserCredentialFingerprint)
	if serverReference == nil || browserReference == nil || serverReference.GetReference() == browserReference.GetReference() {
		return nil, nil
	}
	values := map[string]*providerv0.OutputValue{
		"FEATURE_FLAGS_APPLICATION_ID": publicOutput(in.ApplicationID),
		"FEATURE_FLAGS_ENVIRONMENT_ID": publicOutput(in.EnvironmentID),
		"FEATURE_FLAGS_PROVIDER_MODE":  publicOutput(in.ProviderMode),
	}
	actionID := ""
	summary := ""
	switch target.GetContract() {
	case featureFlagsContract:
		serverEndpoint := observed.find(resourceEndpoint, "server").GetProviderOwnedFields()["endpoint"].GetStringValue()
		if serverEndpoint == "" {
			return nil, nil
		}
		values["FEATURE_FLAGS_SERVER_ENDPOINT"] = publicOutput(serverEndpoint)
		values["FEATURE_FLAGS_SERVER_CREDENTIAL"] = referenceOutput(serverReference)
		actionID = "feature-flags-project"
		summary = "project feature-flags@1 with the opaque server credential"
	case featureFlagsBrowserContract:
		edgeEndpoint := observed.find(resourceEndpoint, "edge").GetProviderOwnedFields()["endpoint"].GetStringValue()
		if edgeEndpoint == "" {
			return nil, nil
		}
		values["FEATURE_FLAGS_EDGE_ENDPOINT"] = publicOutput(edgeEndpoint)
		values["FEATURE_FLAGS_BROWSER_CREDENTIAL"] = referenceOutput(browserReference)
		actionID = "feature-flags-browser-project"
		summary = "project feature-flags-browser@1 with the opaque browser credential"
	default:
		return nil, nil
	}
	proposal := &providerv0.OutputProposal{Contract: target.GetContract(), TargetGeneration: target.GetTargetGeneration(), Values: values}
	action, err := sdk.NewProjectOutputAction(actionID, 0, proposal)
	if err != nil {
		return nil, err
	}
	if target.GetCurrentGeneration() == target.GetTargetGeneration() && target.GetCurrentDigest() == action.GetOutput().GetDigest() {
		return nil, nil
	}
	action.Summary = summary
	return action, nil
}

func (s *Server) planDestroy(request *providerv0.PlanRequest, binding *providerv0.BindingAddress, in inputs, observed observations) (*providerv0.PlanResponse, error) {
	if request.GetDesired().GetDeletionPolicy() != "delete-owned" {
		plan, err := s.assemblePlan(request, binding, nil)
		return &providerv0.PlanResponse{Plan: plan}, err
	}
	accountID := request.GetObservation().GetAccountIdentity()
	marker := ownershipMarker(binding)
	var actions []*providerv0.PlanAction
	if application := observed.find(resourceApplication, in.ApplicationID); application != nil && application.GetOwnership() == providerv0.Ownership_OWNERSHIP_OWNED && application.GetProviderOwnedFields()["url"].GetStringValue() == applicationOwnershipURL(marker) {
		action, err := sdk.NewDeleteAction("application-delete", 0, actionResource)
		if err != nil {
			return nil, err
		}
		action.RemoteIdentity = remoteIdentity(accountID, actionResource, in.ApplicationID)
		action.Ownership = providerv0.Ownership_OWNERSHIP_OWNED
		actions = append(actions, action)
	}
	if project := observed.find(resourceProject, in.ProjectID); project != nil && project.GetOwnership() == providerv0.Ownership_OWNERSHIP_OWNED && project.GetProviderOwnedFields()["description"].GetStringValue() == marker && project.GetProviderOwnedFields()["feature_count"].GetIntegerValue() == 0 {
		action, err := sdk.NewDeleteAction("project-delete", 0, actionResource)
		if err != nil {
			return nil, err
		}
		action.RemoteIdentity = remoteIdentity(accountID, actionResource, in.ProjectID)
		action.Ownership = providerv0.Ownership_OWNERSHIP_OWNED
		actions = append(actions, action)
	}
	for index, action := range actions {
		action.Position = uint32(index)
	}
	plan, err := s.assemblePlan(request, binding, actions)
	return &providerv0.PlanResponse{Plan: plan}, err
}

func blockedConflict(id, summary string) (*providerv0.PlanAction, error) {
	action, err := sdk.NewBlockedAction(id, 0, actionResource)
	if err == nil {
		action.Ownership = providerv0.Ownership_OWNERSHIP_UNMANAGED
		action.Summary = summary
	}
	return action, err
}

func exactImport(in inputs, resourceType, remoteID string) bool {
	return in.ImportResourceType == resourceType && in.ImportRemoteID == remoteID
}

func containsString(value *providerv0.PublicValue, wanted string) bool {
	for _, item := range value.GetListValue().GetValues() {
		if item.GetStringValue() == wanted {
			return true
		}
	}
	return false
}

func referenceByFingerprint(references []*providerv0.OpaqueReference, fingerprint string) *providerv0.OpaqueReference {
	if fingerprint == "" {
		return nil
	}
	for _, reference := range references {
		if reference.GetPurpose() == providerv0.CredentialPurpose_CREDENTIAL_PURPOSE_RUNTIME && reference.GetSafeFingerprint() == fingerprint {
			return reference
		}
	}
	return nil
}

func publicOutput(value string) *providerv0.OutputValue {
	return &providerv0.OutputValue{Kind: &providerv0.OutputValue_PublicValue{PublicValue: publicString(value)}}
}

func referenceOutput(reference *providerv0.OpaqueReference) *providerv0.OutputValue {
	return &providerv0.OutputValue{Kind: &providerv0.OutputValue_OpaqueReference{OpaqueReference: reference}}
}

func (s *Server) assemblePlan(request *providerv0.PlanRequest, binding *providerv0.BindingAddress, actions []*providerv0.PlanAction) (*providerv0.OrderedPlan, error) {
	plan := &providerv0.OrderedPlan{
		PlanId:         fmt.Sprintf("plan-%s-%s-%s-%d", binding.GetWorkspaceId(), binding.GetEnvironmentId(), binding.GetBindingId(), request.GetStateGeneration().GetGeneration()),
		ArtifactDigest: s.artifactDigest, ManifestDigest: s.manifestDigest, CatalogDigest: s.catalogDigest, Actions: actions,
	}
	var err error
	if request.GetDesired() != nil {
		plan.DesiredDigest, err = canonical.BindingDesiredStateDigest(request.GetDesired())
	}
	if err == nil && request.GetObservation() != nil {
		plan.ObservationDigest, err = canonical.MaterialObservationDigest(request.GetObservation())
	}
	if err == nil && request.GetOutputTarget() != nil {
		plan.OutputTargetDigest, err = canonical.OutputTargetDigest(request.GetOutputTarget())
	}
	if err == nil && request.GetStateGeneration() != nil {
		plan.StateGenerationDigest, err = canonical.StateGenerationDigest(request.GetStateGeneration())
	}
	if err == nil && request.GetPolicyInput() != nil {
		plan.PolicyInputDigest, err = canonical.PolicyApprovalInputDigest(request.GetPolicyInput())
	}
	if err != nil {
		return nil, err
	}
	return canonical.BindOrderedPlanDigest(plan)
}
