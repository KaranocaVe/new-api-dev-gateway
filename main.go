package gateway

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
)

const usage = `new-api-gateway <command> [--config gateway.json] [--state DIR]

Commands:
  validate  Validate JSON without printing credentials
  install   Download and verify the locked official Linux binary
  init      Initialize an offline dedicated database and apply JSON
  run       Initialize/sync, then exec New API (one permanent process)
  apply     Sync JSON to an already running instance (no restart)
  status    Check HTTP readiness without printing credentials
  version   Show the locked upstream release

The upstream server listens on ALL interfaces, including during bootstrap.
Use only in an isolated, trusted container network; do not publish the port.
Config must have mode 0600. State defaults to $XDG_STATE_HOME/new-api-gateway.
`

func Main(args []string) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		fmt.Print(usage)
		return 0
	}
	if args[0] == "version" {
		fmt.Println("new-api-dev-gateway; upstream " + release().Tag)
		return 0
	}
	fs := flag.NewFlagSet("new-api-gateway", flag.ContinueOnError)
	cfg := fs.String("config", "gateway.json", "private JSON config")
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "state")
	}
	state := fs.String("state", filepath.Join(base, "new-api-gateway"), "persistent private state directory")
	if fs.Parse(args[1:]) != nil || fs.NArg() != 0 {
		return 2
	}
	dir, e := filepath.Abs(*state)
	if e != nil {
		return fail(errors.New("invalid state path"))
	}
	if args[0] == "validate" {
		c, e := readConfig(*cfg)
		if e != nil {
			return fail(e)
		}
		fmt.Printf("valid: %d channels, %d active models; credentials redacted\n", len(c.Channels), len(c.models()))
		return 0
	}
	if runtime.GOOS != "linux" {
		return fail(errors.New("run deployment commands inside Linux; validate/version also work on macOS"))
	}
	syscall.Umask(0077)
	if e = initDirs(dir); e != nil {
		return fail(e)
	}
	if args[0] == "install" {
		l, e := lock(dir, "sync.lock")
		if e != nil {
			return fail(e)
		}
		defer l.Close()
		if e = install(dir); e != nil {
			return fail(e)
		}
		fmt.Println("verified official New API " + release().Tag + " (" + runtime.GOARCH + ")")
		return 0
	}
	c, e := readConfig(*cfg)
	if e != nil {
		return fail(e)
	}
	switch args[0] {
	case "init":
		l, e := lock(dir, "serve.lock")
		if e != nil {
			return fail(e)
		}
		defer l.Close()
		e = prepare(c, dir)
		if e != nil {
			return fail(e)
		}
		fmt.Println("initialized; private client credentials: " + filepath.Join(dir, "client.json"))
	case "run":
		if e = run(c, dir); e != nil {
			return fail(e)
		}
	case "apply":
		l, e := lock(dir, "sync.lock")
		if e != nil {
			return fail(e)
		}
		defer l.Close()
		s, e := loadState(dir, false)
		if e != nil {
			return fail(e)
		}
		a := newAPI(c.Port)
		if e = a.login(s); e != nil {
			return fail(e)
		}
		defer a.logout()
		if e = a.sync(c, dir, s); e != nil {
			return fail(e)
		}
		fmt.Println("applied JSON; removed managed channels disabled")
	case "status":
		if e = newAPI(c.Port).call("GET", "/api/status", nil, nil); e != nil {
			return fail(e)
		}
		fmt.Printf("ready: http://127.0.0.1:%d (listener also accepts container-network connections)\n", c.Port)
	default:
		return fail(errors.New("unknown command; use --help"))
	}
	return 0
}
func fail(e error) int { fmt.Fprintln(os.Stderr, "error:", e); return 1 }
