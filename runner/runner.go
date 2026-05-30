package runner

import (
	"fmt"
	"log"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/gliderlabs/logspout/cfg"
	"github.com/gliderlabs/logspout/router"
)

// Options configures the Logspout runtime bootstrap.
type Options struct {
	Args      []string
	RouteURIs []string
	Version   string
}

// Run configures and starts all registered jobs.
func Run(opts Options) error {
	args := opts.Args
	if args == nil {
		args = os.Args[1:]
	}
	if len(args) == 1 && args[0] == "--version" {
		fmt.Printf("%s\n", opts.Version)
		return nil
	}

	log.Printf("# logspout %s by gliderlabs\n", opts.Version)
	log.Printf("# adapters: %s\n", strings.Join(router.AdapterFactories.Names(), " "))
	log.Printf("# options : ")
	if d := cfg.GetEnvDefault("DEBUG", ""); d != "" {
		log.Printf("debug:%s\n", d)
	}
	if b := cfg.GetEnvDefault("BACKLOG", ""); b != "" {
		log.Printf("backlog:%s\n", b)
	}
	log.Printf("persist:%s\n", cfg.GetEnvDefault("ROUTESPATH", "/mnt/routes"))

	var jobs []string
	for _, job := range router.Jobs.All() {
		if err := setupJob(job, args, opts.RouteURIs); err != nil {
			return err
		}
		if job.Name() != "" {
			jobs = append(jobs, job.Name())
		}
	}
	log.Printf("# jobs    : %s\n", strings.Join(jobs, " "))

	routes, _ := router.Routes.GetAll()
	if len(routes) > 0 {
		log.Println("# routes  :")
		w := new(tabwriter.Writer)
		w.Init(os.Stdout, 0, 8, 0, '\t', 0)
		fmt.Fprintln(w, "#   ADAPTER\tADDRESS\tCONTAINERS\tSOURCES\tOPTIONS") //nolint:errcheck
		for _, route := range routes {
			_, _ = fmt.Fprintf(w, "#   %s\t%s\t%s\t%s\t%s\n",
				route.Adapter,
				route.Address,
				route.FilterID+route.FilterName+strings.Join(route.FilterLabels, ","),
				strings.Join(route.FilterSources, ","),
				route.Options)
		}
		_ = w.Flush()
	} else {
		log.Println("# routes  : none")
	}

	for _, job := range router.Jobs.All() {
		job := job
		go func() {
			log.Fatalf("%s ended: %s", job.Name(), job.Run())
		}()
	}

	select {}
}

func setupJob(job router.Job, args []string, routeURIs []string) error {
	if routeManager, ok := job.(*router.RouteManager); ok {
		return routeManager.SetupWithArgs(args, routeURIs)
	}
	return job.Setup()
}
