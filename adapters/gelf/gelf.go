// Package gelf provides a GELF adapter based on https://github.com/micahhausler/logspout-gelf
package gelf

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"log"
	"os"
	"strings"
	"time"

	"github.com/Graylog2/go-gelf/gelf"
	"github.com/gliderlabs/logspout/cfg"
	"github.com/gliderlabs/logspout/router"
)

var hostname string

func getHostname() string {
	content, err := os.ReadFile("/etc/host_hostname")
	if err == nil && len(content) > 0 {
		hostname = strings.TrimRight(string(content), "\r\n")
	} else {
		hostname = cfg.GetEnvDefault("SYSLOG_HOSTNAME", "{{.Container.Config.Hostname}}")
	}
	return hostname
}

func init() {
	hostname = getHostname()
	router.AdapterFactories.Register(NewGelfAdapter, "gelf")
}

// Adapter is an adapter that streams UDP JSON to Graylog
type Adapter struct {
	writer gelf.Writer
	route  *router.Route
}

// NewGelfAdapter creates an Adapter with UDP as the default transport.
func NewGelfAdapter(route *router.Route) (router.LogAdapter, error) {
	gelfWriter, err := gelfWriter(route)
	if err != nil {
		return nil, err
	}

	return &Adapter{
		route:  route,
		writer: gelfWriter,
	}, nil
}

func gelfWriter(route *router.Route) (gelf.Writer, error) {
	transport := route.AdapterTransport("udp")
	switch transport {
	case "udp":
		return gelf.NewUDPWriter(route.Address)
	case "tcp":
		return gelf.NewTCPWriter(route.Address)
	case "tls":
		tlsConfig := &tls.Config{}
		return gelf.NewTLSWriter(route.Address, tlsConfig)
	}
	return nil, errors.New("unknown transport: " + transport)
}

// Stream implements the router.LogAdapter interface.
func (a *Adapter) Stream(logstream chan *router.Message) {
	for message := range logstream {
		m := &Message{message}
		level := gelf.LOG_INFO
		if m.Source == "stderr" {
			level = gelf.LOG_ERR
		}
		extra, err := m.getExtraFields()
		if err != nil {
			log.Println("Graylog:", err)
			continue
		}

		msg := gelf.Message{
			Version:  "1.1",
			Host:     hostname,
			Short:    m.Message.Data,
			TimeUnix: float64(m.Message.Time.UnixNano()/int64(time.Millisecond)) / 1000.0,
			Level:    int32(level),
			RawExtra: extra,
		}

		if err := a.writer.WriteMessage(&msg); err != nil {
			log.Println("Graylog:", err)
			continue
		}
	}
}

type Message struct {
	*router.Message
}

func (m Message) getExtraFields() (json.RawMessage, error) {

	extra := map[string]interface{}{
		"_container_id":   m.Container.ID,
		"_container_name": m.Container.Name[1:], // might be better to use strings.TrimLeft() to remove the first /
		"_image_id":       m.Container.Image,
		"_image_name":     m.Container.Config.Image,
		"_command":        strings.Join(m.Container.Config.Cmd[:], " "),
		"_created":        m.Container.Created,
	}
	for name, label := range m.Container.Config.Labels {
		if len(name) > 5 && strings.ToLower(name[0:5]) == "gelf_" {
			extra[name[4:]] = label
		}
	}
	swarmnode := m.Container.Node
	if swarmnode != nil {
		extra["_swarm_node"] = swarmnode.Name
	}

	rawExtra, err := json.Marshal(extra)
	if err != nil {
		return nil, err
	}
	return rawExtra, nil
}
