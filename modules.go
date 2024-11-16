package main

import (
	_ "github.com/gliderlabs/logspout/adapters/gelf"
	_ "github.com/gliderlabs/logspout/adapters/loki"
	_ "github.com/gliderlabs/logspout/adapters/multiline"
	_ "github.com/gliderlabs/logspout/adapters/raw"
	_ "github.com/gliderlabs/logspout/adapters/splunk"
	_ "github.com/gliderlabs/logspout/adapters/syslog"
	_ "github.com/gliderlabs/logspout/healthcheck"
	_ "github.com/gliderlabs/logspout/httpstream"
	_ "github.com/gliderlabs/logspout/routesapi"
	_ "github.com/gliderlabs/logspout/transports/tcp"
	_ "github.com/gliderlabs/logspout/transports/tls"
	_ "github.com/gliderlabs/logspout/transports/udp"
	_ "github.com/looplab/logspout-logstash"
)
