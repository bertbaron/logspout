package syslog

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"log/syslog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/gliderlabs/logspout/transports/tcp"
	_ "github.com/gliderlabs/logspout/transports/tls"
	_ "github.com/gliderlabs/logspout/transports/udp"

	docker "github.com/fsouza/go-dockerclient"

	"github.com/gliderlabs/logspout/router"
)

const (
	connCloseIdx = 5
)

var (
	container = &docker.Container{
		ID:   "8dfafdbc3a40",
		Name: "\x00container",
		Config: &docker.Config{
			Hostname: "8dfafdbc3a40",
		},
	}
	hostHostnameFilename = "/tmp/host_hostname"
	hostnameContent      = "hostname"
	badHostnameContent   = "hostname\r\n"
)

func TestSyslogOctetFraming(t *testing.T) {
	os.Setenv("SYSLOG_TCP_FRAMING", "octet-counted")
	defer os.Unsetenv("SYSLOG_TCP_FRAMING")

	done := make(chan string)
	addr, sock, srvWG := startServer("tcp", "", done)
	defer srvWG.Wait()
	defer os.Remove(addr)
	defer sock.Close()

	route := &router.Route{Adapter: "syslog+tcp", Address: addr}
	adapter, err := NewSyslogAdapter(route)
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.(*Adapter).conn.Close()

	stream := make(chan *router.Message)
	go adapter.Stream(stream)

	count := 1
	messages := make(chan string, count)
	go sendLogstream(stream, messages, adapter, count)

	timeout := time.After(6 * time.Second)
	msgnum := 1
	select {
	case msg := <-done:
		sizeStr := ""
		_, err := fmt.Sscan(msg, &sizeStr)
		if err != nil {
			t.Fatal("unable to scan size from message: ", err)
		}

		size, err := strconv.ParseInt(sizeStr, 10, 32)
		if err != nil {
			t.Fatal("unable to scan size from message: ", err)
		}

		expectedOctetFrame := len(sizeStr) + 1 + int(size)
		if len(msg) != expectedOctetFrame {
			t.Errorf("expected octet frame to be %d. got %d instead for message %s", expectedOctetFrame, size, msg)
		}
		return
	case <-timeout:
		t.Fatal("timeout after", msgnum, "messages")
		return
	}
}

func TestSysLogFormat(t *testing.T) {
	defer os.Unsetenv("SYSLOG_FORMAT")

	newFormat := Rfc3164Format
	os.Setenv("SYSLOG_FORMAT", string(newFormat))
	format, err := getFormat()
	if err != nil {
		t.Fatal("unexpected error: ", err)
	}
	if format != newFormat {
		t.Errorf("expected %v got %v", newFormat, format)
	}

	os.Unsetenv("SYSLOG_FORMAT")
	format, err = getFormat()
	if err != nil {
		t.Fatal("unexpected error: ", err)
	}
	if format != defaultFormat {
		t.Errorf("expected %v got %v", defaultFormat, format)
	}

	os.Setenv("SYSLOG_FORMAT", "invalid-option")
	_, err = getFormat()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestSysLogTCPFraming(t *testing.T) {
	defer os.Unsetenv("SYSLOG_TCP_FRAMING")

	newTCPFraming := OctetCountedTCPFraming
	os.Setenv("SYSLOG_TCP_FRAMING", string(newTCPFraming))
	tcpFraming, err := getTCPFraming()
	if err != nil {
		t.Fatal("unexpected error: ", err)
	}
	if tcpFraming != newTCPFraming {
		t.Errorf("expected %v got %v", newTCPFraming, tcpFraming)
	}

	os.Unsetenv("SYSLOG_TCP_FRAMING")
	tcpFraming, err = getTCPFraming()
	if err != nil {
		t.Fatal("unexpected error: ", err)
	}
	if tcpFraming != defaultTCPFraming {
		t.Errorf("expected %v got %v", defaultTCPFraming, tcpFraming)
	}

	os.Setenv("SYSLOG_TCP_FRAMING", "invalid-option")
	_, err = getTCPFraming()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestSyslogRetryCount(t *testing.T) {
	newRetryCount := uint(20)
	os.Setenv("RETRY_COUNT", strconv.Itoa(int(newRetryCount)))
	retryCount := getRetryCount()
	if retryCount != newRetryCount {
		t.Errorf("expected %v got %v", newRetryCount, retryCount)
	}

	os.Unsetenv("RETRY_COUNT")
	retryCount = getRetryCount()
	if retryCount != defaultRetryCount {
		t.Errorf("expected %v got %v", defaultRetryCount, retryCount)
	}
}

func TestSyslogReconnectOnClose(t *testing.T) {
	done := make(chan string)
	addr, sock, srvWG := startServer("tcp", "", done)
	defer srvWG.Wait()
	defer os.Remove(addr)
	defer sock.Close()
	route := &router.Route{Adapter: "syslog+tcp", Address: addr}
	adapter, err := NewSyslogAdapter(route)
	if err != nil {
		t.Fatal(err)
	}

	stream := make(chan *router.Message)
	go adapter.Stream(stream)

	count := 100
	messages := make(chan string, count)
	go sendLogstream(stream, messages, adapter, count)

	timeout := time.After(6 * time.Second)
	msgnum := 1
	for {
		select {
		case msg := <-done:
			// Don't check a message that we know was dropped
			if msgnum%connCloseIdx == 0 {
				<-messages
				msgnum++
			}
			check(t, <-messages, msg)
			msgnum++
		case <-timeout:
			adapter.(*Adapter).conn.Close()
			t.Fatal("timeout after", msgnum, "messages")
			return
		default:
			if msgnum == count {
				adapter.(*Adapter).conn.Close()
				return
			}
		}
	}
}

func TestHostnameDoesNotHaveLineFeed(t *testing.T) {
	if err := os.WriteFile(hostHostnameFilename, []byte(badHostnameContent), 0777); err != nil {
		t.Fatal(err)
	}
	testHostname := getHostname()
	if strings.Contains(testHostname, badHostnameContent) {
		t.Errorf("expected hostname to be %s. got %s in hostname %s", hostnameContent, badHostnameContent, testHostname)
	}
}

func startServer(n, la string, done chan<- string) (addr string, sock io.Closer, wg *sync.WaitGroup) {
	if n == "udp" || n == "tcp" {
		la = "127.0.0.1:0"
	}
	wg = new(sync.WaitGroup)

	l, err := net.Listen(n, la)
	if err != nil {
		log.Fatalf("startServer failed: %v", err)
	}
	addr = l.Addr().String()
	sock = l
	wg.Add(1)
	go func() {
		defer wg.Done()
		runStreamSyslog(l, done, wg)
	}()

	return
}

func runStreamSyslog(l net.Listener, done chan<- string, wg *sync.WaitGroup) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		wg.Add(1)
		go func(c net.Conn) {
			defer wg.Done()
			c.SetReadDeadline(time.Now().Add(5 * time.Second))
			b := bufio.NewReader(c)
			var i = 1
			for {
				i++
				s, err := b.ReadString('\n')
				if err != nil {
					break
				}
				done <- s
				if i%connCloseIdx == 0 {
					break
				}
			}
			c.Close()
		}(c)
	}
}

func sendLogstream(stream chan *router.Message, messages chan string, adapter router.LogAdapter, count int) {
	for i := 1; i <= count; i++ {
		msg := &Message{
			Message: &router.Message{
				Container: container,
				Data:      "test " + strconv.Itoa(i),
				Time:      time.Now(),
				Source:    "stdout",
			},
		}
		stream <- msg.Message
		b, _ := msg.Render(adapter.(*Adapter).format, adapter.(*Adapter).tmpl)
		messages <- string(b)
		time.Sleep(10 * time.Millisecond)
	}
}

func check(t *testing.T, in string, out string) {
	if in != out {
		t.Errorf("expected: %s\ngot: %s\n", in, out)
	}
}

func newPriorityMessage(source, level string) *Message {
	return &Message{&router.Message{Source: source, Level: level}}
}

// Without Level the priority must equal the pre-pipeline behavior.
func TestPriorityCompatEmptyLevel(t *testing.T) {
	tests := []struct {
		source string
		want   syslog.Priority
	}{
		{"stdout", syslog.LOG_USER | syslog.LOG_INFO},
		{"stderr", syslog.LOG_USER | syslog.LOG_ERR},
		{"journal", syslog.LOG_DAEMON | syslog.LOG_INFO},
	}
	for _, tt := range tests {
		if got := newPriorityMessage(tt.source, "").Priority(); got != tt.want {
			t.Errorf("source %s: priority = %d, want %d", tt.source, got, tt.want)
		}
	}
}

func TestPriorityWithLevel(t *testing.T) {
	tests := []struct {
		source, level string
		want          syslog.Priority
	}{
		{"stdout", "debug", syslog.LOG_USER | syslog.LOG_DEBUG},
		{"stdout", "info", syslog.LOG_USER | syslog.LOG_INFO},
		{"stderr", "notice", syslog.LOG_USER | syslog.LOG_NOTICE},
		{"stderr", "info", syslog.LOG_USER | syslog.LOG_INFO},
		{"stdout", "warning", syslog.LOG_USER | syslog.LOG_WARNING},
		{"stdout", "error", syslog.LOG_USER | syslog.LOG_ERR},
		{"stdout", "critical", syslog.LOG_USER | syslog.LOG_CRIT},
		{"journal", "error", syslog.LOG_DAEMON | syslog.LOG_ERR},
	}
	for _, tt := range tests {
		if got := newPriorityMessage(tt.source, tt.level).Priority(); got != tt.want {
			t.Errorf("%s/%s: priority = %d, want %d", tt.source, tt.level, got, tt.want)
		}
	}
}

func TestRenderLevel(t *testing.T) {
	t.Setenv("SYSLOG_HOSTNAME", "host")
	if _, err := os.Stat("/etc/host_hostname"); err == nil {
		t.Skip("/etc/host_hostname exists")
	}
	container := &docker.Container{
		Name:   "/c",
		Config: &docker.Config{},
		State:  docker.State{Pid: 7},
	}
	ts := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

	render := func(t *testing.T, m *Message) string {
		t.Helper()
		tmpl, err := getFieldTemplates(&router.Route{})
		if err != nil {
			t.Fatal(err)
		}
		buf, err := m.Render(Rfc5424Format, tmpl)
		if err != nil {
			t.Fatal(err)
		}
		return string(buf)
	}

	got := render(t, &Message{&router.Message{Container: container, Source: "stderr", Data: "x", Time: ts}})
	if want := "<11>1 2026-10-04T12:00:00Z host c 7 - - x\n"; got != want {
		t.Errorf("compat stderr: got %q, want %q", got, want)
	}
	got = render(t, &Message{&router.Message{Container: container, Source: "stdout", Data: "x", Time: ts}})
	if want := "<14>1 2026-10-04T12:00:00Z host c 7 - - x\n"; got != want {
		t.Errorf("compat stdout: got %q, want %q", got, want)
	}
	got = render(t, &Message{&router.Message{Container: container, Source: "stdout", Data: "x", Time: ts, Level: "warning"}})
	if want := "<12>1 2026-10-04T12:00:00Z host c 7 - - x\n"; got != want {
		t.Errorf("level warning: got %q, want %q", got, want)
	}

	t.Setenv("SYSLOG_DATA", "{{.Level}}: {{.Data}}")
	got = render(t, &Message{&router.Message{Container: container, Source: "stdout", Data: "x", Time: ts, Level: "error"}})
	if want := "<11>1 2026-10-04T12:00:00Z host c 7 - - error: x\n"; got != want {
		t.Errorf("level template: got %q, want %q", got, want)
	}
}
