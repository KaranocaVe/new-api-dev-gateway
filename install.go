package gateway

import (
	"crypto/sha256"
	"debug/elf"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

//go:embed release.json
var releaseJSON []byte

type Release struct {
	Tag    string           `json:"tag"`
	Assets map[string]Asset `json:"assets"`
}
type Asset struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

func release() Release {
	var r Release
	if json.Unmarshal(releaseJSON, &r) != nil {
		panic("invalid embedded release lock")
	}
	return r
}
func binaryPath(dir string) string {
	return filepath.Join(dir, "bin", "new-api-"+release().Tag+"-"+runtime.GOARCH)
}

func verifyBinary(path string, a Asset) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	h := sha256.New()
	if _, e = io.Copy(h, f); e != nil {
		return e
	}
	if hex.EncodeToString(h.Sum(nil)) != a.SHA256 {
		return errors.New("New API SHA-256 mismatch")
	}
	ef, e := elf.Open(path)
	if e != nil {
		return errors.New("New API asset is not ELF")
	}
	defer ef.Close()
	want := elf.EM_X86_64
	if runtime.GOARCH == "arm64" {
		want = elf.EM_AARCH64
	}
	if ef.Class != elf.ELFCLASS64 || ef.Data != elf.ELFDATA2LSB || ef.Machine != want {
		return errors.New("New API ELF architecture mismatch")
	}
	return nil
}

func install(dir string) error {
	if runtime.GOOS != "linux" {
		return errors.New("official Linux binaries must be installed on Linux")
	}
	r := release()
	a, ok := r.Assets[runtime.GOARCH]
	if !ok {
		return errors.New("supported architectures: linux/amd64, linux/arm64")
	}
	p := binaryPath(dir)
	if verifyBinary(p, a) == nil {
		return nil
	}
	client := http.Client{Timeout: 10 * time.Minute}
	resp, e := client.Get(a.URL)
	if e != nil {
		return errors.New("official release download failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("official release download: HTTP %d", resp.StatusCode)
	}
	f, e := os.CreateTemp(filepath.Join(dir, "bin"), ".download-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	_, e = io.Copy(f, io.LimitReader(resp.Body, 512<<20))
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return errors.New("cannot save official release")
	}
	if e = verifyBinary(f.Name(), a); e != nil {
		return e
	}
	if e = os.Chmod(f.Name(), 0700); e != nil {
		return e
	}
	return os.Rename(f.Name(), p)
}
