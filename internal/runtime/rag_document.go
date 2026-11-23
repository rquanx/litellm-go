package runtime

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"unicode/utf8"
)

const maxRAGChunks = 500

func (s *Server) ragDocumentText(w http.ResponseWriter, r *http.Request, scope []string, input ragIngestInput) (string, bool) {
	options, _ := input.Options["ocr"].(map[string]any)
	if len(options) == 0 {
		if utf8.Valid(input.file) {
			return string(input.file), true
		}
		writeError(w, http.StatusBadRequest, "invalid_request", "Upload UTF-8 text or configure OCR for this document")
		return "", false
	}
	model, _ := options["model"].(string)
	if model == "" {
		model = "mistral/mistral-ocr-latest"
	}
	kind := "document_url"
	if strings.Contains(input.media, "image") {
		kind = "image_url"
	}
	document := map[string]string{"type": kind, kind: "data:" + input.media + ";base64," + base64.StdEncoding.EncodeToString(input.file)}
	body, _ := json.Marshal(map[string]any{"model": model, "document": document})
	stage, ok := s.ragStage(w, func(output http.ResponseWriter) {
		s.infer(output, ragSubrequest(r, http.MethodPost, "/v1/ocr", body, false), "ocr", model, scope)
	})
	if !ok {
		return "", false
	}
	var result struct {
		Pages []struct {
			Markdown string `json:"markdown"`
		} `json:"pages"`
	}
	if json.Unmarshal(stage.body.Bytes(), &result) != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "The OCR deployment returned an invalid result")
		return "", false
	}
	pages := make([]string, 0, len(result.Pages))
	for _, page := range result.Pages {
		pages = append(pages, page.Markdown)
	}
	return strings.Join(pages, "\n\n"), true
}

func splitRAGDocument(text string, size, overlap int, separators []string) []string {
	if len(separators) == 0 {
		separators = []string{"\n\n", "\n", " ", ""}
	}
	var chunks []string
	appendChunk := func(value string) {
		if text := strings.TrimSpace(value); text != "" {
			chunks = append(chunks, text)
		}
	}
	var split func(string, []string, int)
	split = func(text string, separators []string, depth int) {
		if depth > 100 {
			for runes := []rune(text); len(runes) > 0 && len(chunks) <= maxRAGChunks; {
				end := min(len(runes), size)
				chunks = append(chunks, string(runes[:end]))
				runes = runes[end:]
			}
			return
		}
		separator := separators[len(separators)-1]
		var finer []string
		for index, value := range separators {
			if value == "" || strings.Contains(text, value) {
				separator = value
				if separator != "" {
					finer = separators[index+1:]
				}
				break
			}
		}
		var current []string
		length := 0
		separatorLength := utf8.RuneCountInString(separator)
		for part := range strings.SplitSeq(text, separator) {
			if len(chunks) > maxRAGChunks {
				return
			}
			partLength := utf8.RuneCountInString(part)
			if partLength >= size {
				if len(current) > 0 {
					appendChunk(strings.Join(current, separator))
					current, length = current[:0], 0
				}
				if len(finer) > 0 {
					split(part, finer, depth+1)
				} else {
					runes := []rune(part)
					for start := 0; start < len(runes) && len(chunks) <= maxRAGChunks; start += size - overlap {
						end := min(start+size, len(runes))
						appendChunk(string(runes[start:end]))
						if end == len(runes) {
							break
						}
					}
				}
				continue
			}
			joinLength := 0
			if len(current) > 0 {
				joinLength = separatorLength
			}
			if length+partLength+joinLength > size && len(current) > 0 {
				appendChunk(strings.Join(current, separator))
				for length > overlap && len(current) > 1 {
					length -= utf8.RuneCountInString(current[0]) + separatorLength
					current = current[1:]
				}
			}
			current = append(current, part)
			length += partLength + joinLength
		}
		if len(current) > 0 && len(chunks) <= maxRAGChunks {
			appendChunk(strings.Join(current, separator))
		}
	}
	split(text, separators, 0)
	return chunks
}
