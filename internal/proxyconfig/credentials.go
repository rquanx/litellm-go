package proxyconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

type providerCredential struct {
	Name   string            `yaml:"credential_name" json:"credential_name"`
	Values map[string]string `yaml:"credential_values" json:"credential_values"`
	Info   map[string]any    `yaml:"credential_info" json:"credential_info,omitempty"`
}

type modelState struct {
	Models      []deployment                 `json:"models"`
	Credentials map[string]providerCredential `json:"credentials,omitempty"`
	Router      routerPatch                   `json:"router_settings,omitempty"`
	SearchTools []managedSearchTool           `json:"search_tools,omitempty"`
	PassThrough []passThroughConfig          `json:"pass_through_endpoints,omitempty"`
	MCPServers map[string]mcpServerConfig     `json:"mcp_servers,omitempty"`
	MCPToolsets []runtime.MCPToolset          `json:"mcp_toolsets,omitempty"`
	PublicHub publicModelHubState             `json:"public_model_hub,omitzero"`
}

func (state *modelState) UnmarshalJSON(payload []byte) error {
	if bytes.HasPrefix(bytes.TrimSpace(payload), []byte("[")) {
		return json.Unmarshal(payload, &state.Models)
	}
	type modelStateJSON modelState
	return decodeModelJSON(bytes.NewReader(payload), (*modelStateJSON)(state))
}

func combinedCredentials(base, dynamic map[string]providerCredential) map[string]providerCredential {
	credentials := maps.Clone(base)
	if credentials == nil {
		credentials = make(map[string]providerCredential, len(dynamic))
	}
	maps.Copy(credentials, dynamic)
	return credentials
}

func (credential providerCredential) validate() error {
	if strings.TrimSpace(credential.Name) == "" || len(credential.Values) == 0 {
		return errors.New("credential_name and credential_values are required")
	}
	var entry deployment
	fields := entry.credentialFields()
	for field := range credential.Values {
		if _, ok := fields[field]; !ok && field != "vertex_credentials" && field != "vertex_ai_credentials" {
			return errors.New("unsupported provider credential field")
		}
	}
	return nil
}

func (entry deployment) withCredential(credentials map[string]providerCredential) (deployment, error) {
	if entry.Params.CredentialName == "" {
		return entry, nil
	}
	credential, exists := credentials[entry.Params.CredentialName]
	if !exists {
		return deployment{}, errors.New("referenced provider credential is missing")
	}
	for name, target := range entry.credentialFields() {
		if *target == "" {
			*target = credential.Values[name]
		}
	}
	if entry.Params.VertexCredentials == nil && entry.Params.VertexAICredentials == nil {
		if value := credential.Values["vertex_credentials"]; value != "" {
			entry.Params.VertexCredentials = value
		} else if value := credential.Values["vertex_ai_credentials"]; value != "" {
			entry.Params.VertexAICredentials = value
		}
	}
	return entry, nil
}

func (entry *deployment) credentialFields() map[string]*string {
	return map[string]*string{
		"api_key": &entry.Params.APIKey, "api_base": &entry.Params.APIBase, "api_version": &entry.Params.APIVersion,
		"azure_ad_token": &entry.Params.AzureADToken,
		"tenant_id": &entry.Params.AzureTenant, "client_id": &entry.Params.AzureClient,
		"azure_username": &entry.Params.AzureUsername, "azure_password": &entry.Params.AzurePassword,
		"client_secret": &entry.Params.AzureClientSecret, "azure_scope": &entry.Params.AzureScope,
		"azure_credential": &entry.Params.AzureCredential, "azure_authority_host": &entry.Params.AzureAuthority,
		"aws_access_key_id": &entry.Params.AWSAccessKey, "aws_secret_access_key": &entry.Params.AWSSecretKey,
		"aws_session_token": &entry.Params.AWSSessionToken, "aws_region_name": &entry.Params.AWSRegion,
		"aws_profile_name": &entry.Params.AWSProfile, "vertex_project": &entry.Params.VertexProject,
		"aws_role_name": &entry.Params.AWSRole, "aws_session_name": &entry.Params.AWSRoleSession,
		"aws_web_identity_token": &entry.Params.AWSWebIdentity, "aws_sts_endpoint": &entry.Params.AWSSTSEndpoint,
		"aws_external_id": &entry.Params.AWSExternalID,
		"vertex_location": &entry.Params.VertexLocation,
		"aws_bedrock_control_endpoint": &entry.Params.AWSBedrockControlEndpoint,
		"aws_bedrock_agent_endpoint": &entry.Params.AWSBedrockAgentEndpoint,
		"aws_bedrock_agent_runtime_endpoint": &entry.Params.AWSBedrockAgentRuntimeEndpoint,
		"aws_s3vectors_endpoint": &entry.Params.AWSS3VectorsEndpoint,
		"aws_batch_role_arn": &entry.Params.AWSBatchRoleARN,
		"s3_bucket_name": &entry.Params.S3Bucket, "s3_endpoint_url": &entry.Params.S3Endpoint,
		"s3_region_name": &entry.Params.S3Region, "s3_output_bucket_name": &entry.Params.S3OutputBucket,
		"s3_encryption_key_id": &entry.Params.S3EncryptionKey,
		"bucket_name": &entry.Params.GCSBucket, "gcs_endpoint": &entry.Params.GCSEndpoint,
	}
}

func (credential providerCredential) masked() providerCredential {
	values := make(map[string]string, len(credential.Values))
	for name := range credential.Values {
		values[name] = "********"
	}
	credential.Values = values
	return credential
}

func (registry *modelRegistry) registerCredentialRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /credentials", registry.listCredentials)
	mux.HandleFunc("GET /credentials/by_name/{credential_name...}", registry.getCredential)
	mux.HandleFunc("GET /credentials/by_model/{model_id}", registry.getModelCredential)
	mux.HandleFunc("POST /credentials", registry.writeCredential)
	mux.HandleFunc("PATCH /credentials/{credential_name...}", registry.writeCredential)
	mux.HandleFunc("DELETE /credentials/{credential_name...}", registry.deleteCredential)
}

func (registry *modelRegistry) listCredentials(w http.ResponseWriter, r *http.Request) {
	if !registry.allowed(w, r) {
		return
	}
	registry.mu.Lock()
	credentials := combinedCredentials(registry.base.credentials, registry.credentials)
	registry.mu.Unlock()
	result := make([]providerCredential, 0, len(credentials))
	for _, name := range slices.Sorted(maps.Keys(credentials)) {
		result = append(result, credentials[name].masked())
	}
	modelJSON(w, http.StatusOK, map[string]any{"success": true, "credentials": result})
}

func (registry *modelRegistry) getCredential(w http.ResponseWriter, r *http.Request) {
	if !registry.allowed(w, r) {
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.writeMaskedCredential(w, r.PathValue("credential_name"))
}

func (registry *modelRegistry) getModelCredential(w http.ResponseWriter, r *http.Request) {
	if !registry.allowed(w, r) {
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	id := r.PathValue("model_id")
	for _, models := range [][]deployment{registry.base.models, registry.models} {
		if index := modelIndex(models, id); index >= 0 {
			registry.writeMaskedCredential(w, models[index].Params.CredentialName)
			return
		}
	}
	modelError(w, http.StatusNotFound, "Model not found")
}

func (registry *modelRegistry) writeMaskedCredential(w http.ResponseWriter, name string) {
	credential, exists := registry.credentials[name]
	if !exists {
		credential, exists = registry.base.credentials[name]
	}
	if !exists {
		modelError(w, http.StatusNotFound, "Provider credential not found")
		return
	}
	modelJSON(w, http.StatusOK, credential.masked())
}

func (registry *modelRegistry) writeCredential(w http.ResponseWriter, r *http.Request) {
	if !registry.allowed(w, r) {
		return
	}
	var input providerCredential
	if err := decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input); err != nil {
		modelError(w, http.StatusBadRequest, "Invalid provider credential")
		return
	}
	update := r.Method == http.MethodPatch
	if update {
		name := r.PathValue("credential_name")
		if input.Name != "" && input.Name != name {
			modelError(w, http.StatusBadRequest, "Conflicting credential names")
			return
		}
		input.Name = name
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	old, exists := registry.credentials[input.Name]
	if _, configured := registry.base.credentials[input.Name]; configured {
		modelError(w, http.StatusConflict, "Credential is managed by the proxy YAML configuration")
		return
	}
	if update && !exists {
		modelError(w, http.StatusNotFound, "Provider credential not found")
		return
	}
	if !update && exists {
		modelError(w, http.StatusConflict, "Credential name already exists")
		return
	}
	if update {
		values := maps.Clone(old.Values)
		maps.Copy(values, input.Values)
		input.Values = values
		if input.Info == nil {
			input.Info = old.Info
		}
	}
	if err := input.validate(); err != nil {
		modelError(w, http.StatusBadRequest, err.Error())
		return
	}
	next := maps.Clone(registry.credentials)
	if next == nil {
		next = make(map[string]providerCredential)
	}
	next[input.Name] = input
	if registry.applyState(w, registry.models, next) {
		message := "Credential created successfully"
		if update {
			message = "Credential updated successfully"
		}
		modelJSON(w, http.StatusOK, map[string]any{"success": true, "message": message})
	}
}

func (registry *modelRegistry) deleteCredential(w http.ResponseWriter, r *http.Request) {
	if !registry.allowed(w, r) {
		return
	}
	name := r.PathValue("credential_name")
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if _, exists := registry.credentials[name]; !exists {
		modelError(w, http.StatusNotFound, "Provider credential not found")
		return
	}
	next := maps.Clone(registry.credentials)
	delete(next, name)
	if registry.applyState(w, registry.models, next) {
		modelJSON(w, http.StatusOK, map[string]any{"success": true, "message": "Credential deleted successfully"})
	}
}
