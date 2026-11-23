package runtime

import (
	"encoding/json"
	"encoding/xml"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/arn"
)

var awsAccountID = regexp.MustCompile(`^[0-9]{12}$`)

func bedrockDocumentJSON(value any) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func (s *Server) provisionBedrockRAGInfrastructure(w http.ResponseWriter, r *http.Request, candidate Candidate, options map[string]any, prefix, model, index string) (string, string, string, bool) {
	config := candidate.Bedrock
	region := config.Region
	domain, iamRegion := "amazonaws.com", "us-east-1"
	if strings.HasPrefix(region, "cn-") {
		domain, iamRegion = "amazonaws.com.cn", "cn-north-1"
	} else if strings.HasPrefix(region, "us-gov-") {
		iamRegion = "us-gov-west-1"
	}
	stsEndpoint := config.STSBase
	if stsEndpoint == "" {
		stsEndpoint = "https://sts." + region + "." + domain
	}
	identity, ok := s.bedrockAWSQuery(w, r, candidate, stsEndpoint, "sts", "2011-06-15", "GetCallerIdentity", nil)
	if !ok {
		return "", "", "", false
	}
	var caller struct {
		Account string `xml:"GetCallerIdentityResult>Account"`
		ARN     string `xml:"GetCallerIdentityResult>Arn"`
	}
	if xml.Unmarshal(identity, &caller) != nil || !awsAccountID.MatchString(caller.Account) {
		writeError(w, http.StatusBadGateway, "upstream_error", "AWS returned an invalid caller identity")
		return "", "", "", false
	}
	principal, err := arn.Parse(caller.ARN)
	if err != nil || principal.AccountID != caller.Account || (principal.Service != "iam" && principal.Service != "sts") {
		writeError(w, http.StatusBadGateway, "upstream_error", "AWS returned an invalid caller ARN")
		return "", "", "", false
	}
	if role, ok := strings.CutPrefix(principal.Resource, "assumed-role/"); principal.Service == "sts" && ok {
		last := strings.LastIndexByte(role, '/')
		if last <= 0 {
			writeError(w, http.StatusBadGateway, "upstream_error", "AWS returned an invalid assumed role ARN")
			return "", "", "", false
		}
		principal.Resource, principal.Service = "role/"+role[:last], "iam"
	} else if principal.Service != "iam" {
		writeError(w, http.StatusBadGateway, "upstream_error", "The AWS principal cannot manage a Bedrock knowledge base")
		return "", "", "", false
	}
	suffix := newUUID()[:8]
	bucket, _ := options["s3_bucket"].(string)
	if bucket == "" {
		bucket = "litellm-kb-" + suffix
		target := bedrockS3URL(config, &url.URL{Scheme: "s3", Host: bucket, Path: "/"})
		location := config.S3Region
		if location == "" {
			location = region
		}
		var body []byte
		if location != "us-east-1" {
			body, _ = xml.Marshal(struct {
				XMLName   xml.Name `xml:"CreateBucketConfiguration"`
				Namespace string   `xml:"xmlns,attr"`
				Location  string   `xml:"LocationConstraint"`
			}{Namespace: "http://s3.amazonaws.com/doc/2006-03-01/", Location: location})
		}
		if _, ok := s.sendAWSResource(w, r, candidate, http.MethodPut, target, "s3", "application/xml", "", body); !ok {
			return "", "", "", false
		}
	}
	bucketARN := "arn:" + principal.Partition + ":s3:::" + bucket
	embeddingARN := "arn:" + principal.Partition + ":bedrock:" + region + "::foundation-model/" + model
	collectionName := "litellm-kb-" + suffix
	collectionARN, collectionURL, ok := s.createBedrockSearchCollection(w, r, candidate, options, domain, collectionName, principal.String())
	if !ok {
		return "", "", "", false
	}
	iamEndpoint := config.IAMBase
	if iamEndpoint == "" {
		iamEndpoint = "https://iam." + domain
	}
	iamConfig := *config
	iamConfig.Region = iamRegion
	iamCandidate := candidate
	iamCandidate.Bedrock = &iamConfig
	roleName := "litellm-bedrock-kb-" + suffix
	trust := map[string]any{"Version": "2012-10-17", "Statement": []map[string]any{{
		"Effect": "Allow", "Principal": map[string]string{"Service": "bedrock." + domain}, "Action": "sts:AssumeRole",
		"Condition": map[string]any{"StringEquals": map[string]string{"aws:SourceAccount": caller.Account},
			"ArnLike": map[string]string{"aws:SourceArn": "arn:" + principal.Partition + ":bedrock:" + region + ":" + caller.Account + ":knowledge-base/*"}},
	}}}
	raw, ok := s.bedrockAWSQuery(w, r, iamCandidate, iamEndpoint, "iam", "2010-05-08", "CreateRole",
		url.Values{"RoleName": {roleName}, "AssumeRolePolicyDocument": {bedrockDocumentJSON(trust)}})
	if !ok {
		return "", "", "", false
	}
	var role struct {
		ARN string `xml:"CreateRoleResult>Role>Arn"`
	}
	if xml.Unmarshal(raw, &role) != nil || role.ARN == "" {
		writeError(w, http.StatusBadGateway, "upstream_error", "IAM returned no knowledge base role ARN")
		return "", "", "", false
	}
	permissions := map[string]any{"Version": "2012-10-17", "Statement": []map[string]any{
		{"Effect": "Allow", "Action": "bedrock:InvokeModel", "Resource": embeddingARN},
		{"Effect": "Allow", "Action": "aoss:APIAccessAll", "Resource": collectionARN},
		{"Effect": "Allow", "Action": "s3:ListBucket", "Resource": bucketARN},
		{"Effect": "Allow", "Action": "s3:GetObject", "Resource": bucketARN + "/" + strings.TrimRight(prefix, "/") + "/*"},
	}}
	if _, ok := s.bedrockAWSQuery(w, r, iamCandidate, iamEndpoint, "iam", "2010-05-08", "PutRolePolicy",
		url.Values{"RoleName": {roleName}, "PolicyName": {roleName + "-policy"}, "PolicyDocument": {bedrockDocumentJSON(permissions)}}); !ok {
		return "", "", "", false
	}
	access := []map[string]any{{
		"Rules": []map[string]any{
			{"ResourceType": "index", "Resource": []string{"index/" + collectionName + "/" + index}, "Permission": []string{"aoss:CreateIndex", "aoss:UpdateIndex", "aoss:DescribeIndex", "aoss:ReadDocument", "aoss:WriteDocument"}},
			{"ResourceType": "collection", "Resource": []string{"collection/" + collectionName}, "Permission": []string{"aoss:DescribeCollectionItems"}},
		},
		"Principal": []string{principal.String(), role.ARN},
	}}
	if _, ok := s.bedrockOpenSearchAPI(w, r, candidate, domain, "CreateAccessPolicy", map[string]any{
		"name": collectionName + "-access", "type": "data", "clientToken": newUUID(), "policy": bedrockDocumentJSON(access),
	}); !ok {
		return "", "", "", false
	}
	if !s.createBedrockSearchIndex(w, r, candidate, collectionURL, index, options) {
		return "", "", "", false
	}
	return role.ARN, collectionARN, bucket, true
}

func (s *Server) bedrockAWSQuery(w http.ResponseWriter, r *http.Request, candidate Candidate, endpoint, service, version, action string, fields url.Values) ([]byte, bool) {
	if fields == nil {
		fields = make(url.Values)
	}
	fields.Set("Action", action)
	fields.Set("Version", version)
	return s.sendAWSResource(w, r, candidate, http.MethodPost, strings.TrimRight(endpoint, "/")+"/", service, "application/x-www-form-urlencoded", "", []byte(fields.Encode()))
}

func (s *Server) bedrockOpenSearchAPI(w http.ResponseWriter, r *http.Request, candidate Candidate, domain, action string, fields map[string]any) ([]byte, bool) {
	base := candidate.Bedrock.OpenSearchBase
	if base == "" {
		base = "https://aoss." + candidate.Bedrock.Region + "." + domain
	}
	return s.sendAWSResource(w, r, candidate, http.MethodPost, strings.TrimRight(base, "/")+"/", "aoss", "application/x-amz-json-1.0",
		"OpenSearchServerless."+action, []byte(bedrockDocumentJSON(fields)))
}

func (s *Server) createBedrockSearchCollection(w http.ResponseWriter, r *http.Request, candidate Candidate, options map[string]any, domain, name, principal string) (string, string, bool) {
	rule := map[string]any{"ResourceType": "collection", "Resource": []string{"collection/" + name}}
	encryption := map[string]any{"Rules": []map[string]any{rule}, "AWSOwnedKey": true}
	network := map[string]any{"Rules": []map[string]any{rule}, "AllowFromPublic": true}
	if endpoints, _ := options["opensearch_vpc_endpoints"].([]any); len(endpoints) > 0 {
		network["AllowFromPublic"], network["SourceVPCEs"], network["SourceServices"] = false, endpoints, []string{"bedrock.amazonaws.com"}
	}
	for _, policy := range []struct {
		suffix, kind string
		document     any
	}{
		{"-enc", "encryption", encryption}, {"-net", "network", []map[string]any{network}},
	} {
		if _, ok := s.bedrockOpenSearchAPI(w, r, candidate, domain, "CreateSecurityPolicy", map[string]any{
			"name": name + policy.suffix, "type": policy.kind, "policy": bedrockDocumentJSON(policy.document), "clientToken": newUUID(),
		}); !ok {
			return "", "", false
		}
	}
	raw, ok := s.bedrockOpenSearchAPI(w, r, candidate, domain, "CreateCollection", map[string]any{
		"name": name, "type": "VECTORSEARCH", "clientToken": newUUID(),
	})
	if !ok {
		return "", "", false
	}
	var created struct {
		Collection struct {
			ID string `json:"id"`
		} `json:"createCollectionDetail"`
	}
	if json.Unmarshal(raw, &created) != nil || !validResourceSegment(created.Collection.ID) {
		writeError(w, http.StatusBadGateway, "upstream_error", "OpenSearch returned no collection ID")
		return "", "", false
	}
	for attempt := 0; attempt < 60; attempt++ {
		if attempt > 0 && !waitBedrockProvision(r, 5*time.Second) {
			return "", "", false
		}
		raw, ok := s.bedrockOpenSearchAPI(w, r, candidate, domain, "BatchGetCollection", map[string]any{"ids": []string{created.Collection.ID}})
		if !ok {
			return "", "", false
		}
		var result struct {
			Collections []struct {
				ARN      string `json:"arn"`
				Endpoint string `json:"collectionEndpoint"`
				Status   string `json:"status"`
			} `json:"collectionDetails"`
		}
		if json.Unmarshal(raw, &result) != nil || len(result.Collections) != 1 || result.Collections[0].Status == "FAILED" {
			break
		}
		collection := result.Collections[0]
		if collection.Status == "ACTIVE" {
			endpoint, err := url.Parse(collection.Endpoint)
			host := created.Collection.ID + "." + candidate.Bedrock.Region + ".aoss." + domain
			override, _ := url.Parse(candidate.Bedrock.OpenSearchBase)
			if err == nil && endpoint.User == nil && endpoint.RawQuery == "" && endpoint.Fragment == "" &&
				((endpoint.Scheme == "https" && strings.EqualFold(endpoint.Host, host)) ||
					(override != nil && override.Host != "" && endpoint.Scheme == override.Scheme && strings.EqualFold(endpoint.Host, override.Host))) {
				collARN, arnErr := arn.Parse(collection.ARN)
				owner, _ := arn.Parse(principal)
				if arnErr == nil && collARN.Service == "aoss" && collARN.AccountID == owner.AccountID &&
					collARN.Partition == owner.Partition && collARN.Region == candidate.Bedrock.Region && collARN.Resource == "collection/"+created.Collection.ID {
					return collection.ARN, collection.Endpoint, true
				}
			}
			break
		}
	}
	writeError(w, http.StatusBadGateway, "upstream_error", "OpenSearch collection "+name+" did not return a valid active endpoint")
	return "", "", false
}

func (s *Server) createBedrockSearchIndex(w http.ResponseWriter, r *http.Request, candidate Candidate, endpoint, index string, options map[string]any) bool {
	dimension, _ := ragChunkOption(options, "dimension", 1024, 4096)
	body := map[string]any{
		"settings": map[string]any{"index": map[string]any{"knn": true, "knn.algo_param.ef_search": 512}},
		"mappings": map[string]any{"properties": map[string]any{
			"bedrock-knowledge-base-default-vector": map[string]any{"type": "knn_vector", "dimension": dimension,
				"method": map[string]string{"engine": "faiss", "name": "hnsw", "space_type": "l2"}},
			"AMAZON_BEDROCK_METADATA":   map[string]any{"type": "text", "index": false},
			"AMAZON_BEDROCK_TEXT_CHUNK": map[string]string{"type": "text"},
		}},
	}
	address := strings.TrimRight(endpoint, "/") + "/" + url.PathEscape(index)
	for attempt := 0; attempt < 8; attempt++ {
		stage := &ragResult{header: make(http.Header), limit: s.config.MaxBodyBytes}
		if _, ok := s.sendAWSJSONResource(stage, r, candidate, http.MethodPut, address, "aoss", body); ok {
			return true
		}
		if stage.status != http.StatusForbidden || attempt == 7 {
			if stage.status != 0 {
				writeRawResponse(w, stage.status, stage.header, stage.body.Bytes())
			}
			return false
		}
		if !waitBedrockProvision(r, 20*time.Second) {
			return false
		}
	}
	return false
}

func waitBedrockProvision(r *http.Request, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-r.Context().Done():
		return false
	}
}
