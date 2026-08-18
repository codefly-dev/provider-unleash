package provider

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	providerv0 "github.com/codefly-dev/core/generated/go/codefly/services/provider/v0"
	"github.com/codefly-dev/core/provider/sdk"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type filteredFields map[string]*providerv0.PublicValue

func (fields filteredFields) string(path string) string { return fields[path].GetStringValue() }

func (fields filteredFields) boolean(path string) bool { return fields[path].GetBoolValue() }

func (fields filteredFields) indices(prefix, suffix string) []int {
	seen := map[int]bool{}
	for path := range fields {
		rest, ok := strings.CutPrefix(path, prefix+"[")
		if !ok {
			continue
		}
		digits, tail, ok := strings.Cut(rest, "]")
		if !ok || tail != suffix {
			continue
		}
		index, err := strconv.Atoi(digits)
		if err == nil {
			seen[index] = true
		}
	}
	indices := make([]int, 0, len(seen))
	for index := range seen {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	return indices
}

func (fields filteredFields) strings(prefix string) []string {
	indices := fields.indices(prefix, "")
	values := make([]string, 0, len(indices))
	for _, index := range indices {
		values = append(values, fields.string(fmt.Sprintf("%s[%d]", prefix, index)))
	}
	return values
}

func (fields filteredFields) tokenIdentity(prefix string) string {
	if alias := fields.string(prefix + ".alias"); alias != "" {
		return "alias:" + alias
	}
	if createdAt := fields.string(prefix + ".createdAt"); createdAt != "" {
		return "created-at:" + createdAt
	}
	return ""
}

func (s *Server) Observe(ctx context.Context, request *providerv0.ObserveRequest) (*providerv0.ObserveResponse, error) {
	if s.host == nil {
		return nil, status.Error(codes.FailedPrecondition, "provider host callback channel is not attached")
	}
	providerContext := request.GetContext()
	origins, err := endpointOrigins(providerContext)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	if err := s.checkpoint(ctx, providerContext, "observe", "", ""); err != nil {
		return nil, err
	}
	in := parseInputs(providerContext.GetOffline().GetInput())
	marker := ownershipMarker(providerContext.GetOffline().GetBinding())
	accountID := providerContext.GetOffline().GetAccountIdentity()
	if accountID == "" {
		accountID = origins["admin"].GetHost()
	}
	resources := []*providerv0.MaterialResourceObservation{
		endpointResource(accountID, "server", endpointReference("server", "server")),
		endpointResource(accountID, "edge", endpointReference("edge", "edge")),
	}
	complete := true
	failedAt := ""
	var diagnostics []*basev0.FailureDiagnostic

	read := func(descriptorID string, path, query map[string]*providerv0.PublicValue, notFoundIsAbsent bool) filteredFields {
		if !complete {
			return nil
		}
		planned, requestErr := s.plannedRequest(descriptorID, origins["admin"], path, query, nil, "")
		if requestErr != nil {
			err = requestErr
			complete = false
			failedAt = descriptorID
			return nil
		}
		response, requestErr := s.execute(ctx, providerContext, origins["admin"], planned, "observe-"+descriptorID)
		if requestErr != nil {
			err = requestErr
			complete = false
			failedAt = descriptorID
			return nil
		}
		if notFoundIsAbsent && response.GetDelivery() == providerv0.DeliveryState_DELIVERY_STATE_RESPONSE_RECEIVED && response.GetStatusCode() == 404 {
			return filteredFields{}
		}
		if value := responseDiagnostic(response); value != nil {
			diagnostics = append(diagnostics, value)
			complete = false
			failedAt = descriptorID
			return nil
		}
		decoded, requestErr := sdk.DecodeFilteredResponse(response)
		if requestErr != nil {
			err = requestErr
			complete = false
			failedAt = descriptorID
			return nil
		}
		return filteredFields(decoded)
	}

	overview := read("project.overview", map[string]*providerv0.PublicValue{"resource_id": publicString(in.ProjectID)}, nil, true)
	if complete && overview.string("$.name") != "" {
		ownership := providerv0.Ownership_OWNERSHIP_UNMANAGED
		if overview.string("$.description") == marker {
			ownership = providerv0.Ownership_OWNERSHIP_OWNED
		}
		project := observedResource(accountID, resourceProject, in.ProjectID, ownership, map[string]*providerv0.PublicValue{
			"name":        publicString(overview.string("$.name")),
			"description": publicString(overview.string("$.description")),
		})
		resources = append(resources, project)
		for _, index := range overview.indices("$.environments", ".environment") {
			if overview.string(fmt.Sprintf("$.environments[%d].environment", index)) == in.EnvironmentID {
				resources = append(resources, observedResource(accountID, resourceProjectEnvironment, in.ProjectID+":"+in.EnvironmentID, providerv0.Ownership_OWNERSHIP_OBSERVED, nil))
			}
		}
		var featureCount int64
		for _, index := range overview.indices("$.featureTypeCounts", ".count") {
			featureCount += overview[fmt.Sprintf("$.featureTypeCounts[%d].count", index)].GetIntegerValue()
		}
		project.ProviderOwnedFields["feature_count"] = publicInteger(featureCount)
	}

	environment := read("environment.get", map[string]*providerv0.PublicValue{"resource_id": publicString(in.EnvironmentID)}, nil, true)
	if complete && environment.string("$.name") == in.EnvironmentID {
		resources = append(resources, observedResource(accountID, resourceEnvironment, in.EnvironmentID, providerv0.Ownership_OWNERSHIP_OBSERVED, map[string]*providerv0.PublicValue{
			"type":    publicString(environment.string("$.type")),
			"enabled": publicBool(environment.boolean("$.enabled")),
		}))
	}

	application := read("application.get", map[string]*providerv0.PublicValue{"resource_id": publicString(in.ApplicationID)}, nil, true)
	if complete && application.string("$.appName") == in.ApplicationID {
		ownership := providerv0.Ownership_OWNERSHIP_UNMANAGED
		if application.string("$.url") == applicationOwnershipURL(marker) {
			ownership = providerv0.Ownership_OWNERSHIP_OWNED
		}
		resources = append(resources, observedResource(accountID, resourceApplication, in.ApplicationID, ownership, map[string]*providerv0.PublicValue{
			"url": publicString(application.string("$.url")),
		}))
	}

	readToken := func(kind, resourceType string) {
		name := tokenName(marker, kind)
		tokens := read("token.get", map[string]*providerv0.PublicValue{"resource_id": publicString(name)}, nil, false)
		if !complete {
			return
		}
		indices := tokens.indices("$.tokens", ".tokenName")
		if len(indices) == 0 {
			return
		}
		index := indices[0]
		prefix := fmt.Sprintf("$.tokens[%d]", index)
		ownership := providerv0.Ownership_OWNERSHIP_UNMANAGED
		stateField := "server_resource_name"
		identityField := "server_remote_identity"
		if resourceType == resourceBrowserToken {
			stateField = "browser_resource_name"
			identityField = "browser_remote_identity"
		}
		observedIdentity := tokens.tokenIdentity(prefix)
		if observedIdentity != "" && len(indices) == 1 && tokens.string(prefix+".tokenName") == name &&
			request.GetState().GetV1().GetProviderOwnedFields()[stateField].GetStringValue() == name &&
			request.GetState().GetV1().GetProviderOwnedFields()[identityField].GetStringValue() == observedIdentity {
			ownership = providerv0.Ownership_OWNERSHIP_OWNED
		}
		resources = append(resources, observedResource(accountID, resourceType, name, ownership, map[string]*providerv0.PublicValue{
			"type":        publicString(tokens.string(prefix + ".type")),
			"environment": publicString(tokens.string(prefix + ".environment")),
			"projects":    publicStrings(tokens.strings(prefix + ".projects")...),
		}))
	}
	readToken("server", resourceServerToken)
	readToken("browser", resourceBrowserToken)
	if err != nil {
		diagnostics = append(diagnostics, diagnostic(basev0.FailureDiagnostic_WARNING, diagnosticIncomplete, "Unleash observation stopped before all bounded resources were read"))
	}
	return sdk.Observation(&providerv0.MaterialObservation{
		AccountIdentity: accountID, Mode: providerContext.GetOffline().GetMode(), Complete: complete,
		NextCursor: failedAt, Resources: resources,
	}, &providerv0.VolatileObservation{Diagnostics: diagnostics})
}

func endpointResource(accountID, endpointID, value string) *providerv0.MaterialResourceObservation {
	return observedResource(accountID, resourceEndpoint, endpointID, providerv0.Ownership_OWNERSHIP_OBSERVED,
		map[string]*providerv0.PublicValue{"endpoint": publicString(value)})
}

func endpointReference(originRuleID, endpointID string) string {
	return "endpoint://" + originRuleID + "/" + endpointID
}

func observedResource(accountID, resourceType, remoteID string, ownership providerv0.Ownership, fields map[string]*providerv0.PublicValue) *providerv0.MaterialResourceObservation {
	return &providerv0.MaterialResourceObservation{
		Identity: remoteIdentity(accountID, resourceType, remoteID), Ownership: ownership, ProviderOwnedFields: fields,
	}
}
