package proxyconfig

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/arn"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/rquanx/minato-runtime/internal/runtime"
)

type staticAWSIdentityToken string

func (token staticAWSIdentityToken) GetIdentityToken() ([]byte, error) {
	return []byte(token), nil
}

func (entry deployment) assumeAWSRole(config *runtime.BedrockConfig, profile string) (*runtime.BedrockConfig, error) {
	var role, session, identity, endpoint, external string
	for _, field := range []struct {
		setting string
		target *string
	}{
		{entry.Params.AWSRole, &role}, {entry.Params.AWSRoleSession, &session}, {entry.Params.AWSWebIdentity, &identity},
		{entry.Params.AWSSTSEndpoint, &endpoint}, {entry.Params.AWSExternalID, &external},
	} {
		value, err := resolveEnv(field.setting)
		if err != nil {
			return nil, err
		}
		*field.target = value
	}
	targetRole, err := arn.Parse(role)
	if err != nil || targetRole.Service != "iam" || targetRole.AccountID == "" || !strings.HasPrefix(targetRole.Resource, "role/") {
		return nil, errors.New("aws_role_name must be an IAM role ARN")
	}
	if endpoint != "" {
		target, err := url.Parse(endpoint)
		if err != nil || (target.Scheme != "https" && target.Scheme != "http") || target.Hostname() == "" ||
			target.User != nil || target.RawQuery != "" || target.Fragment != "" {
			return nil, errors.New("aws_sts_endpoint must be an HTTP or HTTPS URL without credentials, query or fragment")
		}
	}
	options := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(config.Region)}
	if profile != "" {
		options = append(options, awsconfig.WithSharedConfigProfile(profile))
	}
	if identity != "" {
		options = append(options, awsconfig.WithCredentialsProvider(aws.AnonymousCredentials{}))
	} else if config.AccessKey != "" {
		options = append(options, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(config.AccessKey, config.SecretKey, config.SessionToken)))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	loaded, err := awsconfig.LoadDefaultConfig(ctx, options...)
	if err != nil || loaded.Region == "" {
		return nil, errors.New("could not load AWS role credentials or region")
	}
	client := sts.NewFromConfig(loaded, func(o *sts.Options) {
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
		}
	})
	if identity != "" {
		config.Credentials = aws.NewCredentialsCache(stscreds.NewWebIdentityRoleProvider(client, role, staticAWSIdentityToken(identity),
			func(o *stscreds.WebIdentityRoleOptions) { o.RoleSessionName = session }))
	} else {
		config.Credentials = aws.NewCredentialsCache(stscreds.NewAssumeRoleProvider(client, role, func(o *stscreds.AssumeRoleOptions) {
			o.RoleSessionName = session
			if external != "" {
				o.ExternalID = aws.String(external)
			}
		}))
	}
	config.Region = loaded.Region
	config.AccessKey, config.SecretKey, config.SessionToken = "", "", ""
	return config, nil
}
