package proxyconfig

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

func (entry deployment) azureIdentity(officialHost bool, oidc string) (func(context.Context) (string, error), string, error) {
	options := entry.Params
	var tenant, client, secret, scope, kind, authority, username, password string
	for _, setting := range []struct {
		raw, env string
		dest     *string
	}{
		{options.AzureTenant, "AZURE_TENANT_ID", &tenant}, {options.AzureClient, "AZURE_CLIENT_ID", &client},
		{options.AzureClientSecret, "AZURE_CLIENT_SECRET", &secret}, {options.AzureScope, "AZURE_SCOPE", &scope},
		{options.AzureUsername, "AZURE_USERNAME", &username}, {options.AzurePassword, "AZURE_PASSWORD", &password},
		{options.AzureCredential, "AZURE_CREDENTIAL", &kind}, {options.AzureAuthority, "AZURE_AUTHORITY_HOST", &authority},
	} {
		value, err := resolveEnv(setting.raw)
		if err != nil {
			return nil, "", err
		}
		if value == "" && (officialHost || oidc != "" || options.AzureTokenRefresh || options.AzureCredential != "" || options.AzureTenant != "") {
			value = os.Getenv(setting.env)
		}
		*setting.dest = value
	}
	if oidc != "" {
		kind = "ClientAssertionCredential"
	}
	if kind == "" {
		switch {
		case client != "" && tenant != "" && secret != "":
			kind = "ClientSecretCredential"
		case client != "" && username != "" && password != "":
			kind = "UsernamePasswordCredential"
		case options.AzureTokenRefresh && client != "" && tenant != "" && os.Getenv("AZURE_CERTIFICATE_PATH") != "":
			kind = "CertificateCredential"
		case options.AzureTokenRefresh && client != "" && os.Getenv("AZURE_FEDERATED_TOKEN_FILE") == "":
			kind = "ManagedIdentityCredential"
		case options.AzureTokenRefresh:
			kind = "DefaultAzureCredential"
		default:
			return nil, "", nil
		}
	}
	if scope == "" {
		scope = "https://cognitiveservices.azure.com/.default"
	}
	if authority != "" {
		parsed, err := url.Parse(authority)
		if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil ||
			(parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
			return nil, "", errors.New("azure_authority_host must be an HTTPS origin")
		}
	}
	var credential azcore.TokenCredential
	var err error
	switch kind {
	case "ClientSecretCredential":
		credential, err = azidentity.NewClientSecretCredential(tenant, client, secret, &azidentity.ClientSecretCredentialOptions{
			ClientOptions: azureIdentityOptions(authority),
		})
	case "ClientAssertionCredential":
		source, sourceErr := azureOIDCSource(oidc)
		if sourceErr != nil {
			return nil, "", sourceErr
		}
		credential, err = azidentity.NewClientAssertionCredential(tenant, client, source, &azidentity.ClientAssertionCredentialOptions{
			ClientOptions: azureIdentityOptions(authority),
		})
	case "UsernamePasswordCredential":
		if tenant == "" {
			tenant = "organizations"
		}
		if client == "" || username == "" || password == "" {
			return nil, "", errors.New("Azure username/password authentication requires client_id, azure_username and azure_password")
		}
		credential, err = azidentity.NewUsernamePasswordCredential(tenant, client, username, password, &azidentity.UsernamePasswordCredentialOptions{
			ClientOptions: azureIdentityOptions(authority),
		})
	case "ManagedIdentityCredential":
		settings := &azidentity.ManagedIdentityCredentialOptions{}
		if client != "" {
			settings.ID = azidentity.ClientID(client)
		}
		credential, err = azidentity.NewManagedIdentityCredential(settings)
	case "WorkloadIdentityCredential":
		credential, err = azidentity.NewWorkloadIdentityCredential(&azidentity.WorkloadIdentityCredentialOptions{
			ClientID: client, TenantID: tenant, ClientOptions: azureIdentityOptions(authority),
		})
	case "CertificateCredential":
		path := os.Getenv("AZURE_CERTIFICATE_PATH")
		if tenant == "" || client == "" || path == "" {
			return nil, "", errors.New("Azure certificate authentication requires tenant_id, client_id and AZURE_CERTIFICATE_PATH")
		}
		certificate, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil, "", errors.New("could not read Azure identity certificate")
		}
		certs, privateKey, parseErr := azidentity.ParseCertificates(certificate, []byte(os.Getenv("AZURE_CERTIFICATE_PASSWORD")))
		if parseErr != nil {
			return nil, "", errors.New("could not parse Azure identity certificate")
		}
		credential, err = azidentity.NewClientCertificateCredential(tenant, client, certs, privateKey, &azidentity.ClientCertificateCredentialOptions{
			ClientOptions: azureIdentityOptions(authority),
		})
	case "DefaultAzureCredential":
		credential, err = azidentity.NewDefaultAzureCredential(&azidentity.DefaultAzureCredentialOptions{
			ClientOptions: azureIdentityOptions(authority), TenantID: tenant,
		})
	default:
		return nil, "", errors.New("unsupported Azure credential type")
	}
	if err != nil {
		return nil, "", errors.New("could not initialize Azure identity")
	}
	get := func(ctx context.Context) (string, error) {
		token, err := credential.GetToken(ctx, policy.TokenRequestOptions{Scopes: strings.Fields(scope)})
		return token.Token, err
	}
	identity := sha256.Sum256([]byte(strings.Join([]string{kind, tenant, client, scope, authority, secret, username, password, oidc}, "\x00")))
	return get, fmt.Sprintf("%x", identity), nil
}

func azureIdentityOptions(authority string) azcore.ClientOptions {
	options := azcore.ClientOptions{}
	if authority != "" {
		options.Cloud.ActiveDirectoryAuthorityHost = authority
	}
	return options
}
