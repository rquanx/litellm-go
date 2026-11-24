package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

// A limited session leaves voice detection to the provider and admits generation
// after the provider commits the detected audio to the conversation.
type realtimeVAD struct {
	ready chan struct{}
	automatic bool
	initialUpdate string
	stoppedItem string
	committedItem string
	mu sync.Mutex
	pending *realtimeVADUpdate
}

func (vad *realtimeVAD) automaticTurn(eventType, item string) bool {
	if item == "" {
		return false
	}
	switch eventType {
	case "input_audio_buffer.speech_stopped":
		vad.stoppedItem = item
	case "input_audio_buffer.committed":
		vad.committedItem = item
	default:
		return false
	}
	if vad.stoppedItem != vad.committedItem {
		return false
	}
	vad.stoppedItem, vad.committedItem = "", ""
	return vad.automatic
}

type realtimeVADUpdate struct {
	id string
	desired *bool
	done chan struct{}
}

func newRealtimeVAD() *realtimeVAD {
	return &realtimeVAD{ready: make(chan struct{})}
}

type realtimeVADConfig struct {
	detection map[string]any
	ga bool
}

func getRealtimeVADConfig(event map[string]any) realtimeVADConfig {
	session := objectField(event, "session")
	if detection := objectField(session, "turn_detection"); detection != nil {
		return realtimeVADConfig{detection: detection}
	}
	input := objectField(objectField(session, "audio"), "input")
	return realtimeVADConfig{detection: objectField(input, "turn_detection"), ga: true}
}

func (config realtimeVADConfig) autoResponds() bool {
	if config.detection == nil {
		return false
	}
	if kind, _ := config.detection["type"].(string); kind != "" && kind != "server_vad" && kind != "semantic_vad" {
		return false
	}
	return config.detection["create_response"] != false
}

func (vad *realtimeVAD) initialize(event map[string]any, eventID string) []byte {
	config := getRealtimeVADConfig(event)
	automatic := config.autoResponds()
	vad.automatic = automatic
	if !automatic {
		close(vad.ready)
		return nil
	}
	vad.initialUpdate = eventID
	detection := clone(config.detection)
	detection["create_response"] = false
	session := map[string]any{"turn_detection": detection}
	if config.ga {
		session = map[string]any{"type": "realtime", "audio": map[string]any{"input": map[string]any{"turn_detection": detection}}}
	}
	update, _ := json.Marshal(map[string]any{"type": "session.update", "event_id": eventID, "session": session})
	return update
}

func (vad *realtimeVAD) prepareUpdate(event map[string]any) *realtimeVADUpdate {
	config := getRealtimeVADConfig(event)
	session := objectField(event, "session")
	_, hasBeta := session["turn_detection"]
	_, hasGA := objectField(objectField(session, "audio"), "input")["turn_detection"]
	id, _ := event["event_id"].(string)
	if id == "" {
		id = "gateway-vad-" + newUUID()
		event["event_id"] = id
	}
	update := &realtimeVADUpdate{id: id, done: make(chan struct{})}
	if hasBeta || hasGA {
		automatic := config.autoResponds()
		update.desired = &automatic
		if automatic {
			config.detection["create_response"] = false
		}
	}
	vad.mu.Lock()
	vad.pending = update
	vad.mu.Unlock()
	return update
}

func (vad *realtimeVAD) confirmUpdate(event map[string]any, errorID string, failed bool) ([]byte, error) {
	vad.mu.Lock()
	defer vad.mu.Unlock()
	update := vad.pending
	if update == nil || (failed && errorID != update.id) {
		return nil, nil
	}
	vad.pending = nil
	if failed {
		close(update.done)
		return nil, nil
	}
	if update.desired != nil {
		vad.automatic = *update.desired
	}
	config := getRealtimeVADConfig(event)
	if config.autoResponds() || (vad.automatic && config.detection["create_response"] != false) {
		return nil, errors.New("provider did not disable automatic response creation")
	}
	if !vad.automatic {
		close(update.done)
		return nil, nil
	}
	config.detection["create_response"] = true
	close(update.done)
	return json.Marshal(event)
}

func waitRealtimeSettings(ctx context.Context, ready <-chan struct{}) bool {
	// Prefer an already confirmed configuration without creating a timer.
	select {
	case <-ready:
		return true
	default:
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-ready:
		return true
	case <-timer.C:
	case <-ctx.Done():
	}
	return false
}
