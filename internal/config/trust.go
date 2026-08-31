package config

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
