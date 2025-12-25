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

	if isRequest {
		for _, hf := range headers {
			if _, exists := stream.RequestHeaders[hf.Name]; !exists {
				stream.RequestHeaders[hf.Name] = hf.Value
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

			if _, exists := stream.ResponseHeaders[hf.Name]; !exists {
				stream.ResponseHeaders[hf.Name] = hf.Value
			}

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

// parseDataFrame handles DATA frames
func parseDataFrame(
	f *http2.DataFrame,
	stream *HTTP2Stream,
	isRequest bool,
	opts *ParseOptions,
) {
	data := f.Data()

	if len(data) == 0 {
		return
	}

	var payload []byte
	if stream.IsGRPC {
		// ✅ Parse gRPC frame format: [1 byte compressed][4 bytes length][message]
		payload = parseGRPCFrames(data)
	} else {
		payload = data
	}

	if isRequest {
		if opts.Base64EncodePayload {
			// Packet capture mode: replace body with base64
			stream.RequestBody = []byte(base64.StdEncoding.EncodeToString(payload))
		} else {
			// eBPF mode: accumulate raw bytes
			stream.RequestBody = append(stream.RequestBody, payload...)
		}

		// Mark complete based on mode
		if opts.WaitForEndStream {
			if f.StreamEnded() {
				stream.RequestComplete = true
			}
		} else {
			stream.RequestComplete = true
		}

	} else {
		if opts.Base64EncodePayload {
			// Packet capture mode: replace body with base64
			stream.ResponseBody = []byte(base64.StdEncoding.EncodeToString(payload))
		} else {
			// eBPF mode: accumulate raw bytes
			stream.ResponseBody = append(stream.ResponseBody, payload...)
		}

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
