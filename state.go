package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

type State struct {
	Username      string            `json:"username"`
	Password      string            `json:"password"`
	SessionSecret string            `json:"session_secret"`
	TokenName     string            `json:"token_name"`
	TokenID       int               `json:"token_id"`
	Managed       map[string]int    `json:"managed"`
	Pending       map[string]string `json:"pending"`
}

func randomSecret() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("OS entropy unavailable")
	}
	return hex.EncodeToString(b)
}

func privateDir(path string) error {
	st, e := os.Lstat(path)
	if os.IsNotExist(e) {
		return os.MkdirAll(path, 0700)
	}
	if e != nil {
		return errors.New("cannot access state directory")
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return errors.New("state paths must be real directories")
	}
	return os.Chmod(path, 0700)
}

func initDirs(dir string) error {
	for _, p := range []string{dir, filepath.Join(dir, "data"), filepath.Join(dir, "logs"), filepath.Join(dir, "runtime"), filepath.Join(dir, "bin")} {
		if err := privateDir(p); err != nil {
			return err
		}
	}
	if _, e := os.Lstat(filepath.Join(dir, "runtime", ".env")); !os.IsNotExist(e) {
		return errors.New("remove runtime/.env; this project manages the process environment explicitly")
	}
	return nil
}

func writeJSON(path string, v any) error {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	return atomicFile(path, append(b, '\n'), 0600)
}
func atomicFile(path string, b []byte, mode os.FileMode) error {
	f, e := os.CreateTemp(filepath.Dir(path), ".write-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if e = f.Chmod(mode); e == nil {
		_, e = f.Write(b)
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	if e = os.Rename(f.Name(), path); e != nil {
		return e
	}
	d, e := os.Open(filepath.Dir(path))
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}

func loadState(dir string, create bool) (*State, error) {
	p := filepath.Join(dir, "state.json")
	b, e := os.ReadFile(p)
	if os.IsNotExist(e) && create {
		if _, de := os.Stat(filepath.Join(dir, "data", "new-api.db")); !os.IsNotExist(de) {
			return nil, errors.New("database exists without state.json; restore the whole state backup")
		}
		s := &State{Username: "gateway", Password: randomSecret() + "Aa1!", SessionSecret: randomSecret(), TokenName: "dev-tools-" + randomSecret()[:12], Managed: map[string]int{}, Pending: map[string]string{}}
		return s, writeJSON(p, s)
	}
	if e != nil {
		return nil, errors.New("state unavailable; run init or restore state.json")
	}
	var s State
	if json.Unmarshal(b, &s) != nil || s.Username == "" || s.Password == "" || s.SessionSecret == "" || s.Managed == nil || s.Pending == nil {
		return nil, errors.New("invalid state.json")
	}
	if e = os.Chmod(p, 0600); e != nil {
		return nil, e
	}
	return &s, nil
}

func saveState(dir string, s *State) error { return writeJSON(filepath.Join(dir, "state.json"), s) }

func lock(dir, name string) (*os.File, error) {
	f, e := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		f.Close()
		return nil, errors.New("another gateway operation/process holds " + name)
	}
	return f, nil
}
