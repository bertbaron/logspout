package main

import (
	"log"

	"github.com/gliderlabs/logspout/launcher"
	_ "github.com/gliderlabs/logspout/modules"
)

// Version is injected at build time.
var Version string

func main() {
	if err := launcher.Run(launcher.Options{Version: Version}); err != nil {
		log.Fatal(err)
	}
}
