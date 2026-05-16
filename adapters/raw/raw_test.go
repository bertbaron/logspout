package raw

import (
	"net"
	"testing"
	"time"

	docker "github.com/fsouza/go-dockerclient"

	"github.com/gliderlabs/logspout/router"
)

// mockConn is a net.Conn that records written bytes.
type mockConn struct {
	written []byte
	closed  bool
}

func (c *mockConn) Write(b []byte) (int, error)         { c.written = append(c.written, b...); return len(b), nil }
func (c *mockConn) Close() error                        { c.closed = true; return nil }
func (c *mockConn) Read(b []byte) (int, error)          { return 0, nil }
func (c *mockConn) LocalAddr() net.Addr                 { return &net.TCPAddr{} }
func (c *mockConn) RemoteAddr() net.Addr                { return &net.TCPAddr{} }
func (c *mockConn) SetDeadline(t time.Time) error       { return nil }
func (c *mockConn) SetReadDeadline(t time.Time) error   { return nil }
func (c *mockConn) SetWriteDeadline(t time.Time) error  { return nil }

// mockTransport implements router.AdapterTransport and returns a mockConn.
type mockTransport struct {
	conn *mockConn
}

func (t *mockTransport) Dial(addr string, options map[string]string) (net.Conn, error) {
	t.conn = &mockConn{}
	return t.conn, nil
}

func newTestMessage(data string) *router.Message {
	return &router.Message{
		Container: &docker.Container{
			ID:   "abc123",
			Name: "/testcont",
			Config: &docker.Config{
				Hostname: "host1",
			},
		},
		Data:   data,
		Source: "stdout",
		Time:   time.Now(),
	}
}

func TestRawAdapterStreamDefaultFormat(t *testing.T) {
	transport := &mockTransport{}
	router.AdapterTransports.Register(transport, "mock")
	defer router.AdapterTransports.Unregister("mock")

	route := &router.Route{
		Adapter: "raw+mock",
		Address: "localhost:9999",
	}
	adapter, err := NewRawAdapter(route)
	if err != nil {
		t.Fatalf("unexpected error creating adapter: %v", err)
	}

	stream := make(chan *router.Message, 1)
	done := make(chan struct{})
	go func() {
		adapter.Stream(stream)
		close(done)
	}()

	stream <- newTestMessage("hello raw")
	close(stream)
	<-done

	written := string(transport.conn.written)
	if written != "hello raw\n" {
		t.Errorf("expected 'hello raw\\n', got '%s'", written)
	}
}

func TestRawAdapterStreamCustomFormat(t *testing.T) {
	transport := &mockTransport{}
	router.AdapterTransports.Register(transport, "mock2")
	defer router.AdapterTransports.Unregister("mock2")

	t.Setenv("RAW_FORMAT", "{{.Container.ID}}: {{.Data}}\n")

	route := &router.Route{
		Adapter: "raw+mock2",
		Address: "localhost:9999",
	}
	adapter, err := NewRawAdapter(route)
	if err != nil {
		t.Fatalf("unexpected error creating adapter: %v", err)
	}

	stream := make(chan *router.Message, 1)
	done := make(chan struct{})
	go func() {
		adapter.Stream(stream)
		close(done)
	}()

	stream <- newTestMessage("custom format")
	close(stream)
	<-done

	written := string(transport.conn.written)
	expected := "abc123: custom format\n"
	if written != expected {
		t.Errorf("expected '%s', got '%s'", expected, written)
	}
}

func TestRawAdapterUnknownTransport(t *testing.T) {
	route := &router.Route{
		Adapter: "raw+nonexistent",
		Address: "localhost:9999",
	}
	_, err := NewRawAdapter(route)
	if err == nil {
		t.Error("expected error for unknown transport, got nil")
	}
}

func TestRawAdapterInvalidTemplate(t *testing.T) {
	transport := &mockTransport{}
	router.AdapterTransports.Register(transport, "mock3")
	defer router.AdapterTransports.Unregister("mock3")

	t.Setenv("RAW_FORMAT", "{{.Invalid")

	route := &router.Route{
		Adapter: "raw+mock3",
		Address: "localhost:9999",
	}
	_, err := NewRawAdapter(route)
	if err == nil {
		t.Error("expected error for invalid template, got nil")
	}
}
