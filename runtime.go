package gateway

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func environment(c Config, dir string, s *State, port int) []string {
	fixed := map[string]string{
		"PORT": strconv.Itoa(port), "SESSION_SECRET": s.SessionSecret, "CRYPTO_SECRET": s.SessionSecret,
		"SQLITE_PATH": filepath.Join(dir, "data", "new-api.db") + "?_pragma=busy_timeout(30000)&_pragma=journal_mode(WAL)&_txlock=immediate",
		"SQL_DSN":     "", "LOG_SQL_DSN": "", "REDIS_CONN_STRING": "", "NODE_TYPE": "master",
		"GIN_MODE": "release", "DEBUG": "false", "ENABLE_PPROF": "false", "MEMORY_CACHE_ENABLED": "false",
		"BATCH_UPDATE_ENABLED": "false", "ERROR_LOG_ENABLED": "false", "UPDATE_TASK": "false", "TASK_PLUGIN_ENABLED": "false",
		"CHANNEL_UPSTREAM_MODEL_UPDATE_TASK_ENABLED": "false", "CHANNEL_UPDATE_FREQUENCY": "",
		"GENERATE_DEFAULT_TOKEN": "false", "PASSWORD_LOGIN_ENCRYPTION_ENABLED": "false", "TRUSTED_PROXIES": "none",
		"CountToken": "false", "GET_MEDIA_TOKEN": "false", "GET_MEDIA_TOKEN_NOT_STREAM": "false", "DIFY_DEBUG": "false",
		"TLS_INSECURE_SKIP_VERIFY": "false", "UMAMI_WEBSITE_ID": "", "GOOGLE_ANALYTICS_ID": "", "VERSION": "",
	}
	for k, v := range runtimeDefaults {
		fixed[k] = v
	}
	for k, v := range c.Runtime {
		fixed[k] = v
	}
	env := make([]string, 0, len(fixed)+len(os.Environ()))
	for _, e := range os.Environ() {
		k, _, _ := strings.Cut(e, "=")
		if _, ok := fixed[k]; !ok {
			env = append(env, e)
		}
	}
	for k, v := range fixed {
		env = append(env, k+"="+v)
	}
	return env
}

func reservePort() (int, error) {
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		return 0, e
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func bootstrap(c Config, dir string, s *State) error {
	port, e := reservePort()
	if e != nil {
		return e
	}
	log, e := os.OpenFile(filepath.Join(dir, "logs", "bootstrap.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	defer log.Close()
	cmd := exec.Command(binaryPath(dir), "--log-dir", filepath.Join(dir, "logs"))
	cmd.Dir = filepath.Join(dir, "runtime")
	cmd.Env = environment(c, dir, s, port)
	cmd.Stdout = log
	cmd.Stderr = log
	if e = cmd.Start(); e != nil {
		return errors.New("cannot start official binary (check architecture/runtime compatibility)")
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(35 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}
	defer stop()
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sig)
	a := newAPI(port)
	ready := false
	for deadline := time.Now().Add(90 * time.Second); time.Now().Before(deadline); {
		select {
		case <-done:
			stopped = true
			return errors.New("bootstrap exited; inspect private logs/bootstrap.log")
		case <-sig:
			return errors.New("bootstrap interrupted")
		default:
		}
		var setup struct {
			Status   bool `json:"status"`
			RootInit bool `json:"root_init"`
		}
		if a.call("GET", "/api/setup", nil, &setup) == nil {
			if !setup.Status {
				if setup.RootInit {
					return errors.New("unexpected pre-existing admin; use a dedicated state directory")
				}
				if e = a.call("POST", "/api/setup", map[string]any{"username": s.Username, "password": s.Password, "confirmPassword": s.Password, "SelfUseModeEnabled": true, "DemoSiteEnabled": false}, nil); e != nil {
					return e
				}
			}
			// Upstream installs SIGTERM handling 100ms after opening its listener.
			// Avoid racing that window when a repeat sync is very fast.
			time.Sleep(200 * time.Millisecond)
			ready = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !ready {
		return errors.New("bootstrap readiness timeout; inspect private logs/bootstrap.log")
	}
	if e = a.login(s); e != nil {
		return e
	}
	defer a.logout()
	return a.sync(c, dir, s)
}

func run(c Config, dir string) error {
	serve, e := lock(dir, "serve.lock")
	if e != nil {
		return e
	}
	defer serve.Close()
	if e = prepare(c, dir); e != nil {
		return e
	}
	s, e := loadState(dir, false)
	if e != nil {
		return e
	}
	env := environment(c, dir, s, c.Port)
	// Keep the flock open across exec so another run/init cannot open this DB.
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, serve.Fd(), syscall.F_SETFD, 0); errno != 0 {
		return errno
	}
	if e = os.Chdir(filepath.Join(dir, "runtime")); e != nil {
		return e
	}
	fmt.Fprintf(os.Stderr, "New API %s; port %d on all container interfaces; one steady-state process\n", release().Tag, c.Port)
	return syscall.Exec(binaryPath(dir), []string{binaryPath(dir), "--log-dir", filepath.Join(dir, "logs")}, env)
}

func prepare(c Config, dir string) error {
	sync, e := lock(dir, "sync.lock")
	if e != nil {
		return e
	}
	defer sync.Close()
	if e = install(dir); e != nil {
		return e
	}
	s, e := loadState(dir, true)
	if e != nil {
		return e
	}
	return bootstrap(c, dir, s)
}
