// Permission gating for the tools that touch the world: bash, write, edit.
// Gate is the human UX layer. ToolRuntime is the containment and deterministic
// capability layer; keeping these separate means a missing TUI cannot turn a
// headless run into unrestricted execution.
package tools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/sacca97/ghg/internal/sandbox"
)

// GateDecision is the user's answer to a permission prompt.
type GateDecision int

const (
	GateAllowOnce GateDecision = iota
	GateAllowAlways
	GateReject
)

// GateRequest describes one gated tool call for the prompt.
type GateRequest struct {
	Tool    string // bash | write | edit
	Command string // the bash command or the file path
	Rule    string // the rule "always" would install (arity-collapsed)
}

// Gate is the installed permission hook. It returns the decision and, on
// reject, a free-text redirect that goes back to the model. Nil = allow.
var Gate func(GateRequest) (GateDecision, string)

// arity maps a command prefix to how many tokens define "the command" —
// longest prefix wins, flags never count. Compact version of opencode's
// generated table (permission/arity.ts); the common cases carry the value.
var arity = map[string]int{
	// one-token commands: the binary alone is the rule
	"ls": 1, "cat": 1, "pwd": 1, "grep": 1, "find": 1, "echo": 1,
	"rm": 1, "mv": 1, "cp": 1, "mkdir": 1, "touch": 1, "which": 1,
	// two-token: binary + subcommand
	"git": 2, "npm": 2, "pnpm": 2, "yarn": 2, "go": 2, "cargo": 2,
	"docker": 2, "kubectl": 2, "brew": 2, "apt": 2, "pip": 2,
	// three-token where the shorter prefix under-specifies
	"npm run": 3, "pnpm run": 3, "go tool": 3, "docker compose": 3, "git submodule": 3,
}

// CommandRule collapses a simple shell command to its arity rule. A compound
// command is stored as one normalized full-command rule so an approval for
// "git status" can never cover "git status && rm -rf ...".
func CommandRule(command string) string {
	segments, err := SegmentShell(command)
	if err != nil {
		return normalizeShellText(command)
	}
	if len(segments) != 1 {
		return normalizeShellText(command)
	}
	tokens := segments[0].Argv
	if len(tokens) == 0 {
		return ""
	}
	tokens = unwrapTransparent(tokens)
	if len(tokens) == 0 {
		return ""
	}
	// longest matching prefix wins
	for n := len(tokens); n > 0; n-- {
		prefix := strings.Join(tokens[:n], " ")
		if a, ok := arity[prefix]; ok {
			return strings.Join(tokens[:min(a, len(tokens))], " ")
		}
	}
	return tokens[0] // unknown command: the binary is the rule
}

// SegmentShell performs the deliberately small parsing needed at the
// approval boundary. It is not a shell interpreter: substitutions and nested
// command syntax are opaque and fail closed. Quoted operators remain part of
// their surrounding argument.
func SegmentShell(command string) ([]CommandSegment, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return nil, errors.New("empty command")
	}
	var segments []CommandSegment
	start := 0
	pending := ""
	var quote byte
	for i := 0; i < len(command); i++ {
		c := command[i]
		if c == '\\' {
			i++
			continue
		}
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
		case '`':
			return nil, errors.New("command substitution is opaque")
		case '$':
			if i+1 < len(command) && command[i+1] == '(' {
				return nil, errors.New("command substitution is opaque")
			}
		case '(':
			return nil, errors.New("nested shell syntax is opaque")
		case ';', '|', '&', '>', '<', '\n', '\r':
			// Check for descriptor duplication / close (e.g. 2>&1, >&1, 1>&2, >&2, 2>&-, 1>&-)
			if (c == '>' || c == '<') && i+1 < len(command) && command[i+1] == '&' &&
				i+2 < len(command) && ((command[i+2] >= '0' && command[i+2] <= '9') || command[i+2] == '-') {
				digitStart := i
				for digitStart > start && command[digitStart-1] >= '0' && command[digitStart-1] <= '9' {
					digitStart--
				}
				splitPos := i
				if digitStart == start || (digitStart > start && unicode.IsSpace(rune(command[digitStart-1]))) {
					splitPos = digitStart
				}
				if text := strings.TrimSpace(command[start:splitPos]); text != "" {
					argv, err := shellWords(text)
					if err != nil {
						return nil, err
					}
					segments = append(segments, CommandSegment{Command: text, Argv: argv, Operator: pending})
				}
				pending = ""
				start = i + 3
				i = i + 2
				continue
			}

			// Check for numbered file redirection (e.g. 2>, 1>, 0<, 2>>, 1>>)
			splitPos := i
			if c == '>' || c == '<' {
				digitStart := i
				for digitStart > start && command[digitStart-1] >= '0' && command[digitStart-1] <= '9' {
					digitStart--
				}
				if digitStart < i && (digitStart == start || (digitStart > start && unicode.IsSpace(rune(command[digitStart-1])))) {
					splitPos = digitStart
				}
			}

			if text := strings.TrimSpace(command[start:splitPos]); text != "" {
				argv, err := shellWords(text)
				if err != nil {
					return nil, err
				}
				segments = append(segments, CommandSegment{Command: text, Argv: argv, Operator: pending})
			}
			operator := string(c)
			if c == '\n' || c == '\r' {
				operator = ";"
				if c == '\r' && i+1 < len(command) && command[i+1] == '\n' {
					i++
				}
			}
			if c == '&' && i+2 < len(command) && command[i:i+3] == "&>>" {
				operator = "&>>"
				i += 2
			} else if c != '\n' && c != '\r' && i+1 < len(command) {
				two := command[i : i+2]
				if two == "&&" || two == "||" || two == ">>" || two == "<<" || two == "|&" || two == "&>" {
					operator = two
					i++
				}
			}
			pending = operator
			start = i + 1
		}
	}
	if quote != 0 {
		return nil, errors.New("unclosed shell quote")
	}
	if text := strings.TrimSpace(command[start:]); text != "" {
		argv, err := shellWords(text)
		if err != nil {
			return nil, err
		}
		segments = append(segments, CommandSegment{Command: text, Argv: argv, Operator: pending})
	}
	if len(segments) == 0 {
		return nil, errors.New("empty command")
	}
	return segments, nil
}

func shellWords(text string) ([]string, error) {
	var words []string
	var b strings.Builder
	var quote byte
	started := false
	for i := 0; i < len(text); i++ {
		c := text[i]
		if c == '\\' && quote != '\'' {
			if i+1 >= len(text) {
				return nil, errors.New("trailing shell escape")
			}
			i++
			b.WriteByte(text[i])
			started = true
			continue
		}
		if quote != 0 {
			if c == quote {
				quote = 0
			} else {
				b.WriteByte(c)
			}
			started = true
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			started = true
			continue
		}
		if unicode.IsSpace(rune(c)) {
			if started {
				words = append(words, b.String())
				b.Reset()
				started = false
			}
			continue
		}
		b.WriteByte(c)
		started = true
	}
	if quote != 0 {
		return nil, errors.New("unclosed shell quote")
	}
	if started {
		words = append(words, b.String())
	}
	return words, nil
}

func normalizeShellText(command string) string {
	segments, err := SegmentShell(command)
	if err != nil {
		return strings.Join(strings.Fields(command), " ")
	}
	parts := make([]string, 0, len(segments))
	for _, segment := range segments {
		if len(segment.Argv) == 0 {
			continue
		}
		if segment.Operator != "" && len(parts) > 0 {
			parts[len(parts)-1] += " " + segment.Operator
		}
		parts = append(parts, strings.Join(segment.Argv, " "))
	}
	return strings.Join(parts, " ")
}

func isAssignment(token string) bool {
	idx := strings.IndexByte(token, '=')
	return idx > 0 && !strings.HasPrefix(token, "-") && !strings.ContainsAny(token[:idx], "/\\")
}

func withoutAssignments(tokens []string) []string {
	for len(tokens) > 0 && isAssignment(tokens[0]) {
		tokens = tokens[1:]
	}
	return tokens
}

// unwrapTransparent recursively strips transparent command wrappers that
// pass through to a child command without transforming it. After stripping,
// the returned argv starts with the effective binary. Bounded to prevent
// infinite recursion on pathological input.
func unwrapTransparent(argv []string) []string {
	const maxDepth = 8
	for depth := 0; depth < maxDepth && len(argv) > 0; depth++ {
		argv = withoutAssignments(argv)
		if len(argv) == 0 {
			return nil
		}
		base := filepath.Base(argv[0])
		switch base {
		case "env":
			argv = unwrapEnv(argv[1:])
		case "command":
			argv = skipShortOptions(argv[1:], "pVv")
		case "exec":
			argv = unwrapExec(argv[1:])
		case "nice":
			argv = unwrapNice(argv[1:])
		case "nohup":
			argv = unwrapNohup(argv[1:])
		case "builtin":
			argv = unwrapBuiltin(argv[1:])
		default:
			return argv
		}
	}
	return nil
}

// unwrapEnv handles `env [-i] [-u NAME] [NAME=val...] [--] utility ...`.
func unwrapEnv(argv []string) []string {
	for len(argv) > 0 {
		arg := argv[0]
		if arg == "--" {
			return argv[1:]
		}
		if arg == "-S" || arg == "--split-string" {
			return nil // opaque: env -S 'sh -c ...'
		}
		if arg == "-i" || arg == "-0" || arg == "-v" ||
			arg == "--ignore-environment" || arg == "--null" {
			argv = argv[1:]
			continue
		}
		if arg == "-u" || arg == "--unset" {
			if len(argv) > 1 {
				argv = argv[2:]
			} else {
				return nil
			}
			continue
		}
		if strings.HasPrefix(arg, "-u=") || strings.HasPrefix(arg, "--unset=") {
			argv = argv[1:]
			continue
		}
		if strings.HasPrefix(arg, "-") && !isAssignment(arg) {
			return nil // unknown env flag: conservative stop
		}
		if isAssignment(arg) {
			argv = argv[1:]
			continue
		}
		return argv // first non-flag, non-assignment is the utility
	}
	return nil
}

// unwrapNice handles `nice [-n increment] utility ...`.
func unwrapNice(argv []string) []string {
	for len(argv) > 0 {
		arg := argv[0]
		if arg == "-n" || arg == "--adjustment" {
			if len(argv) > 1 {
				argv = argv[2:]
			} else {
				return nil
			}
			continue
		}
		if strings.HasPrefix(arg, "-n") || strings.HasPrefix(arg, "--adjustment=") {
			argv = argv[1:]
			continue
		}
		if arg == "--" {
			return argv[1:]
		}
		if !strings.HasPrefix(arg, "-") {
			return argv
		}
		return nil // unknown option
	}
	return nil
}

func unwrapExec(argv []string) []string {
	for len(argv) > 0 {
		arg := argv[0]
		switch arg {
		case "--":
			return argv[1:]
		case "-a":
			if len(argv) < 2 {
				return nil
			}
			argv = argv[2:]
			continue
		}
		if arg == "-" || !strings.HasPrefix(arg, "-") {
			return argv
		}
		// Shells accept the no-argument exec flags in a short cluster, but
		// -a always takes the following token. Attached/unknown options are
		// opaque rather than guessed at.
		for _, option := range arg[1:] {
			if option != 'c' && option != 'l' {
				return nil
			}
		}
		argv = argv[1:]
	}
	return nil
}

func unwrapNohup(argv []string) []string {
	if len(argv) == 0 {
		return nil
	}
	if argv[0] == "--" {
		return argv[1:]
	}
	if strings.HasPrefix(argv[0], "-") && argv[0] != "-" {
		return nil
	}
	return argv
}

func unwrapBuiltin(argv []string) []string {
	if len(argv) == 0 || (strings.HasPrefix(argv[0], "-") && argv[0] != "-") {
		return nil
	}
	return argv
}

// skipShortOptions skips single-char flags from the allowed set.
func skipShortOptions(argv []string, allowed string) []string {
	for len(argv) > 0 {
		arg := argv[0]
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			return argv
		}
		if arg == "--" {
			return argv[1:]
		}
		valid := true
		for _, c := range arg[1:] {
			if !strings.ContainsRune(allowed, c) {
				valid = false
				break
			}
		}
		if !valid {
			return nil
		}
		argv = argv[1:]
	}
	return nil
}

func classifyCommand(segments []CommandSegment, policy *sandbox.Policy) (commandDisposition, string, bool) {
	best := dispositionRoutine
	reasons := []string{"routine workspace operation"}
	network := false
	for i, segment := range segments {
		argv := segment.Argv
		if len(argv) == 0 {
			continue
		}
		argv = unwrapTransparent(argv)
		if len(argv) == 0 {
			// Opaque wrapper form (e.g. env -S); classify conservatively.
			best, reasons = strongerDisposition(best, reasons, dispositionHardDeny, "opaque wrapper form cannot be classified")
			continue
		}
		base := filepath.Base(argv[0])
		var disposition commandDisposition
		var reason string
		var segmentNetwork bool
		switch base {
		case "sudo", "su", "doas", "security", "dscl", "passwd", "launchctl", "systemctl", "service", "shutdown", "reboot", "kill", "pkill":
			disposition, reason = dispositionHardDeny, "privilege or service control is never delegated"
		case "chmod", "chown":
			disposition, reason = dispositionHardDeny, "permission/ownership changes are never delegated"
		case "xargs":
			disposition, reason = dispositionHardDeny, "xargs synthesizes commands from input and cannot be classified"
		case "rm":
			disposition, reason = classifyRemoval(argv[1:], policy)
		case "sh", "bash", "zsh", "fish":
			if shellReceivesCode(segments, i, argv) {
				disposition, reason = dispositionHardDeny, "piped or redirected shell code is opaque"
			}
		case "git":
			if len(argv) > 1 {
				switch argv[1] {
				case "add", "commit", "reset", "clean":
					disposition, reason = dispositionHuman, "Git metadata or remote state requires a human approval"
				case "push":
					disposition, reason, segmentNetwork = dispositionHuman, "Git metadata and remote network access require a human approval", true
				case "clone", "fetch", "pull":
					disposition, reason, segmentNetwork = dispositionReview, "Git acquisition or remote access requires network approval", true
				}
			}
		case "curl", "wget":
			disposition, reason, segmentNetwork = dispositionReview, "explicit network access requires approval", true
		case "npm", "pnpm", "yarn", "pip", "pip3", "cargo", "go", "brew", "apt", "apt-get":
			if packageAcquisition(base, argv[1:]) {
				if globalInstall(base, argv[1:]) {
					disposition, reason = dispositionHuman, "global installation remains human-only"
				} else {
					disposition, reason, segmentNetwork = dispositionReview, "dependency acquisition requires approval", true
				}
			}
		}
		network = network || segmentNetwork
		best, reasons = strongerDisposition(best, reasons, disposition, reason)
	}
	return best, boundedReasons(reasons), network
}

func dispositionRank(disposition commandDisposition) int {
	switch disposition {
	case dispositionHardDeny:
		return 3
	case dispositionHuman:
		return 2
	case dispositionReview:
		return 1
	default:
		return 0
	}
}

func strongerDisposition(best commandDisposition, reasons []string, candidate commandDisposition, reason string) (commandDisposition, []string) {
	if candidate == "" {
		return best, reasons
	}
	rank := dispositionRank(candidate)
	bestRank := dispositionRank(best)
	if rank > bestRank {
		return candidate, []string{reason}
	}
	if rank < bestRank || candidate == dispositionRoutine || reason == "" {
		return best, reasons
	}
	for _, existing := range reasons {
		if existing == reason {
			return best, reasons
		}
	}
	if len(reasons) < 3 {
		reasons = append(reasons, reason)
	}
	return best, reasons
}

func boundedReasons(reasons []string) string {
	if len(reasons) == 0 {
		return "routine workspace operation"
	}
	const maxReasonBytes = 512
	text := strings.Join(reasons, "; ")
	if len(text) <= maxReasonBytes {
		return text
	}
	return text[:maxReasonBytes-1] + "…"
}

func shellReceivesCode(segments []CommandSegment, index int, argv []string) bool {
	if shellCommandMode(argv[1:]) {
		return true
	}
	if codeOperator(segments[index].Operator) {
		return true
	}
	return index+1 < len(segments) && codeOperator(segments[index+1].Operator)
}

func shellCommandMode(argv []string) bool {
	for _, arg := range argv {
		if arg == "--" {
			return false
		}
		if arg == "--command" {
			return true
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			return false
		}
		for _, option := range arg[1:] {
			if option == 'c' {
				return true
			}
		}
	}
	return false
}

func codeOperator(operator string) bool {
	switch operator {
	case "|", "|&", "<", "<<":
		return true
	default:
		return false
	}
}

// requestedCommandRoots extracts only explicit shell redirection targets.
// General arbitrary process writes remain contained by the OS backend; a
// known redirection can be presented to the user as a typed one-shot root
// grant before the process is started.
func requestedCommandRoots(segments []CommandSegment, policy *sandbox.Policy) (readRoots, writeRoots []string, err error) {
	if policy == nil {
		return nil, nil, nil
	}
	if gitRoot, ok, gitErr := gitMetadataRoot(segments, policy); gitErr != nil {
		return nil, nil, gitErr
	} else if ok {
		writeRoots = appendUniqueString(writeRoots, gitRoot)
	}
	for _, segment := range segments {
		roots, rootsErr := requestedRemovalRoots(segment, policy)
		if rootsErr != nil {
			return nil, nil, rootsErr
		}
		writeRoots = appendUniqueString(writeRoots, roots...)
	}
	for _, segment := range segments {
		access := sandbox.Access("")
		switch segment.Operator {
		case ">", ">>", "&>", "&>>":
			access = sandbox.AccessWrite
		case "<":
			access = sandbox.AccessRead
		case "<<":
			return nil, nil, errors.New("here-doc syntax cannot be safely authorized")
		default:
			continue
		}
		if len(segment.Argv) != 1 {
			return nil, nil, errors.New("shell redirection target cannot be safely authorized")
		}
		if isDeviceSink(segment.Argv[0]) {
			continue
		}
		canonical, canonicalErr := sandbox.CanonicalPath(segment.Argv[0], true)
		if canonicalErr != nil {
			return nil, nil, fmt.Errorf("redirection target: %w", canonicalErr)
		}
		if isDeviceSink(canonical) {
			continue
		}
		allowMissing := access == sandbox.AccessWrite
		if _, authorizeErr := policy.Authorize(canonical, access, allowMissing); authorizeErr == nil {
			continue
		}
		if access == sandbox.AccessWrite {
			if protected, protectedOK, protectedErr := policy.ProtectedRootFor(canonical, true); protectedErr != nil {
				return nil, nil, protectedErr
			} else if protectedOK {
				writeRoots = appendUniqueString(writeRoots, protected)
				continue
			}
		}
		root, rootErr := capabilityRoot(canonical)
		if rootErr != nil {
			return nil, nil, rootErr
		}
		if access == sandbox.AccessRead {
			readRoots = appendUniqueString(readRoots, root)
		} else {
			writeRoots = appendUniqueString(writeRoots, root)
		}
	}
	return readRoots, writeRoots, nil
}

func requestedRemovalRoots(segment CommandSegment, policy *sandbox.Policy) ([]string, error) {
	argv := unwrapTransparent(segment.Argv)
	if len(argv) == 0 {
		return nil, nil
	}
	if len(argv) < 2 || filepath.Base(argv[0]) != "rm" {
		return nil, nil
	}
	recursive, operands := removalArgs(argv[1:])
	roots := make([]string, 0, len(operands))
	for _, operand := range operands {
		disposition, _ := classifyRemovalTarget(operand, recursive, policy.Workspace(), policy)
		if disposition != dispositionHuman {
			continue
		}
		pattern := expandRemovalHome(operand)
		var target string
		var err error
		if strings.ContainsAny(pattern, "*?[]{}") {
			target, err = removalGlobBase(pattern, policy.Workspace())
		} else {
			target, _, err = removalPath(pattern, policy.Workspace())
		}
		if err != nil {
			return nil, fmt.Errorf("removal target: %w", err)
		}
		if _, authorizeErr := policy.Authorize(target, sandbox.AccessWrite, true); authorizeErr == nil {
			continue
		}
		if protected, protectedOK, protectedErr := policy.ProtectedRootFor(target, true); protectedErr != nil {
			return nil, protectedErr
		} else if protectedOK {
			roots = appendUniqueString(roots, protected)
			continue
		}
		root, rootErr := capabilityRoot(target)
		if rootErr != nil {
			return nil, rootErr
		}
		roots = appendUniqueString(roots, root)
	}
	return roots, nil
}

func gitMetadataRoot(segments []CommandSegment, policy *sandbox.Policy) (string, bool, error) {
	for _, segment := range segments {
		argv := unwrapTransparent(segment.Argv)
		if len(argv) < 2 || filepath.Base(argv[0]) != "git" {
			continue
		}
		switch argv[1] {
		case "add", "commit", "reset", "clean", "push":
			root := filepath.Join(policy.Workspace(), ".git")
			protected, ok, err := policy.ProtectedRootFor(root, true)
			if err != nil {
				return "", false, err
			}
			return protected, ok, nil
		}
	}
	return "", false, nil
}

func capabilityRoot(canonical string) (string, error) {
	info, err := os.Stat(canonical)
	if err == nil && info.IsDir() {
		return canonical, nil
	}
	root := filepath.Dir(canonical)
	if root == canonical {
		return "", errors.New("redirection target resolves to the filesystem root")
	}
	resolved, err := sandbox.CanonicalPath(root, true)
	if err != nil {
		return "", fmt.Errorf("redirection parent: %w", err)
	}
	if filepath.Dir(resolved) == resolved {
		return "", errors.New("redirection target resolves to the filesystem root")
	}
	return resolved, nil
}

func classifyRemoval(argv []string, policy *sandbox.Policy) (commandDisposition, string) {
	recursive, operands := removalArgs(argv)
	if len(operands) == 0 {
		return dispositionRoutine, "rm has no removal targets"
	}
	workspace := ""
	if policy != nil {
		workspace = policy.Workspace()
	}
	if workspace == "" {
		workspace, _ = filepath.Abs(".")
	}
	best := dispositionRoutine
	reasons := []string{"explicit leaf removal is within a writable root"}
	for _, operand := range operands {
		disposition, reason := classifyRemovalTarget(operand, recursive, workspace, policy)
		best, reasons = strongerDisposition(best, reasons, disposition, reason)
	}
	return best, boundedReasons(reasons)
}

func removalArgs(argv []string) (recursive bool, operands []string) {
	options := true
	for _, arg := range argv {
		if options && arg == "--" {
			options = false
			continue
		}
		if options && strings.HasPrefix(arg, "-") && arg != "-" {
			if strings.HasPrefix(arg, "--") {
				if arg == "--recursive" {
					recursive = true
				}
				continue
			}
			for _, flag := range arg[1:] {
				if flag == 'r' || flag == 'R' {
					recursive = true
				}
			}
			continue
		}
		operands = append(operands, arg)
	}
	return recursive, operands
}

func classifyRemovalTarget(operand string, recursive bool, workspace string, policy *sandbox.Policy) (commandDisposition, string) {
	if strings.ContainsAny(operand, "$`") || hasParentTraversal(operand) {
		return dispositionHardDeny, "dynamic or parent-traversing removal target is unsafe"
	}
	pattern := expandRemovalHome(operand)
	hasGlob := strings.ContainsAny(pattern, "*?[]{}")
	canonical, raw, err := removalPath(pattern, workspace)
	if err != nil {
		return dispositionHardDeny, "removal target could not be resolved safely"
	}
	if removalSymlinkUnsafe(raw, workspace) {
		return dispositionHardDeny, "symlinked removal target is unsafe to classify"
	}
	if hasGlob {
		globBase, globErr := removalGlobBase(pattern, workspace)
		if globErr != nil {
			return dispositionHardDeny, "removal glob could not be resolved safely"
		}
		if removalCoversWorkspace(globBase, workspace) && (globBase != workspace || removalGlobCoversContents(pattern, recursive)) {
			return dispositionHardDeny, "removal glob covers the workspace"
		}
		if policy == nil || !removalWithinWritableRoot(globBase, policy) {
			return dispositionHuman, "globbed removal needs a human-approved root"
		}
		return dispositionHuman, "globbed removal of a bounded subtree requires a human"
	}
	if removalCoversRoot(canonical, workspace) {
		return dispositionHardDeny, "removal target is the filesystem, home, workspace, or its parent"
	}
	if info, statErr := os.Lstat(raw); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return dispositionHardDeny, "symlink removal target is unsafe to classify"
	}
	if policy == nil {
		if recursive {
			return dispositionHuman, "recursive removal requires a human"
		}
		return dispositionHuman, "removal root is not available to the execution policy"
	}
	if _, authErr := policy.Authorize(canonical, sandbox.AccessWrite, true); authErr != nil {
		return dispositionHuman, "removal target is outside the configured writable roots"
	}
	if recursive {
		return dispositionHuman, "recursive removal of a bounded subtree requires a human"
	}
	if info, statErr := os.Stat(raw); statErr == nil && info.IsDir() {
		return dispositionHuman, "directory removal requires a human"
	}
	return dispositionRoutine, "explicit leaf removal is within a writable root"
}

func hasParentTraversal(operand string) bool {
	for _, part := range strings.Split(filepath.ToSlash(operand), "/") {
		if part == ".." {
			return true
		}
	}
	return false
}

func removalSymlinkUnsafe(raw, workspace string) bool {
	clean := filepath.Clean(raw)
	if !filepath.IsAbs(clean) {
		return true
	}
	root := filepath.VolumeName(clean) + string(filepath.Separator)
	if filepath.VolumeName(clean) == "" {
		root = string(filepath.Separator)
	}
	rest := strings.TrimPrefix(clean, root)
	current := root
	for _, part := range strings.Split(rest, string(filepath.Separator)) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			break
		}
		if info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		resolved, err := filepath.EvalSymlinks(current)
		if err != nil {
			return true
		}
		resolved = filepath.Clean(resolved)
		if resolved != workspace && !pathContains(resolved, workspace) {
			return true
		}
	}
	return false
}

func expandRemovalHome(operand string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return operand
	}
	switch {
	case operand == "~":
		return home
	case strings.HasPrefix(operand, "~/"):
		return filepath.Join(home, operand[2:])
	case operand == "$HOME", operand == "${HOME}":
		return home
	default:
		return operand
	}
}

func removalPath(operand, workspace string) (canonical, raw string, err error) {
	raw = operand
	if !filepath.IsAbs(raw) {
		raw = filepath.Join(workspace, raw)
	}
	raw, err = filepath.Abs(raw)
	if err != nil {
		return "", "", err
	}
	canonical, err = sandbox.CanonicalPath(raw, true)
	return canonical, raw, err
}

func removalGlobBase(pattern, workspace string) (string, error) {
	first := strings.IndexAny(pattern, "*?[]{}")
	if first < 0 {
		canonical, _, err := removalPath(pattern, workspace)
		return canonical, err
	}
	prefix := pattern[:first]
	if prefix == "" {
		prefix = "."
	} else if !strings.HasSuffix(prefix, string(filepath.Separator)) && !strings.HasSuffix(prefix, "/") {
		prefix = filepath.Dir(prefix)
	} else {
		prefix = strings.TrimRight(prefix, "/\\")
		if prefix == "" {
			prefix = string(filepath.Separator)
		}
	}
	canonical, _, err := removalPath(prefix, workspace)
	return canonical, err
}

func removalCoversWorkspace(path, workspace string) bool {
	return path == workspace || pathContains(path, workspace)
}

func removalGlobCoversContents(pattern string, recursive bool) bool {
	if !recursive {
		return false
	}
	clean := filepath.ToSlash(filepath.Clean(pattern))
	return clean == "*" || clean == "**" || strings.HasSuffix(clean, "/*") || strings.HasSuffix(clean, "/**") || strings.HasSuffix(clean, "/.*")
}

func removalCoversRoot(path, workspace string) bool {
	if path == string(filepath.Separator) || path == workspace || pathContains(path, workspace) {
		return true
	}
	if home, err := os.UserHomeDir(); err == nil && (path == home || pathContains(path, home)) {
		return true
	}
	return false
}

func pathContains(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func removalWithinWritableRoot(path string, policy *sandbox.Policy) bool {
	if policy == nil {
		return false
	}
	for _, root := range append(policy.WriteRoots(), policy.TempRoots()...) {
		if path == root || pathContains(root, path) || pathContains(path, root) {
			return true
		}
	}
	return false
}

func packageAcquisition(base string, argv []string) bool {
	if base == "go" {
		return len(argv) > 0 && (argv[0] == "get" || argv[0] == "install" || argv[0] == "mod" && len(argv) > 1 && argv[1] == "download")
	}
	if base == "brew" || base == "apt" || base == "apt-get" {
		return len(argv) > 0 && (argv[0] == "install" || argv[0] == "upgrade" || argv[0] == "update")
	}
	return len(argv) > 0 && (argv[0] == "install" || argv[0] == "add" || argv[0] == "update" || argv[0] == "upgrade")
}

func globalInstall(base string, argv []string) bool {
	for _, arg := range argv {
		if arg == "-g" || arg == "--global" || arg == "--system" {
			return true
		}
	}
	return base == "apt" || base == "apt-get" || base == "brew"
}

// checkGate runs the installed gate for a tool call; "" means proceed, a
// non-empty string is the rejection fed back to the model.
func checkGate(tool, command string) string {
	if Gate == nil {
		return ""
	}
	decision, redirect := Gate(GateRequest{Tool: tool, Command: command, Rule: CommandRule(command)})
	if decision == GateReject {
		if redirect == "" {
			redirect = "the user rejected this action"
		}
		return "Permission denied: " + redirect
	}
	return ""
}

func isDeviceSink(path string) bool {
	switch filepath.Clean(path) {
	case "/dev/null", "/dev/zero", "/dev/tty", "/dev/stdout", "/dev/stderr", "/dev/stdin":
		return true
	default:
		return false
	}
}
