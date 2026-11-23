package runtime

import (
	"net/http"
	"strconv"
	"strings"
)

func (s *Server) ingestS3Vectors(w http.ResponseWriter, r *http.Request, scope []string, alias, id, name string, options map[string]any, input ragIngestInput) {
	bucket, _ := options["vector_bucket_name"].(string)
	index, _ := options["index_name"].(string)
	if id != "" {
		if namedBucket, namedIndex, combined := strings.Cut(id, ":"); combined {
			if (bucket != "" && bucket != namedBucket) || (index != "" && index != namedIndex) {
				writeError(w, http.StatusBadRequest, "invalid_request", "Conflicting S3 vector bucket or index names")
				return
			}
			bucket, index = namedBucket, namedIndex
		} else {
			index = id
		}
	}
	if index == "" {
		index = strings.ToLower(name)
	}
	if index == "" {
		index = "litellm-" + newUUID()
	}
	if !bedrockRAGBucket.MatchString(bucket) || !bedrockRAGBucket.MatchString(index) || !validResourceSegment(input.filename) ||
		len(input.file) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "An S3 vector bucket, valid index and nonempty document are required")
		return
	}
	strategy, _ := input.Options["chunking_strategy"].(map[string]any)
	size, sizeOK := ragChunkOption(strategy, "chunk_size", 1000, 4000)
	overlap, overlapOK := ragChunkOption(strategy, "chunk_overlap", 200, size-1)
	if !sizeOK || !overlapOK || size == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid S3 Vectors chunk size or overlap")
		return
	}
	var separators []string
	if values, _ := strategy["separators"].([]any); len(values) > 0 {
		separators = make([]string, 0, len(values))
		for _, value := range values {
			separator, ok := value.(string)
			if !ok {
				writeError(w, http.StatusBadRequest, "invalid_request", "Chunk separators must be strings")
				return
			}
			separators = append(separators, separator)
		}
	}
	text, ok := s.ragDocumentText(w, r, scope, input)
	if !ok {
		return
	}
	chunks := splitRAGDocument(text, size, overlap, separators)
	if len(chunks) == 0 || len(chunks) > maxRAGChunks {
		writeError(w, http.StatusBadRequest, "invalid_request", "The document must contain between 1 and 500 vector chunks")
		return
	}
	region, _ := options["aws_region_name"].(string)
	candidate, status := s.bedrockControlDeployment(r, alias, region, "", scope)
	if status != 0 {
		writeError(w, status, "model_required", "Select a configured AWS deployment for S3 Vectors")
		return
	}
	embedding, _ := input.Options["embedding"].(map[string]any)
	model, _ := embedding["model"].(string)
	vectors, ok := s.ragEmbeddings(w, r, scope, model, chunks, embedding)
	if !ok {
		return
	}
	dimension := len(vectors[0])
	expected, valid := ragChunkOption(options, "dimension", dimension, 4096)
	if !valid || dimension == 0 || dimension > 4096 || expected != dimension {
		writeError(w, http.StatusBadRequest, "invalid_request", "The configured dimension must match the embedding output")
		return
	}
	metric, _ := options["distance_metric"].(string)
	if metric == "" {
		metric = "cosine"
	}
	if metric != "cosine" && metric != "euclidean" {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid S3 Vectors distance metric")
		return
	}
	nonFilterable := options["non_filterable_metadata_keys"]
	if nonFilterable == nil {
		nonFilterable = []string{"source_text"}
	}
	entries := make([]map[string]any, 0, len(chunks))
	for index, chunk := range chunks {
		if len(vectors[index]) != dimension {
			writeError(w, http.StatusBadGateway, "upstream_error", "The embedding deployment returned vectors with inconsistent dimensions")
			return
		}
		chunkID := strconv.Itoa(index)
		entries = append(entries, map[string]any{"key": input.filename + "_" + chunkID, "data": map[string]any{"float32": vectors[index]},
			"metadata": map[string]string{"source_text": chunk, "chunk_index": chunkID, "filename": input.filename}})
	}
	bucketFields := map[string]any{"vectorBucketName": bucket}
	indexFields := map[string]any{"vectorBucketName": bucket, "indexName": index}
	createIndex := clone(indexFields)
	createIndex["dimension"], createIndex["dataType"], createIndex["distanceMetric"] = dimension, "float32", metric
	createIndex["metadataConfiguration"] = map[string]any{"nonFilterableMetadataKeys": nonFilterable}
	if !s.ensureS3VectorResource(w, r, candidate, "GetVectorBucket", "CreateVectorBucket", bucketFields, bucketFields) ||
		!s.ensureS3VectorResource(w, r, candidate, "GetIndex", "CreateIndex", indexFields, createIndex) {
		return
	}
	indexFields["vectors"] = entries
	if _, ok := s.sendS3VectorRequest(w, r, candidate, "PutVectors", indexFields); ok {
		writeRAGIngest(w, bucket+":"+index, input.filename)
	}
}

func ragChunkOption(options map[string]any, key string, fallback, maximum int) (int, bool) {
	if options[key] == nil {
		return fallback, fallback <= maximum
	}
	value, ok := options[key].(float64)
	if !ok || value < 0 || value > float64(maximum) || value != float64(int(value)) {
		return 0, false
	}
	return int(value), true
}

func (s *Server) ensureS3VectorResource(w http.ResponseWriter, r *http.Request, candidate Candidate, get, create string, lookup, config map[string]any) bool {
	stage := &ragResult{header: make(http.Header), limit: s.config.MaxBodyBytes}
	if _, ok := s.sendS3VectorRequest(stage, r, candidate, get, lookup); ok {
		return true
	}
	if stage.status != http.StatusNotFound {
		if stage.status != 0 {
			writeRawResponse(w, stage.status, stage.header, stage.body.Bytes())
		}
		return false
	}
	stage = &ragResult{header: make(http.Header), limit: s.config.MaxBodyBytes}
	if _, ok := s.sendS3VectorRequest(stage, r, candidate, create, config); ok || stage.status == http.StatusConflict {
		return true
	}
	if stage.status != 0 {
		writeRawResponse(w, stage.status, stage.header, stage.body.Bytes())
	}
	return false
}
