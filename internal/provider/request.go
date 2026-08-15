package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"strings"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	providerv0 "github.com/codefly-dev/core/generated/go/codefly/services/provider/v0"
	"github.com/codefly-dev/core/provider/canonical"
	"github.com/codefly-dev/core/provider/manifest"
)

var methods = map[string]providerv0.HTTPMethod{
	"GET":    providerv0.HTTPMethod_HTTP_METHOD_GET,
	"POST":   providerv0.HTTPMethod_HTTP_METHOD_POST,
	"PUT":    providerv0.HTTPMethod_HTTP_METHOD_PUT,
	"DELETE": providerv0.HTTPMethod_HTTP_METHOD_DELETE,
}

func (s *Server) descriptor(id string) (manifest.RequestDescriptor, error) {
	for _, descriptor := range s.manifest.Requests {
		if descriptor.ID == id {
			return descriptor, nil
		}
	}
	return manifest.RequestDescriptor{}, fmt.Errorf("request descriptor %q is not packaged", id)
}

func (s *Server) plannedRequest(id string, origin *providerv0.AdmittedOrigin, path, query, body map[string]*providerv0.PublicValue, idempotencyKey string) (*providerv0.PlannedRequest, error) {
	descriptor, err := s.descriptor(id)
	if err != nil {
		return nil, err
	}
	descriptorDigest, err := manifest.RequestDescriptorDigest(descriptor)
	if err != nil {
		return nil, err
	}
	responseDigest, err := s.responsePolicyDigest(descriptor.ResponseSchema)
	if err != nil {
		return nil, err
	}
	request := &providerv0.PlannedRequest{
		RequestDescriptorId: id, RequestDescriptorDigest: descriptorDigest,
		Method: methods[descriptor.Method], AdmittedOriginDigest: origin.GetAdmissionDigest(),
		PathParameters: path, Query: query, Body: body, ResponsePolicyDigest: responseDigest,
		CredentialPurposes: []providerv0.CredentialPurpose{providerv0.CredentialPurpose_CREDENTIAL_PURPOSE_MANAGEMENT},
		IdempotencyKey:     idempotencyKey,
	}
	return canonical.BindPlannedRequestDigest(request)
}

func (s *Server) responsePolicyDigest(schemaID string) (string, error) {
	for _, schema := range s.manifest.ResponseSchemas {
		if schema.ID != schemaID {
			continue
		}
		encoded, err := json.Marshal(schema)
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256(encoded)
		return "sha256:" + hex.EncodeToString(sum[:]), nil
	}
	return "", fmt.Errorf("response schema %q is not packaged", schemaID)
}

func endpointOrigins(providerContext *providerv0.ProviderContext) (map[string]*providerv0.AdmittedOrigin, error) {
	semantic := map[string]string{}
	for _, endpoint := range providerContext.GetEndpoints() {
		semantic[endpoint.GetEndpointId()] = endpoint.GetOriginRuleId()
	}
	for _, id := range []string{"admin", "server", "edge"} {
		if semantic[id] != id {
			return nil, fmt.Errorf("semantic endpoint %q must reference origin rule %q", id, id)
		}
	}
	origins := map[string]*providerv0.AdmittedOrigin{}
	for _, origin := range providerContext.GetAdmittedOrigins() {
		if semantic[origin.GetOriginRuleId()] == origin.GetOriginRuleId() {
			origins[origin.GetOriginRuleId()] = origin
		}
	}
	for _, id := range []string{"admin", "server", "edge"} {
		if origins[id] == nil {
			return nil, fmt.Errorf("host admitted no origin for semantic endpoint %q", id)
		}
	}
	return origins, nil
}

func originString(origin *providerv0.AdmittedOrigin) string {
	host := origin.GetHost()
	port := origin.GetPort()
	if port != 0 && !((origin.GetScheme() == "https" && port == 443) || (origin.GetScheme() == "http" && port == 80)) {
		host = net.JoinHostPort(host, fmt.Sprint(port))
	}
	return origin.GetScheme() + "://" + host
}

func managementHandle(providerContext *providerv0.ProviderContext) (*providerv0.CredentialHandle, error) {
	for _, handle := range providerContext.GetCredentials() {
		if handle.GetPurpose() == providerv0.CredentialPurpose_CREDENTIAL_PURPOSE_MANAGEMENT {
			return handle, nil
		}
	}
	return nil, fmt.Errorf("host provided no management credential handle")
}

func (s *Server) execute(ctx context.Context, providerContext *providerv0.ProviderContext, origin *providerv0.AdmittedOrigin, planned *providerv0.PlannedRequest, requestID string) (*providerv0.ExecuteRequestResponse, error) {
	handle, err := managementHandle(providerContext)
	if err != nil {
		return nil, err
	}
	return s.host.ExecuteRequest(ctx, &providerv0.ExecuteRequestRequest{
		Context: providerContext, RequestId: requestID, Request: planned,
		Origin: origin, CredentialHandles: []*providerv0.CredentialHandle{handle},
	})
}

func (s *Server) checkpoint(ctx context.Context, providerContext *providerv0.ProviderContext, suffix, key, prospectiveID string) error {
	operation := providerContext.GetOperation()
	response, err := s.host.RecordCheckpoint(ctx, &providerv0.RecordCheckpointRequest{Checkpoint: &providerv0.ActionCheckpoint{
		CheckpointId: "checkpoint-" + operation.GetOperationId() + "-" + suffix,
		Operation:    operation, Delivery: providerv0.DeliveryState_DELIVERY_STATE_NOT_SENT,
		IdempotencyKey: key, ProspectiveRemoteId: prospectiveID,
	}})
	if err != nil {
		return err
	}
	if !response.GetDurable() {
		return fmt.Errorf("host did not durably record the checkpoint")
	}
	return nil
}

func responseDiagnostic(response *providerv0.ExecuteRequestResponse) *basev0.FailureDiagnostic {
	switch response.GetDelivery() {
	case providerv0.DeliveryState_DELIVERY_STATE_RESPONSE_RECEIVED:
		if response.GetStatusCode() >= 200 && response.GetStatusCode() < 300 {
			return nil
		}
		return diagnostic(basev0.FailureDiagnostic_ERROR, diagnosticRemote, fmt.Sprintf("Unleash responded with HTTP %d", response.GetStatusCode()))
	case providerv0.DeliveryState_DELIVERY_STATE_SENT_OUTCOME_UNKNOWN:
		return diagnostic(basev0.FailureDiagnostic_WARNING, diagnosticUnknown, "the request reached Unleash but its outcome is unknown")
	default:
		return diagnostic(basev0.FailureDiagnostic_ERROR, diagnosticRemote, "the request was not sent to Unleash")
	}
}

func idempotencyKey(providerContext *providerv0.ProviderContext, action *providerv0.PlanAction) string {
	return strings.Join([]string{"unleash", providerContext.GetOperation().GetOperationId(), action.GetActionId()}, "-")
}
