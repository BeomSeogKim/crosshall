//go:build e2e

package e2e

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The README's shell examples are executed by TestReadmeCommandsRunAsShown.
//
//   - A console block runs when the comment <!-- run --> (or <!-- run: name -->
//     for the terminal it starts in) is the line before it. Lines that start
//     with "$ " are commands and the lines after a command, up to the next
//     one, are its expected output.
//   - <!-- setup --> marks a console block of commands only: they run, and
//     the page shows no output for them.
//   - <!-- script: name --> marks an sh block that a person pastes as it
//     stands: the whole block runs as one command, without an output check.
//   - A line that starts with "> " continues the command above it.
//   - A comment line "# name" inside a block switches to another terminal.
//   - ", fresh" in a marker starts a new installation in a new home.
//   - Every console block needs a marker. Every command must exit 0, except
//     the last command of a block whose marker ends ", exit N".
//   - "crosshall run -- sh" is run as shown and stays open as that terminal's
//     member shell; the terminal's later commands are typed into it.
type readmeCommand struct {
	terminal, command string
	want              []string
	line              int // index of the command's last line in the README
	checked, fresh    bool
	exit              int // the exit status the page expects
}

var (
	readmeRunMarker = regexp.MustCompile(`^<!-- (run|setup|script)(?:: ([a-z]+))?(, fresh)?(?:, exit ([0-9]+))? -->$`)
	readmeTerminal  = regexp.MustCompile(`^# ([a-z]+)$`)
	// Fences as CommonMark reads them: up to three spaces of indentation,
	// three or more backticks or tildes, and an info string whose first word
	// is the language.
	readmeFenceOpen  = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})(.*)$")
	readmeFenceClose = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})[ \t]*$")
)

// readmeConsoleFence reports whether a line opens a console code block, and
// returns its fence.
func readmeConsoleFence(line string) (string, bool) {
	return readmeFenceOf(line, "console")
}

// readmeFenceOf reports whether a line opens a code block in one of the
// languages, and returns its fence.
func readmeFenceOf(line string, languages ...string) (string, bool) {
	m := readmeFenceOpen.FindStringSubmatch(line)
	if m == nil || (m[1][0] == '`' && strings.Contains(m[2], "`")) {
		return "", false
	}
	info := strings.Fields(m[2])
	for _, language := range languages {
		if len(info) > 0 && strings.EqualFold(info[0], language) {
			return m[1], true
		}
	}
	return m[1], false
}

// readmeClosesFence reports whether a line closes a block opened by fence.
func readmeClosesFence(line, fence string) bool {
	m := readmeFenceClose.FindStringSubmatch(line)
	return m != nil && m[1][0] == fence[0] && len(m[1]) >= len(fence)
}

func readmeCommands(t *testing.T, lines []string) []readmeCommand {
	t.Helper()
	// Every console block on the page is run: an unmarked one fails.
	for i, line := range lines {
		if _, console := readmeConsoleFence(line); console && (i == 0 || !readmeRunMarker.MatchString(lines[i-1])) {
			t.Fatalf("README line %d: a console block without a run or setup marker is never checked", i+1)
		}
	}
	var out []readmeCommand
	for i := 0; i < len(lines); i++ {
		m := readmeRunMarker.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		var fence string
		var console bool
		if i+1 < len(lines) {
			fence, console = readmeConsoleFence(lines[i+1])
		}
		if m[1] == "script" {
			// A script block is pasted as it stands: the whole block is one
			// command, and the page shows no output for it.
			if i+1 < len(lines) {
				fence, console = readmeFenceOf(lines[i+1], "sh", "bash")
			}
			if !console {
				t.Fatalf("README line %d: a script marker must precede an sh block", i+1)
			}
			var body []string
			j := i + 2
			for ; j < len(lines) && !readmeClosesFence(lines[j], fence); j++ {
				body = append(body, lines[j])
			}
			if j >= len(lines) || m[2] == "" {
				t.Fatalf("README line %d: a script block needs a terminal and a closing fence", i+1)
			}
			exit := 0
			if m[4] != "" {
				exit, _ = strconv.Atoi(m[4])
			}
			out = append(out, readmeCommand{terminal: m[2], command: strings.Join(body, "\n"), line: j - 1, fresh: m[3] != "", exit: exit})
			i = j
			continue
		}
		if !console {
			t.Fatalf("README line %d: a marker must precede a console block", i+1)
		}
		terminal, fresh, checked := m[2], m[3] != "", m[1] == "run"
		exit := 0
		if m[4] != "" {
			exit, _ = strconv.Atoi(m[4])
		}
		i++
		first := len(out)
		for j := i + 1; ; j++ {
			if j >= len(lines) {
				t.Fatalf("README line %d: unterminated block", i+1)
			}
			if readmeClosesFence(lines[j], fence) {
				if len(out) == first {
					t.Fatalf("README line %d: a marked block with no command", i+1)
				}
				out[len(out)-1].exit = exit
				i = j
				break
			}
			if m := readmeTerminal.FindStringSubmatch(lines[j]); m != nil {
				terminal = m[1]
				continue
			}
			// "> " continues the command above it, as a shell prompts for it.
			if rest, ok := strings.CutPrefix(lines[j], "> "); ok && len(out) > first && len(out[len(out)-1].want) == 0 {
				out[len(out)-1].command += "\n" + rest
				out[len(out)-1].line = j
				continue
			}
			if cmd, ok := strings.CutPrefix(lines[j], "$ "); ok {
				if terminal == "" {
					t.Fatalf("README line %d: command with no terminal", j+1)
				}
				out = append(out, readmeCommand{terminal: terminal, command: cmd, line: j, checked: checked, fresh: fresh && len(out) == first})
				continue
			}
			if strings.TrimSpace(lines[j]) == "" && (len(out) == first || !checked) {
				continue
			}
			if len(out) == first {
				t.Fatalf("README line %d: output before any command", j+1)
			}
			if !checked {
				t.Fatalf("README line %d: a setup block shows commands only", j+1)
			}
			if checked {
				out[len(out)-1].want = append(out[len(out)-1].want, lines[j])
			}
		}
	}
	return out
}

// Clock times and ages differ from run to run; everything else must match.
var readmeVolatile = []struct {
	re   *regexp.Regexp
	with string
}{
	{regexp.MustCompile(`\b\d{2}:\d{2}\b`), "HH:MM"},
	{regexp.MustCompile(`\b\d+[smhd] ago\b`), "N ago"},
}

func readmeNormalize(lines []string) string {
	// A blank line ends an output; it separates a command from the next
	// terminal's comment in the page.
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	text := strings.Join(lines, "\n")
	for _, v := range readmeVolatile {
		text = v.re.ReplaceAllString(text, v.with)
	}
	return text
}

// readmeReleaseBinary builds crosshall with a release version: a development
// build refuses the default state root, and a newcomer runs a release. The
// default root is derived from HOME, which every README command sets to a
// temporary home.
func readmeReleaseBinary(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "crosshall")
	build := exec.Command("go", "build", "-ldflags", "-X main.version=v0.0.0-readme-test", "-o", path, "../../cmd/crosshall")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	return path
}

// readmeInstall is one fresh installation: a home, crosshall on PATH, no
// CROSSHALL_STATE_DIR, the owner's plain terminal, and one member shell per
// terminal, opened by running the shown crosshall run -- sh.
type readmeInstall struct {
	w      *world
	bin    string
	shells map[string]*readmeShell
	extra  []string // environment added by a negative test
}

// readmeShell is a member terminal: the shown launcher command, run by a
// shell, with the commands typed after it written to its input.
type readmeShell struct {
	cmd    *exec.Cmd
	input  io.WriteCloser
	output *bufio.Reader
	marker string
}

func newReadmeInstall(t *testing.T, release string) *readmeInstall {
	t.Helper()
	w := newWorld(t)
	// The default state root under the fresh home, as for a newcomer.
	w.state = filepath.Join(w.home, ".local", "state", "crosshall")
	// The installer's default destination, which the removal example deletes.
	bin := filepath.Join(w.home, ".local", "bin")
	for _, dir := range []string{bin, filepath.Join(w.home, "tmp")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if tmp, err := filepath.EvalSymlinks(os.TempDir()); err != nil || !strings.HasPrefix(w.home, tmp) {
		t.Fatalf("the README runs a release build, which uses the default state root; %s is not a temporary home", w.home)
	}
	if err := os.Symlink(release, filepath.Join(bin, "crosshall")); err != nil {
		t.Fatal(err)
	}
	in := &readmeInstall{w: w, bin: bin, shells: map[string]*readmeShell{}}
	t.Cleanup(in.close)
	return in
}

func (in *readmeInstall) env() []string {
	return append([]string{"HOME=" + in.w.home, "PATH=" + in.bin + ":/usr/bin:/bin", "TMPDIR=" + filepath.Join(in.w.home, "tmp")}, in.extra...)
}

func (in *readmeInstall) close() {
	for _, sh := range in.shells {
		sh.input.Close()
		done := make(chan struct{})
		go func() { sh.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			sh.cmd.Process.Kill()
			<-done
		}
	}
}

// run executes one shown command and returns its output and exit status.
func (in *readmeInstall) run(t *testing.T, c readmeCommand) ([]string, int) {
	t.Helper()
	if c.terminal == "owner" {
		cmd := exec.Command("/bin/sh", "-c", c.command)
		cmd.Dir, cmd.Env = in.w.home, in.env()
		out, err := cmd.CombinedOutput()
		code := 0
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else if err != nil {
			t.Fatalf("README line %d: %v", c.line+1, err)
		}
		return readmeLines(string(out)), code
	}
	if c.command == "crosshall run -- sh" {
		if in.shells[c.terminal] != nil {
			t.Fatalf("README line %d: terminal %q already has a member shell", c.line+1, c.terminal)
		}
		in.shells[c.terminal] = in.launch(t, c)
		// Whatever the launcher printed before the first prompt is its output.
		return in.shells[c.terminal].await(t, c, "true")
	}
	sh := in.shells[c.terminal]
	if sh == nil {
		t.Fatalf("README line %d: terminal %q runs a command before crosshall run -- sh", c.line+1, c.terminal)
	}
	return sh.await(t, c, c.command)
}

func (in *readmeInstall) launch(t *testing.T, c readmeCommand) *readmeShell {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", c.command)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = in.w.home, in.env(), write, write
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("README line %d: %v", c.line+1, err)
	}
	write.Close()
	t.Cleanup(func() { read.Close() })
	return &readmeShell{cmd: cmd, input: input, output: bufio.NewReader(read), marker: "readme-done-" + randomHex(8)}
}

// await types a command into the member shell and reads its output up to a
// marker line that carries the command's exit status. A shell that exits
// first (for example, a launcher that failed) fails the test.
func (sh *readmeShell) await(t *testing.T, c readmeCommand, command string) ([]string, int) {
	t.Helper()
	if _, err := fmt.Fprintf(sh.input, "%s\nprintf '\\n%s %%d\\n' \"$?\"\n", command, sh.marker); err != nil {
		t.Fatalf("README line %d: the member shell is gone: %v", c.line+1, err)
	}
	type result struct {
		text string
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		var text strings.Builder
		for {
			line, err := sh.output.ReadString('\n')
			if rest, ok := strings.CutPrefix(line, sh.marker+" "); ok {
				code, convErr := strconv.Atoi(strings.TrimSpace(rest))
				done <- result{text.String(), code, convErr}
				return
			}
			text.WriteString(line)
			if err != nil {
				done <- result{text.String(), -1, fmt.Errorf("the member shell exited: %w", err)}
				return
			}
		}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("README line %d, %s: $ %s: %v\n%s", c.line+1, c.terminal, c.command, r.err, r.text)
		}
		// The marker starts on a new line; drop the newline it added.
		return readmeLines(strings.TrimSuffix(r.text, "\n")), r.code
	case <-time.After(time.Minute):
		t.Fatalf("README line %d, %s: $ %s did not finish", c.line+1, c.terminal, c.command)
	}
	return nil, -1
}

func readmeLines(out string) []string {
	out = strings.TrimRight(out, "\n")
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// The README's runnable blocks are executed in order, as a newcomer would
// type them. Set CROSSHALL_README_UPDATE=1 to write the observed output back
// into README.md.
func TestReadmeCommandsRunAsShown(t *testing.T) {
	const path = "../../README.md"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	commands := readmeCommands(t, lines)
	if len(commands) == 0 {
		t.Fatal("README has no runnable blocks")
	}
	update := os.Getenv("CROSSHALL_README_UPDATE") == "1"
	replace := map[int][]string{}
	release := readmeReleaseBinary(t)
	var install *readmeInstall
	for _, c := range commands {
		if install == nil || c.fresh {
			install = newReadmeInstall(t, release)
		}
		got, code := install.run(t, c)
		if code != c.exit {
			t.Fatalf("README line %d, %s: $ %s exited %d, want %d:\n%s", c.line+1, c.terminal, c.command, code, c.exit, strings.Join(got, "\n"))
		}
		if !c.checked {
			continue
		}
		if update {
			// Keep the page's lines when only times or ages differ.
			if readmeNormalize(got) == readmeNormalize(c.want) {
				got = c.want
			}
			replace[c.line] = got
			continue
		}
		if readmeNormalize(got) != readmeNormalize(c.want) {
			t.Errorf("README line %d, %s: $ %s\ngot:\n%s\nwant:\n%s", c.line+1, c.terminal, c.command,
				strings.Join(got, "\n"), strings.Join(c.want, "\n"))
		}
	}
	// The page ends by removing crosshall: nothing of the last installation
	// may be left, whatever the removal commands printed.
	for _, gone := range []string{install.w.state, install.w.rendezvous(), filepath.Join(install.bin, "crosshall")} {
		if _, err := os.Lstat(gone); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("after the README's removal steps, %s is still there (%v)", gone, err)
		}
	}
	if update {
		readmeRewrite(t, path, lines, replace)
	}
}

// readmeRewrite replaces each checked command's output with what it printed,
// keeping a blank line that separates it from a following terminal comment.
func readmeRewrite(t *testing.T, path string, lines []string, replace map[int][]string) {
	t.Helper()
	var out []string
	for i := 0; i < len(lines); i++ {
		out = append(out, lines[i])
		got, ok := replace[i]
		if !ok {
			continue
		}
		// A blank line before the next terminal is added once, below.
		for len(got) > 0 && strings.TrimSpace(got[len(got)-1]) == "" {
			got = got[:len(got)-1]
		}
		out = append(out, got...)
		j := i + 1
		for j < len(lines) && !readmeFenceClose.MatchString(lines[j]) && !strings.HasPrefix(lines[j], "$ ") && !readmeTerminal.MatchString(lines[j]) {
			j++
		}
		if j < len(lines) && readmeTerminal.MatchString(lines[j]) {
			out = append(out, "")
		}
		i = j - 1
	}
	if err := os.WriteFile(path, []byte(strings.Join(out, "\n")), 0644); err != nil {
		t.Fatal(err)
	}
	t.Log("README.md updated from the run")
}

// readmeRemoval is the README's removal command: the owner's command that
// deletes the state directory.
func readmeRemoval(t *testing.T) readmeCommand {
	t.Helper()
	raw, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range readmeCommands(t, strings.Split(string(raw), "\n")) {
		if c.terminal == "owner" && strings.Contains(c.command, "rm -rf") {
			return c
		}
	}
	t.Fatal("README has no removal command")
	return readmeCommand{}
}

// The removal command deletes nothing when the dispatcher does not stop, or
// when the state directory is not the default one.
func TestReadmeRemovalDeletesNothingUnlessSafe(t *testing.T) {
	removal := readmeRemoval(t)
	release := readmeReleaseBinary(t)
	for _, tc := range []struct {
		name  string
		setup func(in *readmeInstall)
	}{
		{"stop fails", func(in *readmeInstall) {
			// A crosshall whose owner stop fails, as a timeout or refusal would.
			shim := filepath.Join(in.bin, "crosshall")
			if err := os.Remove(shim); err != nil {
				t.Fatal(err)
			}
			script := "#!/bin/sh\nif [ \"$1 $2\" = \"owner stop\" ]; then echo 'crosshall: the dispatcher did not stop' >&2; exit 1; fi\nexec " + release + " \"$@\"\n"
			if err := os.WriteFile(shim, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
		}},
		{"custom state directory", func(in *readmeInstall) {
			custom := filepath.Join(in.w.home, "custom-state")
			if err := os.Mkdir(custom, 0700); err != nil {
				t.Fatal(err)
			}
			in.extra = []string{"CROSSHALL_STATE_DIR=" + custom}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := newReadmeInstall(t, release)
			start := readmeCommand{terminal: "owner", command: "crosshall owner start"}
			if out, code := in.run(t, start); code != 0 {
				t.Fatalf("owner start: %d %v", code, out)
			}
			// The dispatcher outlives a refused removal. Stop it by the PID taken
			// now: a broken guard may delete the lock file that cleanup reads.
			pids := in.w.dispatcherPIDs()
			if len(pids) == 0 {
				t.Fatal("no dispatcher after owner start")
			}
			t.Cleanup(func() {
				for _, pid := range pids {
					if proc, err := os.FindProcess(pid); err == nil {
						_ = proc.Kill()
					}
				}
			})
			tc.setup(in)
			kept := []string{in.w.state, in.w.rendezvous(), filepath.Join(in.bin, "crosshall")}
			if in.extra != nil {
				kept = append(kept, filepath.Join(in.w.home, "custom-state"))
			}
			if out, code := in.run(t, removal); code == 0 {
				t.Fatalf("the removal command succeeded:\n%s", strings.Join(out, "\n"))
			}
			for _, path := range kept {
				if _, err := os.Lstat(path); err != nil {
					t.Errorf("%s was deleted: %v", path, err)
				}
			}
		})
	}
}
