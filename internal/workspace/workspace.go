// Package workspace loads and validates workspace data repos: workflow
// definitions, agent specs, watchers, skills, policies.
package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kidkuddy/krakoa/internal/core"
)

type Meta struct {
	Name        string        `yaml:"name"`
	Description string        `yaml:"description,omitempty"`
	Doctor      []DoctorCheck `yaml:"doctor,omitempty"`
}

// DoctorCheck is a workspace-declared prerequisite probe. The engine knows
// nothing about the tools it invokes — environment specifics live in the
// workspace, per the repo-separation rule.
type DoctorCheck struct {
	Name    string   `yaml:"name"`
	Command string   `yaml:"command,omitempty"` // run via sh -c; non-zero exit = fail
	URL     string   `yaml:"url,omitempty"`     // GET; non-2xx = fail
	Expect  string   `yaml:"expect,omitempty"`  // substring that must appear in the output
	Fail    []string `yaml:"fail,omitempty"`    // substrings that fail even on exit 0 (e.g. CLIs that exit 0 on bad tokens)
	Fix     string   `yaml:"fix,omitempty"`
}

// Gatekeeper restricts which agents may bind to states of a class.
type Gatekeeper struct {
	Class  string   `yaml:"class"`
	Agents []string `yaml:"agents"`
}

type Policies struct {
	Gatekeepers []Gatekeeper `yaml:"gatekeepers,omitempty"`
}

type Workspace struct {
	Name        string
	Description string
	Path        string
	GitVersion  string
	Doctor      []DoctorCheck

	Workflows map[string]*core.WorkflowDefinition
	Agents    map[string]*core.AgentSpec
	Watchers  map[string]*core.WatcherSpec
	Skills    map[string]string // skill name -> dir containing SKILL.md
	Policies  Policies
}

// Load reads and fully validates one workspace directory. A workspace with
// errors is unusable (the daemon refuses it but keeps serving others).
func Load(path string) (*Workspace, []error) {
	var errs []error
	fail := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	ws := &Workspace{
		Path:      path,
		Workflows: map[string]*core.WorkflowDefinition{},
		Agents:    map[string]*core.AgentSpec{},
		Watchers:  map[string]*core.WatcherSpec{},
		Skills:    map[string]string{},
	}

	var meta Meta
	if err := readYAML(filepath.Join(path, "workspace.yaml"), &meta); err != nil {
		return nil, []error{fmt.Errorf("workspace.yaml: %w", err)}
	}
	if meta.Name == "" {
		return nil, []error{fmt.Errorf("workspace.yaml: name is required")}
	}
	ws.Name = meta.Name
	ws.Description = meta.Description
	ws.GitVersion = gitVersion(path)
	ws.Doctor = meta.Doctor
	for i, dc := range meta.Doctor {
		if dc.Name == "" {
			fail("workspace.yaml: doctor check %d: name is required", i)
		}
		if (dc.Command == "") == (dc.URL == "") {
			fail("workspace.yaml: doctor check %q: exactly one of command/url required", dc.Name)
		}
	}

	if err := readYAML(filepath.Join(path, "policies.yaml"), &ws.Policies); err != nil && !os.IsNotExist(err) {
		fail("policies.yaml: %v", err)
	}

	// Skills: skills/<name>/SKILL.md
	for _, dir := range glob(filepath.Join(path, "skills", "*")) {
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, "SKILL.md")); err != nil {
			fail("skill %s: missing SKILL.md", filepath.Base(dir))
			continue
		}
		ws.Skills[filepath.Base(dir)] = dir
	}

	for _, f := range glob(filepath.Join(path, "agents", "*.yaml")) {
		var spec core.AgentSpec
		if err := readYAML(f, &spec); err != nil {
			fail("%s: %v", rel(path, f), err)
			continue
		}
		if spec.Name == "" {
			spec.Name = base(f)
		}
		if _, dup := ws.Agents[spec.Name]; dup {
			fail("agent %s: duplicate name", spec.Name)
		}
		ws.Agents[spec.Name] = &spec
		for _, sk := range spec.Skills {
			if _, ok := ws.Skills[sk]; !ok {
				fail("agent %s: unknown skill %q", spec.Name, sk)
			}
		}
	}

	for _, f := range glob(filepath.Join(path, "watchers", "*.yaml")) {
		var w core.WatcherSpec
		if err := readYAML(f, &w); err != nil {
			fail("%s: %v", rel(path, f), err)
			continue
		}
		if w.Name == "" {
			w.Name = base(f)
		}
		if _, dup := ws.Watchers[w.Name]; dup {
			fail("watcher %s: duplicate name", w.Name)
		}
		if w.Mode != "spawn" && w.Mode != "resume" {
			fail("watcher %s: mode must be spawn or resume, got %q", w.Name, w.Mode)
		}
		if w.Every <= 0 {
			fail("watcher %s: every is required", w.Name)
		}
		switch {
		case (w.Agent == "") == (w.Command == ""):
			fail("watcher %s: exactly one of agent/command required", w.Name)
		case w.Agent != "":
			if _, ok := ws.Agents[w.Agent]; !ok {
				fail("watcher %s: unknown agent %q", w.Name, w.Agent)
			}
		default:
			if err := checkScript(path, w.Command); err != nil {
				fail("watcher %s: %v", w.Name, err)
			}
		}
		ws.Watchers[w.Name] = &w
	}

	for _, f := range glob(filepath.Join(path, "workflows", "*.yaml")) {
		raw, err := os.ReadFile(f)
		if err != nil {
			fail("%s: %v", rel(path, f), err)
			continue
		}
		var def core.WorkflowDefinition
		if err := yaml.Unmarshal(raw, &def); err != nil {
			fail("%s: %v", rel(path, f), err)
			continue
		}
		if def.Name == "" {
			def.Name = base(f)
		}
		sum := sha256.Sum256(raw)
		def.Hash = hex.EncodeToString(sum[:8])
		def.Workspace = ws.Name
		if _, dup := ws.Workflows[def.Name]; dup {
			fail("workflow %s: duplicate name", def.Name)
		}
		for _, err := range core.Validate(&def) {
			fail("workflow %s: %v", def.Name, err)
		}
		errs = append(errs, ws.checkRefs(&def)...)
		ws.Workflows[def.Name] = &def
	}

	// spawn-mode watchers must point at a loaded workflow
	for _, w := range ws.Watchers {
		if w.Mode == "spawn" {
			if _, ok := ws.Workflows[w.Workflow]; !ok {
				fail("watcher %s: unknown workflow %q", w.Name, w.Workflow)
			}
		}
	}

	if len(errs) > 0 {
		return ws, errs
	}
	return ws, nil
}

// checkRefs validates a definition's references against the workspace:
// agents exist, trigger watchers exist, gatekeeper policies hold.
func (ws *Workspace) checkRefs(def *core.WorkflowDefinition) []error {
	var errs []error
	fail := func(format string, a ...any) {
		errs = append(errs, fmt.Errorf("workflow %s: "+format, append([]any{def.Name}, a...)...))
	}

	if def.Trigger.Kind == core.TriggerWatcher {
		if _, ok := ws.Watchers[def.Trigger.Watcher]; !ok {
			fail("unknown trigger watcher %q", def.Trigger.Watcher)
		}
	}

	names := make([]string, 0, len(def.States))
	for n := range def.States {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		st := def.States[name]
		if st.Step == core.StepAgent {
			if _, ok := ws.Agents[st.Agent]; !ok {
				fail("state %s: unknown agent %q", name, st.Agent)
			}
			for _, gk := range ws.Policies.Gatekeepers {
				if st.Class == gk.Class && !contains(gk.Agents, st.Agent) {
					fail("state %s: class %q only allows agents %v, got %q", name, gk.Class, gk.Agents, st.Agent)
				}
			}
		}
		for _, arm := range st.Arms {
			if arm.Probe == nil {
				continue
			}
			if arm.Probe.Agent != "" {
				if _, ok := ws.Agents[arm.Probe.Agent]; !ok {
					fail("state %s: unknown probe agent %q", name, arm.Probe.Agent)
				}
			} else if err := checkScript(ws.Path, arm.Probe.Command); err != nil {
				fail("state %s: %v", name, err)
			}
		}
	}
	return errs
}

func readYAML(path string, out any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		return err
	}
	return nil
}

func glob(pattern string) []string {
	m, _ := filepath.Glob(pattern)
	sort.Strings(m)
	return m
}

func base(f string) string { return strings.TrimSuffix(filepath.Base(f), filepath.Ext(f)) }

func rel(root, f string) string {
	r, err := filepath.Rel(root, f)
	if err != nil {
		return f
	}
	return r
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// checkScript validates a command probe reference: its first token must be
// an existing, executable, workspace-relative file ($-templates in later
// args are fine — they resolve at fire time).
func checkScript(wsPath, command string) error {
	tok := strings.Fields(command)
	if len(tok) == 0 {
		return fmt.Errorf("empty command")
	}
	p := filepath.Join(wsPath, tok[0])
	fi, err := os.Stat(p)
	if err != nil {
		return fmt.Errorf("command script %q not found in workspace", tok[0])
	}
	if fi.IsDir() || fi.Mode()&0o111 == 0 {
		return fmt.Errorf("command script %q is not executable", tok[0])
	}
	return nil
}

func gitVersion(path string) string {
	out, err := exec.Command("git", "-C", path, "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
