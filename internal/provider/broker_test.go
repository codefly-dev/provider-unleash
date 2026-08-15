package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	providerv0 "github.com/codefly-dev/core/generated/go/codefly/services/provider/v0"
	"github.com/codefly-dev/core/network/urlguard"
	"github.com/codefly-dev/core/provider/broker"
	"github.com/codefly-dev/core/provider/canonical"
	"github.com/codefly-dev/core/provider/credentials"
	"github.com/codefly-dev/core/provider/manifest"
	"github.com/codefly-dev/core/provider/responsepolicy"
	"github.com/codefly-dev/core/provider/sdk"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const (
	brokerManagementToken = "management_0123456789abcdefghijklmnopqrstuvwxyz"
	brokerRuntimeToken    = "starter:production.0123456789abcdefghijklmnopqrstuvwxyz"
)

type brokerTestHost struct {
	manifest    *manifest.Manifest
	vault       *credentials.Vault
	sink        *brokerTestSink
	checkpoints map[string]*providerv0.ActionCheckpoint
	serverAddr  string
	now         time.Time
}

func newBrokerTestHost(t *testing.T, server *httptest.Server) *brokerTestHost {
	t.Helper()
	contents, err := os.ReadFile("../../provider.codefly.yaml")
	if err != nil {
		t.Fatal(err)
	}
	providerManifest, err := manifest.Load(contents)
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	now := time.Date(2026, time.August, 14, 12, 0, 0, 0, time.UTC)
	return &brokerTestHost{
		manifest: providerManifest, vault: credentials.NewVault().WithClock(func() time.Time { return now }),
		sink: &brokerTestSink{}, checkpoints: map[string]*providerv0.ActionCheckpoint{},
		serverAddr: server.Listener.Addr().String(), now: now,
	}
}

func (host *brokerTestHost) ExecuteRequest(ctx context.Context, request *providerv0.ExecuteRequestRequest, _ ...grpc.CallOption) (*providerv0.ExecuteRequestResponse, error) {
	descriptor, err := host.requestDescriptor(request.GetRequest().GetRequestDescriptorId())
	if err != nil {
		return nil, err
	}
	remoteID := request.GetRequest().GetBody()["tokenName"].GetStringValue()
	action := &providerv0.PlanAction{
		ActionId: request.GetContext().GetOperation().GetActionId(), Type: providerv0.ActionType_ACTION_TYPE_CREATE,
		ResourceType: descriptor.ResourceType, ProspectiveRemoteId: remoteID,
		Ownership: providerv0.Ownership_OWNERSHIP_OWNED, Requests: []*providerv0.PlannedRequest{request.GetRequest()},
	}
	if err := canonical.ValidatePlanAction(action); err != nil {
		return nil, err
	}
	origin := urlguard.Origin{Scheme: request.GetOrigin().GetScheme(), Host: request.GetOrigin().GetHost(), Port: request.GetOrigin().GetPort()}
	handle, err := host.vault.Mint(brokerManagementToken, credentials.Scope{
		Principal: "test", Organization: "test", ArtifactDigest: "sha256:" + strings.Repeat("a", 64),
		Binding: request.GetContext().GetOffline().GetBinding(), PlanID: request.GetContext().GetOperation().GetPlanId(),
		ActionID: action.GetActionId(), RequestDigest: request.GetRequest().GetRequestDigest(),
		Purpose: providerv0.CredentialPurpose_CREDENTIAL_PURPOSE_MANAGEMENT, Origin: origin,
		Method: request.GetRequest().GetMethod(), Injection: credentials.Injection{Kind: credentials.InjectBearer},
		MaxUses: 1, TTL: time.Hour,
	})
	if err != nil {
		return nil, err
	}
	bound := proto.Clone(request).(*providerv0.ExecuteRequestRequest)
	bound.Context.Credentials = []*providerv0.CredentialHandle{handle}
	bound.CredentialHandles = []*providerv0.CredentialHandle{handle}
	session, err := broker.New(broker.Config{
		Manifest: host.manifest, Action: action, Binding: request.GetContext().GetOffline().GetBinding(),
		Budget: request.GetContext().GetBudget(), Vault: host.vault, Sink: host.sink, Checkpoints: host,
		ClientFor: func(urlguard.Origin, urlguard.Resolution) *http.Client {
			return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, host.serverAddr)
			}}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		},
		Deadlines: urlguard.DefaultDeadlines(), Now: func() time.Time { return host.now },
	})
	if err != nil {
		return nil, err
	}
	return session.Execute(ctx, bound)
}

func (host *brokerTestHost) requestDescriptor(id string) (manifest.RequestDescriptor, error) {
	for _, descriptor := range host.manifest.Requests {
		if descriptor.ID == id {
			return descriptor, nil
		}
	}
	return manifest.RequestDescriptor{}, fmt.Errorf("descriptor %q not found", id)
}

func (host *brokerTestHost) RecordCheckpoint(_ context.Context, request *providerv0.RecordCheckpointRequest, _ ...grpc.CallOption) (*providerv0.RecordCheckpointResponse, error) {
	host.checkpoints[request.GetCheckpoint().GetOperation().GetActionId()] = proto.Clone(request.GetCheckpoint()).(*providerv0.ActionCheckpoint)
	return &providerv0.RecordCheckpointResponse{Durable: true}, nil
}

func (host *brokerTestHost) Latest(_ context.Context, operation *providerv0.OperationIdentity) (*providerv0.ActionCheckpoint, error) {
	return host.checkpoints[operation.GetActionId()], nil
}

func (host *brokerTestHost) ResolveCapture(context.Context, *providerv0.ResolveCaptureRequest, ...grpc.CallOption) (*providerv0.ResolveCaptureResponse, error) {
	return &providerv0.ResolveCaptureResponse{}, nil
}

func (host *brokerTestHost) ProposeOutput(context.Context, *providerv0.ProposeOutputRequest, ...grpc.CallOption) (*providerv0.ProposeOutputResponse, error) {
	return &providerv0.ProposeOutputResponse{Durable: true}, nil
}

type brokerTestSink struct {
	stored []string
}

func (sink *brokerTestSink) Put(_ context.Context, target responsepolicy.SinkTarget, secret string) (*providerv0.OpaqueReference, error) {
	sink.stored = append(sink.stored, secret)
	fingerprint := sha256.Sum256([]byte(secret))
	return &providerv0.OpaqueReference{
		Reference: "capture://unleash-token", Purpose: target.Purpose,
		SafeFingerprint: "sha256:" + hex.EncodeToString(fingerprint[:]),
	}, nil
}

func TestBrokerCapturesTokenAndRejectsHostileRequest(t *testing.T) {
	hits := 0
	standIn := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		hits++
		if request.Method != http.MethodPost || request.URL.Path != "/api/admin/api-tokens" {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer "+brokerManagementToken {
			t.Error("management credential was not host-injected")
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if body["tokenName"] == nil || body["token_name"] != nil {
			t.Errorf("token name field casing = %v", body)
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"tokenName": body["tokenName"], "type": "backend", "environment": "production",
			"projects": []string{"starter"}, "createdAt": "2026-08-14T12:00:00Z", "alias": "broker-token-id", "secret": brokerRuntimeToken,
			"hostileManagementEcho": brokerManagementToken,
		})
	}))
	t.Cleanup(standIn.Close)

	host := newBrokerTestHost(t, standIn)
	server := testServer(t, host)
	providerContext := testProviderContext(testInputs("apply"))
	for _, origin := range providerContext.AdmittedOrigins {
		if origin.GetOriginRuleId() != "admin" {
			continue
		}
		origin.Host = "localhost"
		origin.PrivateNetworkClass = providerv0.PrivateNetworkClass_PRIVATE_NETWORK_CLASS_LOOPBACK
		var err error
		origin.AdmissionDigest, err = canonical.AdmittedOriginDigest(origin)
		if err != nil {
			t.Fatal(err)
		}
	}
	providerContext.Budget = &providerv0.RequestBudget{RequestCount: 10, RequestBytes: 16384, ResponseBytes: 262144}
	providerContext.Operation.PlanId = "plan-broker"
	providerContext.Operation.ActionId = "token-server-create"
	name := tokenName(ownershipMarker(testBinding()), "server")
	action, err := sdk.NewCreateAction("token-server-create", 0, actionResource, name)
	if err != nil {
		t.Fatal(err)
	}
	action.Ownership = providerv0.Ownership_OWNERSHIP_OWNED
	plan := &providerv0.OrderedPlan{PlanId: "plan-broker", Actions: []*providerv0.PlanAction{action}}
	result, err := server.ApplyAction(context.Background(), &providerv0.ApplyActionRequest{Context: providerContext, Plan: plan, Action: action})
	if err != nil {
		t.Fatalf("apply through broker: %v", err)
	}
	if hits != 1 || len(host.sink.stored) != 1 || host.sink.stored[0] != brokerRuntimeToken {
		t.Fatalf("broker delivery hits=%d captures=%v", hits, host.sink.stored)
	}
	encoded, err := protojson.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), brokerRuntimeToken) || strings.Contains(string(encoded), brokerManagementToken) {
		t.Fatal("credential bytes escaped the broker boundary")
	}
	if len(result.GetReceipt().GetCaptureReferences()) != 1 || result.GetReceipt().GetCaptureReferences()[0].GetPurpose() != providerv0.CredentialPurpose_CREDENTIAL_PURPOSE_RUNTIME {
		t.Fatalf("capture references = %v", result.GetReceipt().GetCaptureReferences())
	}

	admin := providerContext.GetAdmittedOrigins()[0]
	for _, origin := range providerContext.GetAdmittedOrigins() {
		if origin.GetOriginRuleId() == "admin" {
			admin = origin
		}
	}
	hostile, err := server.plannedRequest("token.create", admin, nil, nil, map[string]*providerv0.PublicValue{
		"tokenName": publicString(name), "type": publicString("backend"), "environment": publicString("production"),
		"projects": publicStrings("starter"), "hostile": publicString("enabled"),
	}, "hostile")
	if err != nil {
		t.Fatal(err)
	}
	providerContext.Operation.ActionId = "hostile-token"
	_, err = host.ExecuteRequest(context.Background(), &providerv0.ExecuteRequestRequest{
		Context: providerContext, RequestId: "hostile", Request: hostile, Origin: admin,
	})
	if err == nil || hits != 1 {
		t.Fatal("manifest-external body field reached Unleash")
	}
}
