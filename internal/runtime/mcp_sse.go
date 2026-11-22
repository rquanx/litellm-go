package runtime

import (
	"bufio"
	"bytes"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

func (session *mcpSession) openLegacySSE() error {
	response, err := session.send(session.ctx, http.MethodGet, nil)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		response.Body.Close()
		return errors.New("MCP SSE endpoint rejected the stream")
	}
	events := bufio.NewScanner(response.Body)
	events.Buffer(make([]byte, 4096), int(session.limit))
	for {
		event, data, ok := nextMCPEvent(events, session.limit)
		if !ok {
			response.Body.Close()
			return errors.New("MCP SSE endpoint did not advertise its message URL")
		}
		if event != "endpoint" {
			continue
		}
		base := response.Request.URL
		target, err := base.Parse(strings.TrimSpace(string(data)))
		if err != nil || !sameMCPOrigin(base, target) || target.User != nil || target.Fragment != "" {
			response.Body.Close()
			return errors.New("invalid MCP SSE message URL")
		}
		session.sseBody, session.sseScanner, session.postURL = response.Body, events, target.String()
		return nil
	}
}

func sameMCPOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

func nextMCPEvent(scanner *bufio.Scanner, limit int64) (string, []byte, bool) {
	var name string
	var data bytes.Buffer
	var scanned int64
	for scanner.Scan() {
		line := scanner.Bytes()
		scanned += int64(len(line)) + 1
		if scanned > limit {
			return "", nil, false
		}
		if len(line) == 0 {
			if name != "" || data.Len() > 0 {
				return name, bytes.TrimSuffix(data.Bytes(), []byte("\n")), true
			}
			continue
		}
		if value, ok := bytes.CutPrefix(line, []byte("event:")); ok {
			name = string(bytes.TrimSpace(value))
		} else if value, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			data.Write(bytes.TrimPrefix(value, []byte(" ")))
			data.WriteByte('\n')
		}
	}
	return "", nil, false
}
