package http2parser

import (
	"bytes"
	"encoding/base64"
	"testing"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

type frameWriter struct {
	buf    bytes.Buffer
	framer *http2.Framer
	hbuf   bytes.Buffer
	enc    *hpack.Encoder
}

func newFrameWriter() *frameWriter {
	w := &frameWriter{}
	w.framer = http2.NewFramer(&w.buf, nil)
	w.enc = hpack.NewEncoder(&w.hbuf)
	return w
}

func (w *frameWriter) headers(t *testing.T, streamID uint32, endStream bool, kv ...string) {
	t.Helper()
	w.hbuf.Reset()
	for i := 0; i < len(kv); i += 2 {
		if err := w.enc.WriteField(hpack.HeaderField{Name: kv[i], Value: kv[i+1]}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.framer.WriteHeaders(http2.HeadersFrameParam{
		StreamID: streamID, BlockFragment: w.hbuf.Bytes(), EndStream: endStream, EndHeaders: true,
	}); err != nil {
		t.Fatal(err)
	}
}

func (w *frameWriter) data(t *testing.T, streamID uint32, endStream bool, payload []byte) {
	t.Helper()
	if err := w.framer.WriteData(streamID, endStream, payload); err != nil {
		t.Fatal(err)
	}
}

func grpcMessage(msg []byte) []byte {
	n := len(msg)
	return append([]byte{0, byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}, msg...)
}

func ebpfOpts() *ParseOptions {
	return NewParseOptions(WithBase64Encoding(true), WithWaitForEndStream(true), WithGRPCTrailers(true))
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func TestBodySpanningDataFramesBase64(t *testing.T) {
	part1 := bytes.Repeat([]byte("a"), 16384)
	part2 := []byte("tail")

	w := newFrameWriter()
	w.headers(t, 1, false, ":method", "POST", ":path", "/upload", "content-type", "application/json")
	w.data(t, 1, false, part1)
	w.data(t, 1, true, part2)

	streams := map[uint32]*HTTP2Stream{}
	if err := ParseHTTP2Frames(w.buf.Bytes(), streams, true, ebpfOpts()); err != nil {
		t.Fatal(err)
	}
	s := streams[1]
	if !s.RequestComplete {
		t.Fatal("request should be complete")
	}
	if got, want := string(s.RequestBody), b64(append(append([]byte{}, part1...), part2...)); got != want {
		t.Errorf("body lost frames: got %d bytes, want %d", len(got), len(want))
	}
	if s.reqData != nil {
		t.Error("raw request data should be released once the request is complete")
	}
}

func TestBodySpanningDataFramesRaw(t *testing.T) {
	w := newFrameWriter()
	w.headers(t, 1, false, ":method", "POST", ":path", "/upload")
	w.data(t, 1, false, []byte("hello "))
	w.data(t, 1, true, []byte("world"))

	streams := map[uint32]*HTTP2Stream{}
	opts := NewParseOptions(WithBase64Encoding(false))
	if err := ParseHTTP2Frames(w.buf.Bytes(), streams, true, opts); err != nil {
		t.Fatal(err)
	}
	if got := string(streams[1].RequestBody); got != "hello world" {
		t.Errorf("got %q", got)
	}
}

func TestEmptyEndStreamDataFrame(t *testing.T) {
	w := newFrameWriter()
	w.headers(t, 1, false, ":method", "POST", ":path", "/items")
	w.data(t, 1, false, []byte(`{"id":1}`))
	w.data(t, 1, true, nil)

	streams := map[uint32]*HTTP2Stream{}
	if err := ParseHTTP2Frames(w.buf.Bytes(), streams, true, ebpfOpts()); err != nil {
		t.Fatal(err)
	}
	s := streams[1]
	if !s.RequestComplete {
		t.Fatal("empty DATA frame with END_STREAM should complete the request")
	}
	if got := string(s.RequestBody); got != b64([]byte(`{"id":1}`)) {
		t.Errorf("got %q", got)
	}
}

func TestGRPCRequestAndResponseUnwrapped(t *testing.T) {
	reqMsg := []byte{0x0a, 0x03, 'b', 'o', 'b'}
	respMsg := bytes.Repeat([]byte{0x12}, 20000) // larger than one 16 KB DATA frame
	respFrame := grpcMessage(respMsg)

	req := newFrameWriter()
	req.headers(t, 1, false, ":method", "POST", ":path", "/helloworld.Greeter/SayHello", "content-type", "application/grpc")
	req.data(t, 1, true, grpcMessage(reqMsg))

	resp := newFrameWriter()
	resp.headers(t, 1, false, ":status", "200", "content-type", "application/grpc")
	resp.data(t, 1, false, respFrame[:16384])
	resp.data(t, 1, false, respFrame[16384:])
	resp.headers(t, 1, true, "grpc-status", "0")

	streams := map[uint32]*HTTP2Stream{}
	opts := ebpfOpts()
	if err := ParseHTTP2Frames(req.buf.Bytes(), streams, true, opts); err != nil {
		t.Fatal(err)
	}
	if err := ParseHTTP2Frames(resp.buf.Bytes(), streams, false, opts); err != nil {
		t.Fatal(err)
	}

	s := streams[1]
	if !s.IsGRPC || !s.RequestComplete || !s.ResponseComplete {
		t.Fatalf("unexpected stream state: %+v", s)
	}
	if got := string(s.RequestBody); got != b64(reqMsg) {
		t.Errorf("request body still has gRPC prefix: %q", got)
	}
	if got := string(s.ResponseBody); got != b64(respMsg) {
		t.Errorf("response message split across DATA frames was lost: got %d bytes", len(got))
	}
	if s.GRPCStatus != "0" {
		t.Errorf("grpc-status = %q", s.GRPCStatus)
	}
}

func TestBodiesKeptOnTruncatedFrame(t *testing.T) {
	w := newFrameWriter()
	w.headers(t, 1, false, ":method", "POST", ":path", "/a")
	w.data(t, 1, true, []byte("first"))
	w.headers(t, 3, false, ":method", "POST", ":path", "/b")
	w.data(t, 3, true, []byte("second body"))
	buf := w.buf.Bytes()
	buf = buf[:len(buf)-4]

	streams := map[uint32]*HTTP2Stream{}
	if err := ParseHTTP2Frames(buf, streams, true, ebpfOpts()); err == nil {
		t.Fatal("expected error for truncated frame")
	}
	if got := string(streams[1].RequestBody); got != b64([]byte("first")) {
		t.Errorf("body parsed before the error was lost: %q", got)
	}
	if streams[3].RequestComplete {
		t.Error("truncated stream must not be complete")
	}
}
