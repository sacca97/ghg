package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/sacca97/ghg/internal/sys"
)

// CredentialStatus describes the local state of subscription authentication.
type CredentialStatus struct {
	Configured bool
	AccountID  string
	ExpiresAt  time.Time
	Expired    bool
}

// LoginOptions configures an interactive or automated browser login flow.
type LoginOptions struct {
	OpenBrowser bool
	Printer     func(url string)
	Prompt      func(label string) (string, error)
	Port        int
	AuthDir     string
}

type pkceValues struct {
	Verifier  string
	Challenge string
	State     string
}

func generatePKCE() (pkceValues, error) {
	verifierBytes := make([]byte, 32)
	if _, err := rand.Read(verifierBytes); err != nil {
		return pkceValues{}, err
	}
	verifier := base64.RawURLEncoding.EncodeToString(verifierBytes)
	hash := sha256.Sum256([]byte(verifier))
	state, err := generateOAuthState()
	if err != nil {
		return pkceValues{}, err
	}
	return pkceValues{
		Verifier:  verifier,
		Challenge: base64.RawURLEncoding.EncodeToString(hash[:]),
		State:     state,
	}, nil
}

func generateOAuthState() (string, error) {
	stateBytes := make([]byte, 16)
	if _, err := rand.Read(stateBytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(stateBytes), nil
}

func startOAuthCallback(listener net.Listener, path, state string) (*http.Server, <-chan string, <-chan error) {
	codeChan := make(chan string, 1)
	errChan := make(chan error, 1)
	mux := http.NewServeMux()
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if oauthErr := query.Get("error"); oauthErr != "" {
			desc := query.Get("error_description")
			if desc == "" {
				desc = oauthErr
			}
			http.Error(w, "Authentication failed: "+desc, http.StatusBadRequest)
			errChan <- fmt.Errorf("oauth error: %s", desc)
			return
		}
		if query.Get("state") == "" || query.Get("state") != state {
			http.Error(w, "State mismatch", http.StatusBadRequest)
			errChan <- errors.New("oauth state mismatch")
			return
		}
		code := query.Get("code")
		if code == "" {
			http.Error(w, "Missing authorization code", http.StatusBadRequest)
			errChan <- errors.New("missing authorization code")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<!DOCTYPE html><html><body><h3>ghg authentication successful</h3><p>You can close this tab.</p></body></html>`))
		codeChan <- code
	})
	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(listener) }()
	return server, codeChan, errChan
}

func stopOAuthCallback(server *http.Server, listener net.Listener) {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if server != nil {
		_ = server.Shutdown(shutdownCtx)
	}
	if listener != nil {
		_ = listener.Close()
	}
}

func loadJSON(path string, dst any) (bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(data, dst); err != nil {
		return false, fmt.Errorf("malformed credentials in %s: %w", path, err)
	}
	return true, nil
}

func saveJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create auth dir: %w", err)
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-")
	if err != nil {
		return fmt.Errorf("write tmp creds: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write tmp creds: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write tmp creds: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename creds: %w", err)
	}
	return nil
}

func lockFile(f *os.File) error {
	if !sys.FileLocksSupported() {
		return nil
	}
	return sys.LockFile(f)
}

func unlockFile(f *os.File) error {
	if !sys.FileLocksSupported() {
		return nil
	}
	return sys.UnlockFile(f)
}

func acquireOAuthFileLock(dir, path string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := lockFile(f); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("acquire lock: %w", err)
	}
	return f, nil
}

func releaseOAuthFileLock(f *os.File) {
	if f == nil {
		return
	}
	_ = unlockFile(f)
	_ = f.Close()
}

func openBrowser(target string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", target).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", target).Start()
	default:
		return exec.Command("xdg-open", target).Start()
	}
}
