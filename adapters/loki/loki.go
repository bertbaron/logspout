// Package loki provides a loki adapter. Inspired by https://github.com/darkdarkdragon/logspout-loki/blob/master/loki.go
package loki

import (
	"fmt"
	"log"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/gliderlabs/logspout/cfg"
	"github.com/gliderlabs/logspout/router"
	lokiclient "github.com/livepeer/loki-client/client"
	"github.com/livepeer/loki-client/model"
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
	router.AdapterFactories.Register(NewLokiAdapter, "loki")
}

// LokiAdapter is an adapter that streams logs to Loki.
type LokiAdapter struct {
	route  *router.Route
	client *lokiclient.Client
}

func logger(v ...interface{}) {
	log.Println(v...)
}

// NewLokiAdapter creates a LokiAdapter.
func NewLokiAdapter(route *router.Route) (router.LogAdapter, error) {
	baseLabels := model.LabelSet{}
	path := "/api/prom/push"
	if route.Path != "" {
		path = route.Path
	}
	urlObject := &url.URL{
		Scheme: scheme(route.Adapter),
		User:   route.User,
		Host:   route.Address,
		Path:   path,
	}
	log.Printf("Using Loki url: %s\n", urlObject.Redacted())
	client, err := lokiclient.NewWithDefaults(urlObject.String(), baseLabels, logger)
	if err != nil {
		return nil, err
	}
	c := make(chan os.Signal)
	signal.Notify(c, os.Interrupt)
	go waitExit(client, c)

	return &LokiAdapter{
		route:  route,
		client: client,
	}, nil
}

// Stream implements the router.LogAdapter interface.
func (a *LokiAdapter) Stream(logstream chan *router.Message) {
	defer a.client.Stop()

	for m := range logstream {
		labels := model.LabelSet{
			"nodename":       hostname,
			"container_id":   m.Container.ID,
			"container_name": m.Container.Name[1:],
			"image_id":       m.Container.Image,
			"image_name":     m.Container.Config.Image,
			"command":        strings.Join(m.Container.Config.Cmd[:], " "),
			"created":        fmt.Sprintf("%s", m.Container.Created),
		}

		line := strings.TrimSpace(m.Data)
		if len(line) > 0 {
			a.client.Handle(labels, time.Now(), line)
		}
	}
}

func waitExit(client *lokiclient.Client, c chan os.Signal) {
	<-c
	client.Stop()
}

func scheme(adapter string) string {
	parts := strings.Split(adapter, "+")
	if len(parts) > 1 {
		return parts[1]
	}
	return "http"
}
