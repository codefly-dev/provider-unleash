package provider

import (
	"context"
	"os"
	"strings"
	"testing"

	providerv0 "github.com/codefly-dev/core/generated/go/codefly/services/provider/v0"
	"github.com/codefly-dev/core/provider/canonical"
	"github.com/codefly-dev/core/provider/configuration"
	"github.com/codefly-dev/core/provider/manifest"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

var (
	serverFingerprint     = "sha256:" + strings.Repeat("1", 64)
	browserFingerprint    = "sha256:" + strings.Repeat("2", 64)
	managementFingerprint = "sha256:" + strings.Repeat("3", 64)
)

type fakeUnleash struct {
	project            bool
	projectName        string
	projectDescription string
	environment        bool
	environmentType    string
	association        bool
	application        bool
	applicationURL     string
	serverToken        bool
	browserToken       bool
	serverIdentity     string
	browserIdentity    string
	uncertainOn        string
	executions         int
	proposals          int
}

func (fake *fakeUnleash) ExecuteRequest(_ context.Context, request *providerv0.ExecuteRequestRequest, _ ...grpc.CallOption) (*providerv0.ExecuteRequestResponse, error) {
	fake.executions++
	id := request.GetRequest().GetRequestDescriptorId()
	if fake.uncertainOn == id {
		return &providerv0.ExecuteRequestResponse{RequestId: request.GetRequestId(), Delivery: providerv0.DeliveryState_DELIVERY_STATE_SENT_OUTCOME_UNKNOWN, Certainty: providerv0.OutcomeCertainty_OUTCOME_CERTAINTY_UNCERTAIN}, nil
	}
	response := &providerv0.ExecuteRequestResponse{
		RequestId: request.GetRequestId(), Delivery: providerv0.DeliveryState_DELIVERY_STATE_RESPONSE_RECEIVED,
		StatusCode: 200, Certainty: providerv0.OutcomeCertainty_OUTCOME_CERTAINTY_COMPLETE,
	}
	field := func(selector string, value *providerv0.PublicValue) {
		response.Forwarded = append(response.Forwarded, &providerv0.FilteredField{Selector: selector, Value: value})
	}
	switch id {
	case "project.overview":
		if !fake.project {
			response.StatusCode = 404
			break
		}
		field("$.name", publicString(fake.projectName))
		field("$.description", publicString(fake.projectDescription))
		field("$.featureTypeCounts[0].count", publicInteger(0))
		if fake.association {
			field("$.environments[0].environment", publicString("production"))
		}
	case "environment.get":
		if !fake.environment {
			response.StatusCode = 404
			break
		}
		field("$.name", publicString("production"))
		field("$.type", publicString(fake.environmentType))
		field("$.enabled", publicBool(true))
	case "application.get":
		if !fake.application {
			response.StatusCode = 404
			break
		}
		field("$.appName", publicString("starter-web"))
		field("$.url", publicString(fake.applicationURL))
	case "token.get":
		marker := ownershipMarker(testBinding())
		requested := request.GetRequest().GetPathParameters()["resource_id"].GetStringValue()
		index := 0
		addToken := func(name, tokenType, identity string) {
			prefix := "$.tokens[" + string(rune('0'+index)) + "]"
			field(prefix+".tokenName", publicString(name))
			field(prefix+".type", publicString(tokenType))
			field(prefix+".environment", publicString("production"))
			field(prefix+".projects[0]", publicString("starter"))
			field(prefix+".createdAt", publicString("2026-08-14T12:00:00Z"))
			field(prefix+".alias", publicString(identity))
			index++
		}
		if fake.serverToken && requested == tokenName(marker, "server") {
			if fake.serverIdentity == "" {
				fake.serverIdentity = "server-token-id"
			}
			addToken(tokenName(marker, "server"), "backend", fake.serverIdentity)
		}
		if fake.browserToken && requested == tokenName(marker, "browser") {
			if fake.browserIdentity == "" {
				fake.browserIdentity = "browser-token-id"
			}
			addToken(tokenName(marker, "browser"), "frontend", fake.browserIdentity)
		}
	case "project.create", "project.update":
		fake.project = true
		fake.projectName = request.GetRequest().GetBody()["name"].GetStringValue()
		fake.projectDescription = request.GetRequest().GetBody()["description"].GetStringValue()
		field("$.id", publicString("starter"))
		field("$.name", publicString(fake.projectName))
		field("$.description", publicString(fake.projectDescription))
	case "project.delete":
		fake.project = false
	case "environment.create":
		fake.environment = true
		fake.environmentType = request.GetRequest().GetBody()["type"].GetStringValue()
		field("$.name", publicString("production"))
		field("$.type", publicString(fake.environmentType))
		field("$.enabled", publicBool(true))
	case "project-environment.create":
		fake.association = true
	case "application.create", "application.update":
		fake.application = true
		fake.applicationURL = request.GetRequest().GetBody()["url"].GetStringValue()
	case "application.delete":
		fake.application = false
	case "token.create":
		tokenType := request.GetRequest().GetBody()["type"].GetStringValue()
		kind := "browser"
		fingerprint := browserFingerprint
		if tokenType == "backend" {
			kind = "server"
			fingerprint = serverFingerprint
			fake.serverToken = true
			fake.serverIdentity = "server-token-id"
		} else {
			fake.browserToken = true
			fake.browserIdentity = "browser-token-id"
		}
		field("$.tokenName", request.GetRequest().GetBody()["tokenName"])
		field("$.type", publicString(tokenType))
		field("$.environment", publicString("production"))
		field("$.projects[0]", publicString("starter"))
		field("$.createdAt", publicString("2026-08-14T12:00:00Z"))
		identity := fake.serverIdentity
		if kind == "browser" {
			identity = fake.browserIdentity
		}
		field("$.alias", publicString(identity))
		response.Captures = []*providerv0.CaptureResult{{
			CaptureId: "$.secret", Selector: "$.secret", Captured: true,
			SinkReference: runtimeReference("secret://unleash/"+kind, fingerprint),
		}}
	default:
		response.StatusCode = 404
	}
	return response, nil
}

func (fake *fakeUnleash) RecordCheckpoint(context.Context, *providerv0.RecordCheckpointRequest, ...grpc.CallOption) (*providerv0.RecordCheckpointResponse, error) {
	return &providerv0.RecordCheckpointResponse{Durable: true}, nil
}

func (fake *fakeUnleash) ResolveCapture(context.Context, *providerv0.ResolveCaptureRequest, ...grpc.CallOption) (*providerv0.ResolveCaptureResponse, error) {
	return &providerv0.ResolveCaptureResponse{}, nil
}

func (fake *fakeUnleash) ProposeOutput(_ context.Context, request *providerv0.ProposeOutputRequest, _ ...grpc.CallOption) (*providerv0.ProposeOutputResponse, error) {
	fake.proposals++
	return &providerv0.ProposeOutputResponse{Durable: true, Generation: request.GetProposal().GetTargetGeneration(), Digest: request.GetProposal().GetDigest()}, nil
}

func TestLifecycleConvergesRuntimeAndBrowserOutputs(t *testing.T) {
	fake := &fakeUnleash{}
	server := testServer(t, fake)
	providerContext := testProviderContext(testInputs("apply"))

	validated, err := server.Validate(context.Background(), &providerv0.ValidateRequest{Context: providerContext.GetOffline()})
	if err != nil || !validated.GetValid() {
		t.Fatalf("validate: valid=%v err=%v diagnostics=%v", validated.GetValid(), err, validated.GetDiagnostics())
	}

	observation := observe(t, server, providerContext, nil)
	if !observation.GetComplete() {
		t.Fatalf("initial observation incomplete at %s", observation.GetNextCursor())
	}
	repeatedObservation := observe(t, server, providerContext, nil)
	if !proto.Equal(observation, repeatedObservation) {
		t.Fatal("unchanged Unleash state produced a different material observation")
	}
	desired := testDesired(providerContext.GetOffline().GetInput(), nil, "retain")
	first := plan(t, server, providerContext.GetOffline(), desired, observation, &providerv0.OutputTarget{Contract: featureFlagsContract, TargetGeneration: 1})
	if got := len(first.GetActions()); got != 6 {
		t.Fatalf("initial action count = %d, want 6", got)
	}
	repeated := plan(t, server, providerContext.GetOffline(), desired, observation, &providerv0.OutputTarget{Contract: featureFlagsContract, TargetGeneration: 1})
	if !proto.Equal(first, repeated) {
		t.Fatal("same desired state and observation produced a different plan")
	}

	var state *providerv0.ProviderState
	for _, action := range first.GetActions() {
		providerContext.Operation.ActionId = action.GetActionId()
		result, applyErr := server.ApplyAction(context.Background(), &providerv0.ApplyActionRequest{Context: providerContext, Plan: first, Action: action, State: state})
		if applyErr != nil {
			t.Fatalf("apply %s: %v", action.GetActionId(), applyErr)
		}
		if result.GetReceipt().GetCertainty() != providerv0.OutcomeCertainty_OUTCOME_CERTAINTY_COMPLETE {
			t.Fatalf("apply %s was not complete", action.GetActionId())
		}
		state = result.GetNextState()
	}
	if len(state.GetV1().GetSecretReferences()) != 2 {
		t.Fatalf("captured references = %d, want 2", len(state.GetV1().GetSecretReferences()))
	}

	providerContext.Offline.Input["server_credential_fingerprint"] = publicString(serverFingerprint)
	providerContext.Offline.Input["browser_credential_fingerprint"] = publicString(browserFingerprint)
	desired = testDesired(providerContext.GetOffline().GetInput(), state.GetV1().GetSecretReferences(), "retain")
	observation = observe(t, server, providerContext, state)
	runtimePlan := plan(t, server, providerContext.GetOffline(), desired, observation, &providerv0.OutputTarget{Contract: featureFlagsContract, TargetGeneration: 1})
	if len(runtimePlan.GetActions()) != 1 || runtimePlan.GetActions()[0].GetType() != providerv0.ActionType_ACTION_TYPE_PROJECT_OUTPUT {
		t.Fatalf("runtime plan = %v, want only output", runtimePlan.GetActions())
	}
	runtimeOutput := runtimePlan.GetActions()[0].GetOutput()
	assertFeatureFlagsOutput(t, runtimeOutput, featureFlagsContract)
	providerContext.Operation.ActionId = runtimePlan.GetActions()[0].GetActionId()
	runtimeProjected, err := server.ApplyAction(context.Background(), &providerv0.ApplyActionRequest{Context: providerContext, Plan: runtimePlan, Action: runtimePlan.GetActions()[0], State: state})
	if err != nil {
		t.Fatalf("apply runtime output: %v", err)
	}
	if fake.proposals != 1 {
		t.Fatalf("output proposals = %d, want 1", fake.proposals)
	}

	runtimeEmpty := plan(t, server, providerContext.GetOffline(), desired, observation, &providerv0.OutputTarget{
		Contract: featureFlagsContract, TargetGeneration: 1, CurrentGeneration: 1, CurrentDigest: runtimeOutput.GetDigest(),
	})
	if len(runtimeEmpty.GetActions()) != 0 {
		t.Fatalf("converged runtime plan has %d actions, want empty", len(runtimeEmpty.GetActions()))
	}

	browserPlan := plan(t, server, providerContext.GetOffline(), desired, observation, &providerv0.OutputTarget{Contract: featureFlagsBrowserContract, TargetGeneration: 1})
	if len(browserPlan.GetActions()) != 1 || browserPlan.GetActions()[0].GetActionId() != "feature-flags-browser-project" {
		t.Fatalf("browser plan = %v, want only browser output", browserPlan.GetActions())
	}
	browserOutput := browserPlan.GetActions()[0].GetOutput()
	assertFeatureFlagsOutput(t, browserOutput, featureFlagsBrowserContract)
	providerContext.Operation.ActionId = browserPlan.GetActions()[0].GetActionId()
	browserProjected, err := server.ApplyAction(context.Background(), &providerv0.ApplyActionRequest{
		Context: providerContext, Plan: browserPlan, Action: browserPlan.GetActions()[0], State: runtimeProjected.GetNextState(),
	})
	if err != nil {
		t.Fatalf("apply browser output: %v", err)
	}
	if fake.proposals != 2 {
		t.Fatalf("output proposals = %d, want 2", fake.proposals)
	}
	browserEmpty := plan(t, server, providerContext.GetOffline(), desired, observation, &providerv0.OutputTarget{
		Contract: featureFlagsBrowserContract, TargetGeneration: 1, CurrentGeneration: 1, CurrentDigest: browserOutput.GetDigest(),
	})
	if len(browserEmpty.GetActions()) != 0 {
		t.Fatalf("converged browser plan has %d actions, want empty", len(browserEmpty.GetActions()))
	}
	if err := validateState(browserProjected.GetNextState()); err != nil {
		t.Fatalf("next state: %v", err)
	}
}

func TestOwnershipDriftImportAndDestroy(t *testing.T) {
	fake := &fakeUnleash{project: true, projectName: "Other", projectDescription: "operator-owned", environment: true, environmentType: "production", association: true, application: true, applicationURL: "https://operator.invalid", serverToken: true, browserToken: true}
	server := testServer(t, fake)
	providerContext := testProviderContext(testInputs("apply"))
	observation := observe(t, server, providerContext, nil)
	desired := testDesired(providerContext.GetOffline().GetInput(), nil, "retain")
	conflict := plan(t, server, providerContext.GetOffline(), desired, observation, nil)
	if !hasAction(conflict, "project-conflict") || !hasAction(conflict, "application-conflict") || !hasAction(conflict, "token-server-conflict") || !hasAction(conflict, "token-browser-conflict") {
		t.Fatalf("unmanaged conflicts were not blocked: %v", conflict.GetActions())
	}
	providerContext.Offline.Input["intent"] = publicString("destroy")
	unmanagedDestroy := plan(t, server, providerContext.GetOffline(), testDesired(providerContext.GetOffline().GetInput(), nil, "delete-owned"), observation, nil)
	if len(unmanagedDestroy.GetActions()) != 0 {
		t.Fatal("destroy selected unmanaged resources")
	}
	providerContext.Offline.Input["intent"] = publicString("apply")

	providerContext.Offline.Input["import_resource_type"] = publicString(resourceProject)
	providerContext.Offline.Input["import_remote_id"] = publicString("starter")
	desired = testDesired(providerContext.GetOffline().GetInput(), nil, "retain")
	importPlan := plan(t, server, providerContext.GetOffline(), desired, observation, nil)
	if !hasAction(importPlan, "project-import") {
		t.Fatal("exact project import was not planned")
	}
	projectImport := actionByID(t, importPlan, "project-import")
	providerContext.Operation.ActionId = projectImport.GetActionId()
	imported, err := server.ApplyAction(context.Background(), &providerv0.ApplyActionRequest{Context: providerContext, Plan: importPlan, Action: projectImport})
	if err != nil {
		t.Fatalf("apply project import: %v", err)
	}
	state := imported.GetNextState()

	observation = observe(t, server, providerContext, state)
	providerContext.Offline.Input["import_resource_type"] = publicString(resourceApplication)
	providerContext.Offline.Input["import_remote_id"] = publicString("starter-web")
	desired = testDesired(providerContext.GetOffline().GetInput(), nil, "retain")
	applicationImportPlan := plan(t, server, providerContext.GetOffline(), desired, observation, nil)
	applicationImport := actionByID(t, applicationImportPlan, "application-import")
	providerContext.Operation.ActionId = applicationImport.GetActionId()
	imported, err = server.ApplyAction(context.Background(), &providerv0.ApplyActionRequest{Context: providerContext, Plan: applicationImportPlan, Action: applicationImport, State: state})
	if err != nil {
		t.Fatalf("apply application import: %v", err)
	}
	state = imported.GetNextState()

	fake.projectName = "Drifted"
	providerContext.Offline.Input["import_resource_type"] = publicString("")
	providerContext.Offline.Input["import_remote_id"] = publicString("")
	observation = observe(t, server, providerContext, state)
	desired = testDesired(providerContext.GetOffline().GetInput(), nil, "retain")
	drift := plan(t, server, providerContext.GetOffline(), desired, observation, nil)
	if !hasAction(drift, "project-update") || hasAction(drift, "application-update") {
		t.Fatal("owned project drift was not isolated for update")
	}
	action := actionByID(t, drift, "project-update")
	providerContext.Operation.ActionId = action.GetActionId()
	updated, applyErr := server.ApplyAction(context.Background(), &providerv0.ApplyActionRequest{Context: providerContext, Plan: drift, Action: action, State: state})
	if applyErr != nil {
		t.Fatalf("apply project-update: %v", applyErr)
	}
	state = updated.GetNextState()

	fake.applicationURL = "https://replacement.invalid"
	observation = observe(t, server, providerContext, state)
	replacement := plan(t, server, providerContext.GetOffline(), desired, observation, nil)
	if !hasAction(replacement, "application-conflict") || hasAction(replacement, "application-update") {
		t.Fatal("unstamped application replacement was not treated as unmanaged")
	}
	providerContext.Offline.Input["import_resource_type"] = publicString(resourceApplication)
	providerContext.Offline.Input["import_remote_id"] = publicString("starter-web")
	desired = testDesired(providerContext.GetOffline().GetInput(), nil, "retain")
	reimport := plan(t, server, providerContext.GetOffline(), desired, observation, nil)
	action = actionByID(t, reimport, "application-import")
	providerContext.Operation.ActionId = action.GetActionId()
	updated, err = server.ApplyAction(context.Background(), &providerv0.ApplyActionRequest{Context: providerContext, Plan: reimport, Action: action, State: state})
	if err != nil {
		t.Fatalf("reimport application: %v", err)
	}
	state = updated.GetNextState()
	providerContext.Offline.Input["import_resource_type"] = publicString("")
	providerContext.Offline.Input["import_remote_id"] = publicString("")

	observation = observe(t, server, providerContext, state)
	providerContext.Offline.Input["intent"] = publicString("destroy")
	desired = testDesired(providerContext.GetOffline().GetInput(), nil, "retain")
	retained := plan(t, server, providerContext.GetOffline(), desired, observation, nil)
	if len(retained.GetActions()) != 0 {
		t.Fatal("retain policy planned remote deletion")
	}
	desired.DeletionPolicy = "delete-owned"
	destroy := plan(t, server, providerContext.GetOffline(), desired, observation, nil)
	if !hasAction(destroy, "application-delete") || !hasAction(destroy, "project-delete") {
		t.Fatalf("owned destroy actions missing: %v", destroy.GetActions())
	}
	for _, action := range destroy.GetActions() {
		if action.GetOwnership() != providerv0.Ownership_OWNERSHIP_OWNED {
			t.Fatalf("destroy selected non-owned action %s", action.GetActionId())
		}
		providerContext.Operation.ActionId = action.GetActionId()
		deleted, applyErr := server.ApplyAction(context.Background(), &providerv0.ApplyActionRequest{Context: providerContext, Plan: destroy, Action: action, State: state})
		if applyErr != nil {
			t.Fatalf("apply %s: %v", action.GetActionId(), applyErr)
		}
		state = deleted.GetNextState()
	}
	if fake.project || fake.application {
		t.Fatal("exact owned resources survived delete-owned destroy")
	}
}

func TestIncompleteObservationAndUncertainMutationFailClosed(t *testing.T) {
	fake := &fakeUnleash{uncertainOn: "environment.get"}
	server := testServer(t, fake)
	providerContext := testProviderContext(testInputs("apply"))
	observation := observe(t, server, providerContext, nil)
	if observation.GetComplete() || observation.GetNextCursor() == "" {
		t.Fatal("uncertain read was not represented as incomplete")
	}
	desired := testDesired(providerContext.GetOffline().GetInput(), nil, "retain")
	blocked := plan(t, server, providerContext.GetOffline(), desired, observation, nil)
	if len(blocked.GetActions()) != 1 || blocked.GetActions()[0].GetType() != providerv0.ActionType_ACTION_TYPE_BLOCKED {
		t.Fatal("incomplete observation did not block")
	}

	fake.uncertainOn = ""
	complete := observe(t, server, providerContext, nil)
	createPlan := plan(t, server, providerContext.GetOffline(), desired, complete, nil)
	executions := fake.executions
	action := createPlan.GetActions()[0]
	providerContext.Operation.ActionId = action.GetActionId()
	result, err := server.ApplyAction(context.Background(), &providerv0.ApplyActionRequest{
		Context: providerContext, Plan: createPlan, Action: action,
		PriorCheckpoint: &providerv0.ActionCheckpoint{Delivery: providerv0.DeliveryState_DELIVERY_STATE_SENT_OUTCOME_UNKNOWN},
	})
	if err != nil {
		t.Fatalf("uncertain recovery: %v", err)
	}
	if fake.executions != executions {
		t.Fatal("uncertain mutation was blindly retried")
	}
	if result.GetReceipt().GetCertainty() != providerv0.OutcomeCertainty_OUTCOME_CERTAINTY_UNCERTAIN {
		t.Fatal("uncertain recovery claimed a complete outcome")
	}
}

func TestReplacedDeterministicTokenNameIsUnmanaged(t *testing.T) {
	marker := ownershipMarker(testBinding())
	fake := &fakeUnleash{
		project: true, projectName: "Starter", projectDescription: marker,
		environment: true, environmentType: "production", association: true,
		application: true, applicationURL: applicationOwnershipURL(marker),
		serverToken: true, browserToken: true,
	}
	server := testServer(t, fake)
	providerContext := testProviderContext(testInputs("apply"))
	state := providerStateWithTokens(t, server, providerContext, marker)
	fake.serverIdentity = "replacement-token-id"
	observation := observe(t, server, providerContext, state)
	serverToken := indexedObservations(observation).find(resourceServerToken, tokenName(marker, "server"))
	if serverToken.GetOwnership() != providerv0.Ownership_OWNERSHIP_UNMANAGED {
		t.Fatal("replacement token retained owned status")
	}
	result := plan(t, server, providerContext.GetOffline(), testDesired(providerContext.GetOffline().GetInput(), nil, "retain"), observation, nil)
	if !hasAction(result, "token-server-conflict") {
		t.Fatal("replacement token did not block mutation")
	}
}

func TestHostileActionsAndManagementProjectionAreRejected(t *testing.T) {
	fake := &fakeUnleash{}
	server := testServer(t, fake)
	providerContext := testProviderContext(testInputs("apply"))
	forged, _ := sdkProjectOutput("forged", &providerv0.OutputProposal{Contract: featureFlagsContract, TargetGeneration: 1, Values: map[string]*providerv0.OutputValue{}})
	_, err := server.ApplyAction(context.Background(), &providerv0.ApplyActionRequest{Context: providerContext, Plan: &providerv0.OrderedPlan{}, Action: forged})
	if err == nil || fake.proposals != 0 {
		t.Fatal("forged output action reached the host")
	}
	hostileInput := testInputs("apply")
	hostileInput["management_token"] = publicString("sk_test_0123456789abcdef")
	validated, err := server.Validate(context.Background(), &providerv0.ValidateRequest{Context: &providerv0.OfflineProviderContext{Input: hostileInput}})
	if err != nil || validated.GetValid() {
		t.Fatal("secret-shaped undeclared input was accepted")
	}

	marker := ownershipMarker(testBinding())
	fake.project, fake.projectName, fake.projectDescription = true, "Starter", marker
	fake.environment, fake.environmentType, fake.association = true, "production", true
	fake.application, fake.applicationURL = true, applicationOwnershipURL(marker)
	fake.serverToken, fake.browserToken = true, true
	state := providerStateWithTokens(t, server, providerContext, marker)
	observation := observe(t, server, providerContext, state)
	providerContext.Offline.Input["server_credential_fingerprint"] = publicString(managementFingerprint)
	providerContext.Offline.Input["browser_credential_fingerprint"] = publicString(browserFingerprint)
	references := []*providerv0.OpaqueReference{
		{Reference: "secret://unleash/management", Purpose: providerv0.CredentialPurpose_CREDENTIAL_PURPOSE_MANAGEMENT, SafeFingerprint: managementFingerprint},
		runtimeReference("secret://unleash/browser", browserFingerprint),
	}
	desired := testDesired(providerContext.GetOffline().GetInput(), references, "retain")
	result := plan(t, server, providerContext.GetOffline(), desired, observation, &providerv0.OutputTarget{Contract: featureFlagsContract, TargetGeneration: 1})
	if hasAction(result, "feature-flags-project") {
		t.Fatal("management credential was admitted into runtime projection")
	}
}

func TestManifestNeverForwardsTokenSecrets(t *testing.T) {
	contents, err := os.ReadFile("../../provider.codefly.yaml")
	if err != nil {
		t.Fatal(err)
	}
	providerManifest, err := manifest.Load(contents)
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	projectionContracts := map[string]bool{}
	for _, projection := range providerManifest.Projections {
		projectionContracts[projection.Contract] = true
	}
	if len(projectionContracts) != 2 || !projectionContracts[featureFlagsContract] || !projectionContracts[featureFlagsBrowserContract] {
		t.Fatalf("feature flags projections = %v", projectionContracts)
	}
	for _, schema := range providerManifest.ResponseSchemas {
		for _, field := range schema.Fields {
			if field.Selector.Path == "$.tokens[*].secret" && string(field.Disposition) == "FORWARD_SAFE" {
				t.Fatal("token list secret is forwarded")
			}
		}
	}
}

func testServer(t *testing.T, host Host) *Server {
	t.Helper()
	contents, err := os.ReadFile("../../provider.codefly.yaml")
	if err != nil {
		t.Fatal(err)
	}
	providerManifest, err := manifest.Load(contents)
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	digest, err := providerManifest.Digest()
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(contents, Identity{Publisher: "codefly.dev", Name: "unleash", Version: "0.1.0", ArtifactDigest: "sha256:" + strings.Repeat("a", 64), ManifestDigest: digest}, WithHost(host))
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	return server
}

func testBinding() *providerv0.BindingAddress {
	return &providerv0.BindingAddress{WorkspaceId: "workspace", EnvironmentId: "production", BindingId: "feature-flags"}
}

func testInputs(intent string) map[string]*providerv0.PublicValue {
	return map[string]*providerv0.PublicValue{
		"project_id": publicString("starter"), "project_name": publicString("Starter"),
		"environment_id": publicString("production"), "environment_type": publicString("production"),
		"application_id": publicString("starter-web"), "provider_mode": publicString("edge"), "intent": publicString(intent),
	}
}

func testProviderContext(input map[string]*providerv0.PublicValue) *providerv0.ProviderContext {
	origin := func(rule, host string, port uint32) *providerv0.AdmittedOrigin {
		value := &providerv0.AdmittedOrigin{OriginRuleId: rule, Scheme: "http", Host: host, Port: port}
		var err error
		value.AdmissionDigest, err = canonical.AdmittedOriginDigest(value)
		if err != nil {
			panic(err)
		}
		return value
	}
	return &providerv0.ProviderContext{
		Offline:     &providerv0.OfflineProviderContext{Binding: testBinding(), Input: input, Mode: providerv0.HostMode_HOST_MODE_PRODUCTION, AccountIdentity: "unleash-local"},
		Credentials: []*providerv0.CredentialHandle{{Handle: "opaque-management", Purpose: providerv0.CredentialPurpose_CREDENTIAL_PURPOSE_MANAGEMENT}},
		Endpoints: []*providerv0.SemanticEndpointReference{
			{EndpointId: "admin", OriginRuleId: "admin"}, {EndpointId: "server", OriginRuleId: "server"}, {EndpointId: "edge", OriginRuleId: "edge"},
		},
		AdmittedOrigins: []*providerv0.AdmittedOrigin{
			origin("admin", "unleash", 4242),
			origin("server", "unleash", 4242),
			origin("edge", "unleash-edge", 3063),
		},
		Operation: &providerv0.OperationIdentity{OperationId: "operation", AttemptId: "attempt"},
	}
}

func testDesired(input map[string]*providerv0.PublicValue, references []*providerv0.OpaqueReference, deletionPolicy string) *providerv0.BindingDesiredState {
	return &providerv0.BindingDesiredState{Binding: testBinding(), Input: input, CredentialReferences: references, DeletionPolicy: deletionPolicy}
}

func observe(t *testing.T, server *Server, providerContext *providerv0.ProviderContext, state *providerv0.ProviderState) *providerv0.MaterialObservation {
	t.Helper()
	response, err := server.Observe(context.Background(), &providerv0.ObserveRequest{Context: providerContext, State: state})
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	return response.GetMaterial()
}

func plan(t *testing.T, server *Server, offline *providerv0.OfflineProviderContext, desired *providerv0.BindingDesiredState, observation *providerv0.MaterialObservation, target *providerv0.OutputTarget) *providerv0.OrderedPlan {
	t.Helper()
	response, err := server.Plan(context.Background(), &providerv0.PlanRequest{Context: offline, Desired: desired, Observation: observation, OutputTarget: target, StateGeneration: &providerv0.StateGeneration{Generation: 1}})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if response.GetPlan() == nil {
		t.Fatalf("plan returned no ordered plan: %v", response.GetDiagnostics())
	}
	return response.GetPlan()
}

func runtimeReference(reference, fingerprint string) *providerv0.OpaqueReference {
	return &providerv0.OpaqueReference{Reference: reference, Purpose: providerv0.CredentialPurpose_CREDENTIAL_PURPOSE_RUNTIME, SafeFingerprint: fingerprint}
}

func hasAction(plan *providerv0.OrderedPlan, id string) bool {
	for _, action := range plan.GetActions() {
		if action.GetActionId() == id {
			return true
		}
	}
	return false
}

func actionByID(t *testing.T, plan *providerv0.OrderedPlan, id string) *providerv0.PlanAction {
	t.Helper()
	for _, action := range plan.GetActions() {
		if action.GetActionId() == id {
			return action
		}
	}
	t.Fatalf("action %s not found", id)
	return nil
}

func assertFeatureFlagsOutput(t *testing.T, output *providerv0.OutputProposal, contractID string) {
	t.Helper()
	if output.GetContract() != contractID || len(output.GetValues()) != 5 {
		t.Fatalf("invalid feature flags output: %v", output)
	}
	endpointKey := "FEATURE_FLAGS_SERVER_ENDPOINT"
	credentialKey := "FEATURE_FLAGS_SERVER_CREDENTIAL"
	forbiddenEndpointKey := "FEATURE_FLAGS_EDGE_ENDPOINT"
	forbiddenCredentialKey := "FEATURE_FLAGS_BROWSER_CREDENTIAL"
	wantEndpoint := endpointReference("server", "server")
	wantFingerprint := serverFingerprint
	if contractID == featureFlagsBrowserContract {
		endpointKey = "FEATURE_FLAGS_EDGE_ENDPOINT"
		credentialKey = "FEATURE_FLAGS_BROWSER_CREDENTIAL"
		forbiddenEndpointKey = "FEATURE_FLAGS_SERVER_ENDPOINT"
		forbiddenCredentialKey = "FEATURE_FLAGS_SERVER_CREDENTIAL"
		wantEndpoint = endpointReference("edge", "edge")
		wantFingerprint = browserFingerprint
	}
	if got := output.GetValues()[endpointKey].GetPublicValue().GetStringValue(); got != wantEndpoint {
		t.Fatalf("%s = %q, want %q", endpointKey, got, wantEndpoint)
	}
	reference := output.GetValues()[credentialKey].GetOpaqueReference()
	if got := reference.GetSafeFingerprint(); got != wantFingerprint {
		t.Fatalf("%s fingerprint = %q", credentialKey, got)
	}
	if output.GetValues()[credentialKey].GetPublicValue() != nil {
		t.Fatalf("%s was projected as public bytes", credentialKey)
	}
	if _, exists := output.GetValues()[forbiddenEndpointKey]; exists {
		t.Fatalf("%s crossed the consumer boundary", forbiddenEndpointKey)
	}
	if _, exists := output.GetValues()[forbiddenCredentialKey]; exists {
		t.Fatalf("%s crossed the consumer boundary", forbiddenCredentialKey)
	}
	if _, exists := output.GetValues()["FEATURE_FLAGS_MANAGEMENT_CREDENTIAL"]; exists {
		t.Fatal("management credential was projected")
	}
	registry := configuration.NewRegistry()
	if err := registry.ValidateProposal(output); err != nil {
		t.Fatalf("feature flags proposal validation: %v", err)
	}
	contract, err := registry.Lookup(contractID)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]configuration.Value{}
	for name, outputValue := range output.GetValues() {
		declaration := contract.Keys[name]
		value := configuration.Value{
			Type: declaration.Type, Classification: declaration.ClassificationFloor,
			CredentialPurpose: declaration.CredentialPurpose, BrowserExposure: declaration.BrowserExposure,
			Consumer: declaration.Consumer, MutatedBy: configuration.MutatorProvider,
			Provenance: map[configuration.Provenance]string{
				configuration.ProvenanceProvider: "codefly.dev/unleash",
				configuration.ProvenanceBinding:  "workspace/production/feature-flags",
				configuration.ProvenanceArtifact: "sha256:artifact",
			},
		}
		if outputValue.GetOpaqueReference() != nil {
			value.OpaqueReference = outputValue.GetOpaqueReference().GetReference()
		} else {
			value.String = outputValue.GetPublicValue().GetStringValue()
		}
		if declaration.Type == configuration.ValueEndpointReference {
			value.MutatedBy = configuration.MutatorHost
			value.Provenance[configuration.ProvenanceHost] = "host-admitted-endpoint"
		}
		values[name] = value
	}
	if err := registry.Validate(contractID, values); err != nil {
		t.Fatalf("%s contract validation: %v", contractID, err)
	}
}

func providerStateWithTokens(t *testing.T, server *Server, providerContext *providerv0.ProviderContext, marker string) *providerv0.ProviderState {
	t.Helper()
	action, _ := sdkNoOp("state")
	plan := &providerv0.OrderedPlan{PlanDigest: "sha256:plan", Actions: []*providerv0.PlanAction{action}}
	providerContext.Operation.ActionId = action.GetActionId()
	state := server.nextState(&providerv0.ApplyActionRequest{Context: providerContext, Plan: plan, Action: action}, nil)
	state.GetV1().ProviderOwnedFields = map[string]*providerv0.PublicValue{
		"server_resource_name":    publicString(tokenName(marker, "server")),
		"server_remote_identity":  publicString("alias:server-token-id"),
		"browser_resource_name":   publicString(tokenName(marker, "browser")),
		"browser_remote_identity": publicString("alias:browser-token-id"),
	}
	return state
}

func sdkNoOp(id string) (*providerv0.PlanAction, error) {
	return &providerv0.PlanAction{ActionId: id, Type: providerv0.ActionType_ACTION_TYPE_NO_OP, ResourceType: actionResource}, nil
}

func sdkProjectOutput(id string, proposal *providerv0.OutputProposal) (*providerv0.PlanAction, error) {
	return &providerv0.PlanAction{ActionId: id, Type: providerv0.ActionType_ACTION_TYPE_PROJECT_OUTPUT, ResourceType: "project-output", Output: proposal}, nil
}
