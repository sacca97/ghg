package config

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// trusted.json records which folder paths the user has trusted (the startup
// "Do you trust the files in this folder?" dialog). Trust is per absolute
// path, like Claude Code's hasTrustDialogAccepted per project — trusting a
// folder means ghg may read its files (they feed the model) and, with
// per-command approval, execute code in it.

type trustedFile struct {
	Paths map[string]bool `json:"paths"`
}

// Trusted reports whether dir (absolute path) has been trusted.
func Trusted(dir string) bool {
	var t trustedFile
	if err := ReadJSON("trusted.json", &t); err != nil {
		return false
	}
	return t.Paths[dir]
}

// Trust records dir (absolute path) as trusted.
func Trust(dir string) error {
	var t trustedFile
	_ = ReadJSON("trusted.json", &t)
	if t.Paths == nil {
		t.Paths = map[string]bool{}
	}
	t.Paths[dir] = true
	if err := WriteJSON("trusted.json", t); err != nil {
		return err
	}
	LogEvent("trust.grant", dir)
	return nil
}

// CheckTrust gates startup on the folder-trust prompt.
func CheckTrust(wd string) (bool, error) {
	if Trusted(wd) {
		return true, nil
	}
	st, err := os.Stdin.Stat()
	if err != nil || st.Mode()&os.ModeCharDevice == 0 {
		return false, fmt.Errorf("folder %s is not trusted (run interactively once to trust it, or add it to ~/.ghg/trusted.json)", wd)
	}
	fmt.Fprintf(os.Stderr, "\nDo you trust the files in this folder?\n%s\n\n", wd)
	fmt.Fprintln(os.Stderr, "ghg may read files in this folder. Reading untrusted files may lead ghg to behave in unexpected ways.")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "With your permission ghg may execute files in this folder. Executing untrusted code is unsafe.")
	fmt.Fprintln(os.Stderr)
	fmt.Fprint(os.Stderr, "Proceed? [Y/n] ")
	ans, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false, err
	}
	if a := strings.ToLower(strings.TrimSpace(ans)); a == "" || a == "y" || a == "yes" {
		if err := Trust(wd); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}
