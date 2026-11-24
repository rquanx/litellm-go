package runtime

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

const bedrockEventStreamType = "application/vnd.amazon.eventstream"
const bedrockStreamingHash = "STREAMING-AWS4-HMAC-SHA256-EVENTS"

func bedrockRealtimeDeployment(candidate Candidate) bool {
	return candidate.Provider == "bedrock" && (strings.Contains(candidate.ModelID, "nova-sonic") ||
		strings.Contains(candidate.ModelID, "nova-2-sonic"))
}

type bedrockRealtimeStream struct {
	ctx context.Context
	cancel context.CancelFunc
	writer *io.PipeWriter
	result <-chan *http.Response
	signer *v4.StreamSigner
	encoder *eventstream.Encoder
}

func (s *Server) openBedrockRealtime(ctx context.Context, candidate Candidate) (*bedrockRealtimeStream, error) {
	ctx, cancel := context.WithCancel(ctx)
	reader, writer := io.Pipe()
	opened := false
	defer func() {
		if !opened {
			cancel()
			reader.Close()
			writer.Close()
		}
	}()
	address := strings.TrimRight(candidate.APIBase, "/") + "/model/" + url.PathEscape(candidate.ModelID) + "/invoke-with-bidirectional-stream"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, address, reader)
	if err != nil {
		return nil, err
	}
	if request.URL.Scheme != "https" {
		return nil, errors.New("Bedrock Realtime requires HTTPS for HTTP/2")
	}
	request.Header.Set("Content-Type", bedrockEventStreamType)
	request.Header.Set("Accept", bedrockEventStreamType)
	bearer := candidate.APIKey
	if bearer == "" && candidate.CredentialRef != "" {
		bearer = os.Getenv(candidate.CredentialRef)
	}
	var signer *v4.StreamSigner
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	} else {
		credentials, err := bedrockSigningCredentials(ctx, candidate.Bedrock)
		if err != nil {
			return nil, err
		}
		request.Header.Set("X-Amz-Content-Sha256", bedrockStreamingHash)
		if err := v4.NewSigner().SignHTTP(ctx, credentials, request, bedrockStreamingHash, "bedrock", candidate.Bedrock.Region, time.Now()); err != nil {
			return nil, err
		}
		signature, err := v4.GetSignedRequestSignature(request)
		if err != nil || len(signature) == 0 {
			return nil, errors.New("could not sign Bedrock Realtime request")
		}
		signer = v4.NewStreamSigner(credentials, "bedrock", candidate.Bedrock.Region, signature)
	}
	result := make(chan *http.Response)
	connected := make(chan error, 1)
	var connectionReported sync.Once
	reportConnection := func(err error) {
		connectionReported.Do(func() { connected <- err })
	}
	request = request.WithContext(httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			conn, ok := info.Conn.(*tls.Conn)
			if !ok || conn.ConnectionState().NegotiatedProtocol != "h2" {
				reportConnection(errors.New("Bedrock Realtime endpoint did not negotiate HTTP/2"))
				cancel()
				return
			}
			reportConnection(nil)
		},
	}))
	client := *s.config.HTTPClient
	client.Timeout = 0
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	go func() {
		defer close(result)
		response, err := client.Do(request)
		if err != nil {
			_ = reader.CloseWithError(err)
			reportConnection(errors.New("could not connect to the Bedrock Realtime endpoint"))
			return
		}
		reportConnection(errors.New("Bedrock Realtime endpoint did not confirm HTTP/2"))
		select {
		case result <- response:
		case <-ctx.Done():
			response.Body.Close()
		}
	}()
	connectTimeout := 15 * time.Second
	if s.config.HTTPClient.Timeout > 0 {
		connectTimeout = min(connectTimeout, s.config.HTTPClient.Timeout)
	}
	timer := time.NewTimer(connectTimeout)
	defer timer.Stop()
	select {
	case err := <-connected:
		if err != nil {
			return nil, err
		}
	case <-timer.C:
		return nil, errors.New("Bedrock Realtime endpoint connection timed out")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	opened = true
	return &bedrockRealtimeStream{ctx: ctx, cancel: cancel, writer: writer, result: result,
		signer: signer, encoder: eventstream.NewEncoder()}, nil
}

func (stream *bedrockRealtimeStream) close() {
	stream.cancel()
	stream.writer.Close()
}

func (stream *bedrockRealtimeStream) send(payload []byte) error {
	frame := eventstream.Message{Headers: eventstream.Headers{
		{Name: ":message-type", Value: eventstream.StringValue("event")},
		{Name: ":event-type", Value: eventstream.StringValue("chunk")},
		{Name: ":content-type", Value: eventstream.StringValue("application/octet-stream")},
	}, Payload: payload}
	if stream.signer == nil {
		return stream.encoder.Encode(stream.writer, frame)
	}
	var encoded bytes.Buffer
	if err := stream.encoder.Encode(&encoded, frame); err != nil {
		return err
	}
	return stream.sendSigned(encoded.Bytes())
}

func (stream *bedrockRealtimeStream) sendSigned(payload []byte) error {
	now := time.Now().UTC()
	// EventStream timestamp header encoding is a length-prefixed name, type
	// 8 and an eight-byte big-endian timestamp in milliseconds.
	header := [15]byte{5, ':', 'd', 'a', 't', 'e', 8}
	binary.BigEndian.PutUint64(header[7:], uint64(now.UnixMilli()))
	signature, err := stream.signer.GetSignature(stream.ctx, header[:], payload, now)
	if err != nil {
		return err
	}
	return stream.encoder.Encode(stream.writer, eventstream.Message{Headers: eventstream.Headers{
		{Name: ":date", Value: eventstream.TimestampValue(now)},
		{Name: ":chunk-signature", Value: eventstream.BytesValue(signature)},
	}, Payload: payload})
}

func (stream *bedrockRealtimeStream) finish() error {
	if stream.signer != nil {
		if err := stream.sendSigned(nil); err != nil {
			return err
		}
	}
	return stream.writer.Close()
}

func (stream *bedrockRealtimeStream) response(ctx context.Context) (*http.Response, error) {
	select {
	case response := <-stream.result:
		if response == nil {
			return nil, errors.New("could not open Bedrock Realtime stream")
		}
		return response, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
