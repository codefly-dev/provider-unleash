package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	providerv0 "github.com/codefly-dev/core/generated/go/codefly/services/provider/v0"
	"github.com/codefly-dev/core/provider/configuration"
	"github.com/codefly-dev/core/provider/sdk"
)

const (
	resourceProject             = "unleash.project"
	resourceEnvironment         = "unleash.environment"
	resourceProjectEnvironment  = "unleash.project-environment"
	resourceApplication         = "unleash.application"
	resourceServerToken         = "unleash.server-token"
	resourceBrowserToken        = "unleash.browser-token"
	resourceEndpoint            = "unleash.endpoint"
	featureFlagsContract        = configuration.FeatureFlagsContract
	featureFlagsBrowserContract = configuration.FeatureFlagsBrowserContract
	actionResource              = "unleash.binding"
)

var identifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

var inputKeys = map[string]bool{
	"project_id": true, "project_name": true, "environment_id": true,
	"environment_type": true, "application_id": true, "provider_mode": true,
	"import_resource_type": true, "import_remote_id": true,
	"server_credential_fingerprint": true, "browser_credential_fingerprint": true,
	"intent": true,
}

type inputs struct {
	ProjectID                    string
	ProjectName                  string
	EnvironmentID                string
	EnvironmentType              string
	ApplicationID                string
	ProviderMode                 string
	ImportResourceType           string
	ImportRemoteID               string
	ServerCredentialFingerprint  string
	BrowserCredentialFingerprint string
	Intent                       string
}

func parseInputs(values map[string]*providerv0.PublicValue) inputs {
	return inputs{
		ProjectID:                    strings.TrimSpace(stringValue(values["project_id"])),
		ProjectName:                  strings.TrimSpace(stringValue(values["project_name"])),
		EnvironmentID:                strings.TrimSpace(stringValue(values["environment_id"])),
		EnvironmentType:              strings.TrimSpace(stringValue(values["environment_type"])),
		ApplicationID:                strings.TrimSpace(stringValue(values["application_id"])),
		ProviderMode:                 strings.TrimSpace(stringValue(values["provider_mode"])),
		ImportResourceType:           strings.TrimSpace(stringValue(values["import_resource_type"])),
		ImportRemoteID:               strings.TrimSpace(stringValue(values["import_remote_id"])),
		ServerCredentialFingerprint:  strings.TrimSpace(stringValue(values["server_credential_fingerprint"])),
		BrowserCredentialFingerprint: strings.TrimSpace(stringValue(values["browser_credential_fingerprint"])),
		Intent:                       strings.TrimSpace(stringValue(values["intent"])),
	}
}

func (in inputs) validate() []*basev0.FailureDiagnostic {
	var diagnostics []*basev0.FailureDiagnostic
	invalid := func(message string) {
		diagnostics = append(diagnostics, diagnostic(basev0.FailureDiagnostic_ERROR, diagnosticInvalid, message))
	}
	for _, field := range []struct{ name, value string }{
		{"project_id", in.ProjectID}, {"project_name", in.ProjectName},
		{"environment_id", in.EnvironmentID}, {"application_id", in.ApplicationID},
	} {
		if !identifierPattern.MatchString(field.value) {
			invalid(fmt.Sprintf("%s must be a 1-100 character Unleash identifier", field.name))
		}
	}
	switch in.EnvironmentType {
	case "development", "test", "preproduction", "production":
	default:
		invalid("environment_type must be development, test, preproduction, or production")
	}
	if in.ProviderMode != "edge" {
		invalid("provider_mode must be edge")
	}
	if in.Intent == "" {
		in.Intent = "apply"
	}
	if in.Intent != "apply" && in.Intent != "destroy" {
		invalid("intent must be apply or destroy")
	}
	if (in.ImportResourceType == "") != (in.ImportRemoteID == "") {
		invalid("import_resource_type and import_remote_id must be supplied together")
	}
	if in.ImportResourceType != "" && in.ImportResourceType != resourceProject && in.ImportResourceType != resourceApplication {
		invalid("only a project or application can be imported")
	}
	if in.ImportResourceType == resourceProject && in.ImportRemoteID != in.ProjectID {
		invalid("project import must name the exact declared project_id")
	}
	if in.ImportResourceType == resourceApplication && in.ImportRemoteID != in.ApplicationID {
		invalid("application import must name the exact declared application_id")
	}
	for _, field := range []struct{ name, fingerprint string }{
		{"server_credential_fingerprint", in.ServerCredentialFingerprint},
		{"browser_credential_fingerprint", in.BrowserCredentialFingerprint},
	} {
		if field.fingerprint != "" && !digestPattern.MatchString(field.fingerprint) {
			invalid(field.name + " must be a canonical sha256 digest")
		}
	}
	if in.ServerCredentialFingerprint != "" && in.ServerCredentialFingerprint == in.BrowserCredentialFingerprint {
		invalid("server and browser credential fingerprints must differ")
	}
	return diagnostics
}

func validateRawInputs(values map[string]*providerv0.PublicValue) []*basev0.FailureDiagnostic {
	var diagnostics []*basev0.FailureDiagnostic
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := values[key]
		if !inputKeys[key] {
			diagnostics = append(diagnostics, diagnostic(basev0.FailureDiagnostic_ERROR, diagnosticInvalid, fmt.Sprintf("input %q is not declared", key)))
			continue
		}
		if _, ok := value.GetKind().(*providerv0.PublicValue_StringValue); !ok {
			diagnostics = append(diagnostics, diagnostic(basev0.FailureDiagnostic_ERROR, diagnosticInvalid, fmt.Sprintf("input %q must be a string", key)))
			continue
		}
		if configuration.LooksSecret(value.GetStringValue()) {
			diagnostics = append(diagnostics, diagnostic(basev0.FailureDiagnostic_ERROR, diagnosticInvalid, fmt.Sprintf("input %q contains secret-shaped bytes; use a host credential handle or opaque reference", key)))
		}
	}
	return diagnostics
}

func ownershipMarker(binding *providerv0.BindingAddress) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		binding.GetWorkspaceId(), binding.GetEnvironmentId(), binding.GetBindingId(),
	}, "\x00")))
	return "codefly:owner:" + hex.EncodeToString(sum[:12])
}

func applicationOwnershipURL(marker string) string {
	return "https://codefly.dev/ownership/" + strings.TrimPrefix(marker, "codefly:owner:")
}

func tokenName(marker, kind string) string {
	return "codefly-" + strings.TrimPrefix(marker, "codefly:owner:") + "-" + kind
}

func stringValue(value *providerv0.PublicValue) string {
	if value == nil {
		return ""
	}
	return value.GetStringValue()
}

func publicString(value string) *providerv0.PublicValue {
	return &providerv0.PublicValue{Kind: &providerv0.PublicValue_StringValue{StringValue: value}}
}

func publicBool(value bool) *providerv0.PublicValue {
	return &providerv0.PublicValue{Kind: &providerv0.PublicValue_BoolValue{BoolValue: value}}
}

func publicInteger(value int64) *providerv0.PublicValue {
	return &providerv0.PublicValue{Kind: &providerv0.PublicValue_IntegerValue{IntegerValue: value}}
}

func publicStrings(values ...string) *providerv0.PublicValue {
	items := make([]*providerv0.PublicValue, 0, len(values))
	for _, value := range values {
		items = append(items, publicString(value))
	}
	return &providerv0.PublicValue{Kind: &providerv0.PublicValue_ListValue{ListValue: &providerv0.PublicList{Values: items}}}
}

func diagnostic(severity basev0.FailureDiagnostic_Severity, code, message string) *basev0.FailureDiagnostic {
	value, err := sdk.Diagnostic(severity, diagnosticNamespace, strings.TrimPrefix(code, diagnosticNamespace), message)
	if err != nil {
		return &basev0.FailureDiagnostic{Severity: severity, Code: code, Message: message}
	}
	return value
}

func hasErrors(diagnostics []*basev0.FailureDiagnostic) bool {
	for _, value := range diagnostics {
		if value.GetSeverity() == basev0.FailureDiagnostic_ERROR {
			return true
		}
	}
	return false
}

func remoteIdentity(accountID, resourceType, remoteID string) *providerv0.RemoteIdentity {
	return &providerv0.RemoteIdentity{Provider: "unleash", AccountId: accountID, ResourceType: resourceType, RemoteId: remoteID}
}
