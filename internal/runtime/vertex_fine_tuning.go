package runtime

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

const vertexTuningFamily = "vertex_fine_tuning"

type vertexTuningJob struct {
	Name    string     `json:"name"`
	State   string     `json:"state"`
	Created *time.Time `json:"createTime"`
	Model   string     `json:"baseModel"`
	Display string     `json:"tunedModelDisplayName"`
	Spec    struct {
		Training string         `json:"trainingDatasetUri"`
		Hyper    map[string]any `json:"hyperParameters"`
	} `json:"supervisedTuningSpec"`
}

func vertexTuningPath(candidate Candidate) string {
	return "/v1/projects/" + url.PathEscape(candidate.Vertex.Project) + "/locations/" + url.PathEscape(candidate.Vertex.Location) + "/tuningJobs"
}

func normalizeVertexTuning(job vertexTuningJob, candidate Candidate) map[string]any {
	_, id, found := strings.Cut(job.Name, "/tuningJobs/")
	if !found || !validResourceSegment(id) {
		return nil
	}
	hyper := map[string]any{"n_epochs": 0}
	for key, value := range job.Spec.Hyper {
		switch key {
		case "epochCount", "epoch_count":
			hyper["n_epochs"] = value
		case "learningRateMultiplier":
			hyper["learning_rate_multiplier"] = value
		case "adapterSize":
			hyper["adapter_size"] = value
		default:
			hyper[key] = value
		}
	}
	var created int64
	if job.Created != nil {
		created = job.Created.Unix()
	}
	status := map[string]string{"JOB_STATE_RUNNING": "running", "JOB_STATE_SUCCEEDED": "succeeded",
		"JOB_STATE_FAILED": "failed", "JOB_STATE_CANCELLED": "cancelled"}[job.State]
	if status == "" {
		status = "queued"
	}
	return map[string]any{
		"id": encodeResourceID(vertexTuningFamily, candidate.DeploymentID, id), "object": "fine_tuning.job",
		"created_at": created, "fine_tuned_model": job.Display, "finished_at": nil, "hyperparameters": hyper,
		"model": job.Model, "organization_id": "", "result_files": []string{}, "seed": 0, "status": status,
		"trained_tokens": nil, "training_file": job.Spec.Training, "validation_file": nil, "estimated_finish": nil, "integrations": []string{},
	}
}

func (s *Server) createVertexFineTuning(w http.ResponseWriter, r *http.Request, alias string, scope []string, fields map[string]any) {
	candidate, ok := s.selectGoogleResourceCandidate(alias, "", "vertex_ai", scope, false)
	if !ok || candidate.Vertex == nil {
		writeError(w, http.StatusBadRequest, "model_required", "Select a configured Vertex fine-tuning deployment")
		return
	}
	for _, name := range []string{"training_file", "validation_file"} {
		id, _ := fields[name].(string)
		if id == "" && name == "validation_file" {
			continue
		}
		uri, err := url.Parse(id)
		if err != nil || uri.Scheme != "gs" || uri.Hostname() == "" || uri.User != nil || uri.RawQuery != "" ||
			uri.Fragment != "" || uri.Path == "" || strings.HasSuffix(uri.Path, "/") {
			writeError(w, http.StatusBadRequest, "invalid_request", "Vertex fine-tuning requires GCS dataset URIs")
			return
		}
		if _, _, managed := vertexStorageObject(candidate, id); managed && !s.authorized(r) {
			owners, err := s.config.ContainerOwners.Read(r.Context(), candidate.DeploymentID, []string{id})
			if err != nil {
				writeError(w, http.StatusServiceUnavailable, "resource_error", "Could not check Vertex dataset ownership")
				return
			}
			if !slices.Contains(s.containerOwnerScopes(r, false), owners[id]) {
				writeError(w, http.StatusNotFound, "resource_not_found", "Unknown Vertex dataset")
				return
			}
		}
	}
	spec := map[string]any{"training_dataset_uri": fields["training_file"]}
	if validation, _ := fields["validation_file"].(string); validation != "" {
		spec["validation_dataset"] = validation
	}
	options, _ := fields["hyperparameters"].(map[string]any)
	hyper := make(map[string]any)
	for from, to := range map[string]string{"n_epochs": "epoch_count", "learning_rate_multiplier": "learning_rate_multiplier", "adapter_size": "adapter_size"} {
		if value := options[from]; value != nil && value != "auto" {
			hyper[to] = value
		}
	}
	if len(hyper) != 0 {
		spec["hyperParameters"] = hyper
	}
	model := strings.TrimPrefix(candidate.ModelID, "models/")
	payload := map[string]any{"baseModel": model, "supervisedTuningSpec": spec}
	if suffix, _ := fields["suffix"].(string); suffix != "" {
		payload["tunedModelDisplayName"] = suffix
	}
	raw, ok := s.sendVertexBatch(w, r, candidate, http.MethodPost, vertexTuningPath(candidate), payload)
	if !ok {
		return
	}
	var job vertexTuningJob
	if json.Unmarshal(raw, &job) != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Invalid Vertex tuning response")
		return
	}
	result := normalizeVertexTuning(job, candidate)
	if result == nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Invalid Vertex tuning identifier")
		return
	}
	if !s.authorized(r) {
		ref, _ := decodeResourceID(result["id"].(string), vertexTuningFamily)
		owner := s.containerOwnerScopes(r, false)[0]
		if err := s.config.ContainerOwners.Record(r.Context(), candidate.DeploymentID, "vertex_tuning:"+ref.Original, owner); err != nil {
			writeError(w, http.StatusServiceUnavailable, "resource_error", "Could not record Vertex tuning ownership")
			return
		}
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) vertexFineTuningResource(w http.ResponseWriter, r *http.Request, provider, alias string, scope []string) bool {
	remainder, ok := strings.CutPrefix(r.URL.EscapedPath(), fineTuningPath+"/")
	if !ok || (provider != "" && provider != "vertex_ai") {
		return false
	}
	rawID, action, _ := strings.Cut(remainder, "/")
	id, err := url.PathUnescape(rawID)
	if err != nil {
		return false
	}
	bound, managed := decodeResourceID(id, vertexTuningFamily)
	if managed {
		id = bound.Original
	} else if provider == "vertex_ai" {
		if _, original, ok := strings.Cut(id, "/tuningJobs/"); ok {
			id = original
		}
	} else {
		return false
	}
	candidate, ok := s.selectGoogleResourceCandidate(alias, bound.Deployment, "vertex_ai", scope, false)
	if !ok || candidate.Vertex == nil || !validResourceSegment(id) {
		writeError(w, http.StatusNotFound, "resource_not_found", "Unknown Vertex tuning job")
		return true
	}
	if !s.authorized(r) {
		key := "vertex_tuning:" + id
		owners, err := s.config.ContainerOwners.Read(r.Context(), candidate.DeploymentID, []string{key})
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "resource_error", "Could not check Vertex tuning job ownership")
			return true
		}
		if !slices.Contains(s.containerOwnerScopes(r, false), owners[key]) {
			writeError(w, http.StatusNotFound, "resource_not_found", "Unknown Vertex tuning job")
			return true
		}
	}
	path := vertexTuningPath(candidate) + "/" + url.PathEscape(id)
	if r.Method == http.MethodPost && action == "cancel" {
		if _, ok := s.sendVertexBatch(w, r, candidate, http.MethodPost, path+":cancel", map[string]any{}); !ok {
			return true
		}
	} else if r.Method != http.MethodGet || action != "" {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Unsupported Vertex tuning operation")
		return true
	}
	raw, ok := s.sendVertexBatch(w, r, candidate, http.MethodGet, path, nil)
	if ok {
		var job vertexTuningJob
		if json.Unmarshal(raw, &job) == nil {
			if result := normalizeVertexTuning(job, candidate); result != nil && result["id"] == encodeResourceID(vertexTuningFamily, candidate.DeploymentID, id) {
				writeJSON(w, http.StatusOK, result)
				return true
			}
		}
		writeError(w, http.StatusBadGateway, "upstream_error", "Invalid Vertex tuning response")
	}
	return true
}
