// drover runs any command as a set of supervised instances, load balances one port across them, and
// swaps the binary they run without dropping requests.
package main

import (
	_ "embed"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
)

var version = "dev"

// llmsTxt is the complete reference for AI agents, shipped in the binary because drover is usually copied
// into images that do not have this repository.
//
//go:embed llms.txt
var llmsTxt string

const usage = `drover: run a command as supervised instances behind one load-balanced port

Server:
  drover run -c drover.yaml
  drover run [--name app] [--listen :8080] [--instances 2] [--health /health] [--env-file f] -- <command> [args, {port}]

Client (talks to a running drover; DROVER_ADMIN, default http://127.0.0.1:9100, and DROVER_TOKEN):
  drover status [app]
  drover releases <app>
  drover env <app>                                   names of the variables, by source
  drover env set <app> KEY=VALUE... [--file] [--no-restart]
  drover env unset <app> KEY... [--file] [--no-restart]
  drover env clear <app> [--no-restart]              drop every override
  drover env put <app> <file> [--no-restart]         replace the app's env_file
  drover logs <app> [-f] [--tail n] [--instance n]
  drover metrics
  drover fetch <app> --url <url> [--version v] [--sha256 sum] [--no-activate]
                     [--header "Name: value"]... [--username u --password p]   for a download that needs a login
  drover fetch <app> --image <ref> --path </file/in/image> [--username u --password p] [...]
  drover upload <app> <file> [--version v] [--sha256 sum] [--no-activate]
  folder apps ({dir} in the command), a version is a whole folder:
  drover upload <app> <folder | archive.tar.gz | .zip> [--subdir d] [--version v]
  drover fetch <app> --url <archive url> [--subdir d]
  drover fetch <app> --image <ref> --path </folder/in/image>
  drover fetch <app> --git <repo url> [--ref branch|tag|commit] [--subdir d] [--username u --password token]
  drover activate <app> <version>
  drover rollback <app>
  drover restart <app>
  drover rm <app> <version>
  drover version
  drover llms          print the complete reference, written for AI agents
`

func main() {
	log.SetFlags(log.LstdFlags)
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "run":
		err = runServer(args)
	case "version":
		fmt.Println(version)
	case "llms", "llms.txt":
		fmt.Print(llmsTxt)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		err = runClient(cmd, args)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "drover:", err)
		os.Exit(1)
	}
}

func runServer(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	path := fs.String("c", os.Getenv("DROVER_CONFIG"), "config file")
	name := fs.String("name", "app", "app name (without -c)")
	listen := fs.String("listen", ":8080", "public address (without -c)")
	instances := fs.Int("instances", 2, "number of instances (without -c)")
	portBase := fs.Int("port-base", 9000, "first instance port (without -c)")
	folder := fs.String("folder", "", "run from this folder, which versions replace as a whole: {dir} in the command (without -c)")
	envFile := fs.String("env-file", "", "KEY=VALUE file read at every instance start (without -c)")
	health := fs.String("health", "", "health check path; empty checks that the port accepts connections (without -c)")
	admin := fs.String("admin", os.Getenv("DROVER_ADMIN_LISTEN"), "admin API address (default 127.0.0.1:9100; also DROVER_ADMIN_LISTEN)")
	data := fs.String("data", "", "where releases are kept")
	metrics := fs.String("metrics", "", "extra address that serves only /metrics, without a token (e.g. :9101)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var cfg *Config
	switch {
	case fs.NArg() > 0:
		// drover run [flags] -- command args...
		cfg = &Config{Apps: []*AppConfig{{Name: *name, Command: fs.Args(), Listen: *listen,
			Instances: *instances, PortBase: *portBase, Health: HealthConfig{Path: *health}, EnvFile: *envFile, Folder: *folder}}}
	default:
		if *path == "" {
			for _, p := range []string{"drover.yaml", "/etc/drover.yaml"} {
				if _, err := os.Stat(p); err == nil {
					*path = p
					break
				}
			}
		}
		if *path == "" {
			return fmt.Errorf("no config: use -c drover.yaml, or `drover run -- <command>`")
		}
		var err error
		if cfg, err = loadConfig(*path); err != nil {
			return err
		}
	}
	if *admin != "" {
		cfg.Admin.Listen = *admin
	}
	if *metrics != "" {
		cfg.Metrics.Listen = *metrics
	}
	if *data != "" {
		cfg.Data = *data
	}
	d, err := newDrover(cfg)
	if err != nil {
		return err
	}
	if err := d.start(); err != nil {
		return err
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	sig := <-sigs
	log.Printf("[drover] %s received, stopping", sig)
	d.stop()
	return nil
}
