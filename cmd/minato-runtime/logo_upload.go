package main

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"sync"
)

type logoUploadStore struct { mu sync.RWMutex; data []byte; contentType string }

func withLogoUpload(next http.Handler, token string, store *logoUploadStore) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/upload/logo":
			if token == "" || r.Header.Get("Authorization") != "Bearer "+token { http.Error(w, "Unauthorized", http.StatusUnauthorized); return }
			if err := r.ParseMultipartForm(8 << 20); err != nil { http.Error(w, "Invalid image upload", http.StatusBadRequest); return }
			var file multipart.File; var err error
			for _, name := range []string{"file", "logo"} { file, _, err = r.FormFile(name); if err == nil { break } }
			if err != nil { http.Error(w, "Image file is required", http.StatusBadRequest); return }
			defer file.Close(); data, err := io.ReadAll(io.LimitReader(file, 8<<20)); if err != nil || len(data) == 0 { http.Error(w, "Invalid image upload", http.StatusBadRequest); return }
			contentType := http.DetectContentType(data); if !bytes.HasPrefix([]byte(contentType), []byte("image/")) { http.Error(w, "Only image uploads are supported", http.StatusBadRequest); return }
			store.mu.Lock(); store.data, store.contentType = data, contentType; store.mu.Unlock()
			modelJSONCompat(w, http.StatusOK, map[string]any{"message": "Logo uploaded successfully", "status": "success"}); return
		case r.Method == http.MethodGet && r.URL.Path == "/get_image":
			store.mu.RLock(); data, contentType := append([]byte(nil), store.data...), store.contentType; store.mu.RUnlock(); if len(data) > 0 { w.Header().Set("Content-Type", contentType); w.WriteHeader(http.StatusOK); _, _ = w.Write(data); return }
		}
		next.ServeHTTP(w, r)
	})
}

func modelJSONCompat(w http.ResponseWriter, status int, value any) { w.Header().Set("Content-Type", "application/json"); w.WriteHeader(status); _ = json.NewEncoder(w).Encode(value) }
