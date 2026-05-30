package main

import (
	"log"

	"github.com/gliderlabs/logspout/runner"
)

// Version is the running version of logspout
var Version string

func main() {
	if err := runner.Run(runner.Options{
		Version: Version,
	}); err != nil {
		log.Fatal(err)
	}
}
