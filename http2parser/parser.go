package http2parser

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"strings"

	"golang.org/x/net/http2/hpack"

	"golang.org/x/net/http2"
)

// HTTP2Stream represents a single HTTP/2 stream with request and response
type HTTP2Stream struct {
	StreamID         uint32
	RequestHeaders   map[string]string
	RequestBody      []byte
	ResponseHeaders  map[string]string
	ResponseBody     []byte
	Method           string
	Path             string
	StatusCode       int
	Status           string
	RequestComplete  bool
	ResponseComplete bool

	// gRPC specific fields
	IsGRPC       bool
	GRPCStatus   string
	GRPCMessage  string
	HeadersCount int // Track number of HEADERS frames (for gRPC trailing headers)

	// Raw DATA bytes per direction; turned into RequestBody/ResponseBody by finalizeBodies.
	reqData  []byte
	respData []byte
}

// ParseOptions configures HTTP/2 parsing behavior
type ParseOptions struct {
	// Base64EncodePayload encodes payloads as base64 (packet capture mode)
	Base64EncodePayload bool

	// WaitForEndStream requires END_STREAM flag before marking complete (eBPF mode)
	WaitForEndStream bool

	// HandleGRPCTrailers waits for gRPC trailing headers
	HandleGRPCTrailers bool

	// MaxHeaderTableSize for HPACK decoder
	MaxHeaderTableSize uint32
}

func DefaultOptions() *ParseOptions {
	return &ParseOptions{
		Base64EncodePayload: false,
		WaitForEndStream:    true,
		HandleGRPCTrailers:  true,
		MaxHeaderTableSize:  4096,
	}
}

func NewParseOptions(modifiers ...func(*ParseOptions)) *ParseOptions {
	opts := DefaultOptions()
	for _, modifier := range modifiers {
		modifier(opts)
	}
	return opts
}

func WithBase64Encoding(enable bool) func(*ParseOptions) {
	return func(opts *ParseOptions) {
		opts.Base64EncodePayload = enable
	}
}

func WithWaitForEndStream(enable bool) func(*ParseOptions) {
	return func(opts *ParseOptions) {
		opts.WaitForEndStream = enable
	}
}

func WithGRPCTrailers(enable bool) func(*ParseOptions) {
	return func(opts *ParseOptions) {
		opts.HandleGRPCTrailers = enable
	}
}

func WithMaxHeaderTableSize(size uint32) func(*ParseOptions) {
	return func(opts *ParseOptions) {
		opts.MaxHeaderTableSize = size
	}
}

func ParseHTTP2Frames(
	buffer []byte,
	streams map[uint32]*HTTP2Stream,
	isRequest bool,
	opts *ParseOptions,
) error {
	if opts == nil {
		opts = DefaultOptions()
	}

	framer := http2.NewFramer(nil, bytes.NewReader(buffer))
	framer.SetMaxReadFrameSize(1 << 20) // 1MB max frame size

	decoder := hpack.NewDecoder(opts.MaxHeaderTableSize, nil)

	// Runs on error returns too, so streams parsed before a bad frame keep their bodies.
	defer finalizeBodies(streams, isRequest, opts)

	for {
		frame, err := framer.ReadFrame()
		if err != nil {
			if err != io.EOF {
				return fmt.Errorf("error reading frame: %w", err)
			}
			break
		}

		streamID := frame.Header().StreamID

		if streamID == 0 {
			continue
		}

		stream, exists := streams[streamID]
		if !exists {
			stream = &HTTP2Stream{
				StreamID:        streamID,
				RequestHeaders:  make(map[string]string),
				ResponseHeaders: make(map[string]string),
			}
			streams[streamID] = stream
		}

		switch f := frame.(type) {
		case *http2.HeadersFrame:
			err := parseHeadersFrame(f, stream, isRequest, decoder, opts)
			if err != nil {
				return fmt.Errorf("error parsing headers: %w", err)
			}

		case *http2.DataFrame:
			parseDataFrame(f, stream, isRequest, opts)

		case *http2.RSTStreamFrame:
			// Stream was reset - do nothing for now
		}
	}

	return nil
}

func parseHeadersFrame(
	f *http2.HeadersFrame,
	stream *HTTP2Stream,
	isRequest bool,
	decoder *hpack.Decoder,
	opts *ParseOptions,
) error {
	headerBlock := f.HeaderBlockFragment()
	headers, err := decoder.DecodeFull(headerBlock)
	if err != nil {
		return fmt.Errorf("HPACK decode failed: %w", err)
	}

	// Interim responses (100 Continue, 103 Early Hints) precede the final response;
	// skip them so status and headers come from the final one. Decoding above still
	// ran, which keeps the HPACK dynamic table in sync.
	if !isRequest && isInformational(headers) {
		return nil
	}

	if isRequest {
		for _, hf := range headers {
			addHeader(stream.RequestHeaders, hf.Name, hf.Value)

			// Requests are parsed before responses, so detect gRPC here too;
			// otherwise request bodies keep their gRPC length prefix.
			if hf.Name == "content-type" && strings.HasPrefix(hf.Value, "application/grpc") {
				stream.IsGRPC = true
			}

			// Extract pseudo-headers
			if hf.Name == ":method" {
				stream.Method = hf.Value
			} else if hf.Name == ":path" {
				stream.Path = hf.Value
			}
		}

		// Mark complete based on mode
		if opts.WaitForEndStream {
			if f.StreamEnded() {
				stream.RequestComplete = true
			}
		} else {
			// Packet capture mode: mark complete after seeing headers
			stream.RequestComplete = true
		}

	} else {
		// Response headers
		stream.HeadersCount++

		for _, hf := range headers {
			// Detect gRPC from content-type in FIRST HEADERS frame
			if hf.Name == "content-type" && strings.HasPrefix(hf.Value, "application/grpc") {
				stream.IsGRPC = true
			}

			// Capture gRPC status from TRAILING HEADERS frame
			if hf.Name == "grpc-status" {
				stream.GRPCStatus = hf.Value
			}
			if hf.Name == "grpc-message" {
				stream.GRPCMessage = hf.Value
			}

			addHeader(stream.ResponseHeaders, hf.Name, hf.Value)

			if hf.Name == ":status" && stream.StatusCode == 0 {
				stream.Status = hf.Value
				fmt.Sscanf(hf.Value, "%d", &stream.StatusCode)
			}
		}

		if opts.WaitForEndStream {
			// eBPF mode: wait for END_STREAM
			if f.StreamEnded() {
				stream.ResponseComplete = true
			}
		} else {
			// Packet capture mode: handle gRPC trailing headers
			if opts.HandleGRPCTrailers && stream.IsGRPC {
				// For gRPC, wait for trailing headers (2nd HEADERS frame)
				if stream.HeadersCount >= 2 {
					stream.ResponseComplete = true
				}
			} else {
				// Non-gRPC: mark complete after first HEADERS
				stream.ResponseComplete = true
			}
		}
	}

	return nil
}

// addHeader stores a header field, combining repeated fields instead of keeping only the
// first: cookies are joined with "; " (RFC 9113 §8.2.3, HTTP/2 clients send one field per
// cookie) and other fields with ", " (RFC 9110 §5.3).
func addHeader(headers map[string]string, name, value string) {
	prev, exists := headers[name]
	switch {
	case !exists:
		headers[name] = value
	case name == "cookie":
		headers[name] = prev + "; " + value
	default:
		headers[name] = prev + ", " + value
	}
}

// isInformational reports whether a response header block is a 1xx interim response.
func isInformational(headers []hpack.HeaderField) bool {
	for _, hf := range headers {
		if hf.Name == ":status" {
			return len(hf.Value) == 3 && hf.Value[0] == '1'
		}
	}
	return false
}

// parseDataFrame handles DATA frames
func parseDataFrame(
	f *http2.DataFrame,
	stream *HTTP2Stream,
	isRequest bool,
	opts *ParseOptions,
) {
	// Copy out: f.Data() points into the framer's buffer, which is reused by the next ReadFrame.
	// Empty frames still matter, they often carry END_STREAM.
	data := f.Data()

	if isRequest {
		stream.reqData = append(stream.reqData, data...)

		// Mark complete based on mode
		if opts.WaitForEndStream {
			if f.StreamEnded() {
				stream.RequestComplete = true
			}
		} else {
			stream.RequestComplete = true
		}

	} else {
		stream.respData = append(stream.respData, data...)

		// Mark complete based on mode
		if opts.WaitForEndStream {
			if f.StreamEnded() {
				stream.ResponseComplete = true
			}
		} else {
			stream.ResponseComplete = true
		}
	}
}

// finalizeBodies turns the DATA bytes collected for one direction into RequestBody or
// ResponseBody: gRPC messages are unwrapped, then the result is base64 encoded if asked.
// Working on the whole body (not frame by frame) keeps bodies and gRPC messages that
// span several DATA frames intact. Raw bytes are released once the direction is complete.
func finalizeBodies(streams map[uint32]*HTTP2Stream, isRequest bool, opts *ParseOptions) {
	for _, s := range streams {
		data, body, complete := &s.respData, &s.ResponseBody, s.ResponseComplete
		if isRequest {
			data, body, complete = &s.reqData, &s.RequestBody, s.RequestComplete
		}
		if *data == nil {
			continue
		}

		payload := *data
		if s.IsGRPC {
			payload = parseGRPCFrames(payload)
		}
		if opts.Base64EncodePayload {
			encoded := make([]byte, base64.StdEncoding.EncodedLen(len(payload)))
			base64.StdEncoding.Encode(encoded, payload)
			payload = encoded
		}
		*body = payload

		if complete {
			*data = nil
		}
	}
}

// parseGRPCFrames extracts gRPC messages from HTTP/2 DATA frames
// gRPC frame format: [1 byte compressed][4 bytes length][message]
func parseGRPCFrames(data []byte) []byte {
	var result []byte
	offset := 0

	for offset < len(data) {
		// Need at least 5 bytes for gRPC frame header
		if offset+5 > len(data) {
			break
		}

		compressed := data[offset]
		messageLength := binary.BigEndian.Uint32(data[offset+1 : offset+5])

		// Check if we have the full message
		if offset+5+int(messageLength) > len(data) {
			break
		}

		message := data[offset+5 : offset+5+int(messageLength)]

		if compressed == 1 {
			// TODO: Handle compressed messages (gzip decompression)
			// For now, just note that it's compressed
		}

		// Append the raw protobuf message
		result = append(result, message...)

		offset += 5 + int(messageLength)
	}

	return result
}

// GetCompleteStreams returns only streams where both request and response are complete
func GetCompleteStreams(streams map[uint32]*HTTP2Stream) map[uint32]*HTTP2Stream {
	complete := make(map[uint32]*HTTP2Stream)
	for streamID, stream := range streams {
		if stream.RequestComplete && stream.ResponseComplete {
			complete[streamID] = stream
		}
	}
	return complete
}

// GetProtocolType returns the protocol type string for output
func (s *HTTP2Stream) GetProtocolType() string {
	if s.IsGRPC {
		return "gRPC"
	}
	return "HTTP/2.0"
}
