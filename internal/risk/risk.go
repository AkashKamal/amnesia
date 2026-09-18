// Package risk classifies a shell command before Amnesia offers to run it.
//
// Amnesia turns natural language into executable shell commands, which means a
// bad corpus row, a hallucinating model, or text injected into the memory bank
// by a third party can all end up one keypress from running. Classification is
// deliberately conservative: it is a speed bump on the confirm prompt, never a
// sandbox. Anything that gets past it still requires the user to say yes.
package risk

import (
	"regexp"
	"strings"
)

type Level uint8

const (
	Safe        Level = iota // read-only or trivially reversible
	Caution                  // writes, restarts, network mutation
	Destructive              // irreversible data or infrastructure loss
)

func (l Level) String() string {
	switch l {
	case Destructive:
		return "destructive"
	case Caution:
		return "caution"
	default:
		return "safe"
	}
}

type rule struct {
	re     *regexp.Regexp
	level  Level
	reason string
}

// Patterns are matched against the whole command line. Keep this list short and
// obvious; a long list of clever regexes is a maintenance burden that still
// misses the interesting cases. Real defence is the confirm prompt.
var rules = []rule{
	{regexp.MustCompile(`\brm\s+(-\w*\s+)*-\w*[rR]\w*f|\brm\s+-\w*f\w*[rR]`), Destructive, "recursive force delete"},
	{regexp.MustCompile(`\b(mkfs|fdisk|parted)\b`), Destructive, "writes a filesystem or partition table"},
	{regexp.MustCompile(`\bdd\b.*\bof=/dev/`), Destructive, "raw write to a block device"},
	{regexp.MustCompile(`>\s*/dev/(sd|nvme|hd)`), Destructive, "raw write to a block device"},
	{regexp.MustCompile(`\bgit\s+reset\s+--hard\b|\bgit\s+clean\s+-\w*[dfx]`), Destructive, "discards uncommitted work"},
	{regexp.MustCompile(`\bgit\s+push\b.*(--force\b|-f\b)`), Destructive, "rewrites remote history"},
	{regexp.MustCompile(`\b(docker|podman)\s+system\s+prune\b`), Destructive, "deletes unused images and volumes"},
	{regexp.MustCompile(`\bkubectl\s+delete\b`), Destructive, "deletes cluster resources"},
	{regexp.MustCompile(`\bdrop\s+(database|table)\b`), Destructive, "drops a database object"},
	{regexp.MustCompile(`\bcurl\b[^|]*\|\s*(sudo\s+)?(ba)?sh`), Destructive, "pipes a remote script straight into a shell"},
	{regexp.MustCompile(`:\(\)\s*\{.*\}\s*;?\s*:`), Destructive, "fork bomb"},
	{regexp.MustCompile(`\bchmod\s+-R\s+777\b`), Destructive, "removes all permission boundaries"},

	// Windows. amnesia ships for Windows and the corpus carries Windows rows,
	// so a POSIX-only classifier is half a classifier. A stress run caught
	// "net stop <service_name>" coming back labelled safe.
	{regexp.MustCompile(`(?i)\bdiskpart\b`), Destructive, "repartitions a disk"},
	{regexp.MustCompile(`(?i)\bformat\s+[a-z]:`), Destructive, "formats a drive"},
	{regexp.MustCompile(`(?i)\b(rd|rmdir)\s+/s`), Destructive, "recursively removes a directory"},
	{regexp.MustCompile(`(?i)\bdel\s+(/\w+\s+)*/[qsf]`), Destructive, "force deletes files"},
	{regexp.MustCompile(`(?i)Remove-Item\b[^|]*-Recurse[^|]*-Force`), Destructive, "recursively force deletes"},
	{regexp.MustCompile(`(?i)\bcipher\s+/w`), Destructive, "wipes free space"},
	{regexp.MustCompile(`(?i)\breg\s+delete\b`), Destructive, "deletes registry keys"},

	{regexp.MustCompile(`^\s*sudo\b`), Caution, "runs as root"},
	{regexp.MustCompile(`\brm\b`), Caution, "deletes files"},
	{regexp.MustCompile(`\b(systemctl|service)\s+(stop|restart|disable)\b`), Caution, "stops or restarts a service"},
	{regexp.MustCompile(`(?i)\b(net|sc)\s+(stop|start|delete|config)\b`), Caution, "changes a Windows service"},
	{regexp.MustCompile(`(?i)\btaskkill\b`), Caution, "terminates processes"},
	{regexp.MustCompile(`(?i)\bStop-(Process|Service|Computer)\b`), Caution, "stops a process, service or machine"},
	{regexp.MustCompile(`(?i)\b(shutdown|reboot)\b`), Caution, "shuts down or reboots"},
	{regexp.MustCompile(`\bkubectl\s+(apply|rollout|scale|drain|cordon)\b`), Caution, "mutates cluster state"},
	{regexp.MustCompile(`\b(kill|pkill|killall|fuser\s+-k)\b`), Caution, "terminates processes"},
	{regexp.MustCompile(`\b(iptables|ufw|firewall-cmd)\b`), Caution, "changes firewall rules"},
	{regexp.MustCompile(`\bgit\s+(push|rebase|checkout|restore)\b`), Caution, "changes repository state"},
	{regexp.MustCompile(`>\s*\S`), Caution, "redirects output over a file"},
}

// placeholder matches <container>, <path-to-file> and friends. They are
// template markers, not shell syntax, and leaving them in makes the ">" of
// "docker exec -it <container> sh" look like an output redirect.
var placeholder = regexp.MustCompile(`<[a-zA-Z_][a-zA-Z0-9_.-]*>`)

// Classify returns the highest level any rule matches, and why.
func Classify(cmd string) (Level, string) {
	c := strings.TrimSpace(placeholder.ReplaceAllString(cmd, "ARG"))
	level, reason := Safe, ""
	for _, r := range rules {
		if r.level > level && r.re.MatchString(c) {
			level, reason = r.level, r.reason
			if level == Destructive {
				return level, reason
			}
		}
	}
	return level, reason
}
