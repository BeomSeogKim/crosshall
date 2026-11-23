package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/telemetryconfig"
)

const telemetryQuestion = `Usage statistics help Adrian Studio decide what to fix and build in crosshall. A daily report can include rounded counts of messages, notices and hook timings, plus the crosshall release, platform, agent release families and a random token for that day. Reports exclude message text, channel names, aliases, paths, host names and user names.
The maintainer's server keeps a report about ten days, until its encrypted copy in Google Drive is confirmed, and Drive keeps it 180 days. Sending starts with the first full UTC day after you opt in. Declining leaves every crosshall feature available. crosshall owner telemetry show shows the report waiting to be sent, exactly as it will be sent; crosshall owner telemetry off turns sending off.
Send usage statistics? [Y/n]`

func applyTelemetryEnvironment(env Env, out io.Writer) error {
	if env.TelemetryStateRoot == "" || !telemetryconfig.EnvironmentOff() {
		return nil
	}
	_, changed, err := telemetryconfig.SetMode(env.TelemetryStateRoot, telemetryconfig.Off, "environment", telemetryconfig.On)
	if changed {
		fmt.Fprintln(out, "Usage statistics turned off for this state directory by an environment opt-out.")
	}
	return err
}

func offerTelemetry(env Env, opts options, input *bufio.Reader, out io.Writer, source string) (bool, error) {
	_, ci := os.LookupEnv("CI")
	if ci || opts.json || env.TelemetryEndpoint == "" || env.TelemetryStateRoot == "" ||
		env.SetupTerminal == nil || !env.SetupTerminal() || telemetryconfig.EnvironmentOff() || managedTelemetryOff(env) {
		return false, nil
	}
	setting, err := telemetryconfig.ReadMode(env.TelemetryStateRoot)
	if err != nil || setting.Mode != telemetryconfig.Unset {
		return false, err
	}
	choice, err := askTelemetry(input, out)
	if err != nil || choice == telemetryconfig.Unset {
		return true, err
	}
	_, _, err = telemetryconfig.SetMode(env.TelemetryStateRoot, choice, source, telemetryconfig.Unset)
	return true, err
}

func managedTelemetryOff(env Env) bool {
	path := telemetryconfig.ManagedOffPath
	if env.ResolveTelemetryPolicyPath != nil {
		path = env.ResolveTelemetryPolicyPath(path)
	}
	return telemetryconfig.ManagedOffAt(path)
}

func askTelemetry(input *bufio.Reader, out io.Writer) (string, error) {
	for range 2 {
		if _, err := fmt.Fprintln(out, telemetryQuestion); err != nil {
			return telemetryconfig.Unset, err
		}
		line, err := input.ReadString('\n')
		if errors.Is(err, io.EOF) {
			return telemetryconfig.Unset, nil
		}
		if err != nil {
			return telemetryconfig.Unset, err
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "", "y", "yes":
			return telemetryconfig.On, nil
		case "n", "no":
			return telemetryconfig.Off, nil
		}
	}
	return telemetryconfig.Unset, nil
}

func runOwnerTelemetry(env Env, opts options, args []string, stdout, stderr io.Writer) core.ExitCode {
	o := outputs{json: opts.json, stdout: stdout, stderr: stderr}
	if env.TelemetryStateRoot == "" {
		return o.unavailableFor(env, "owner telemetry")
	}
	action := ""
	if len(args) > 0 {
		action = args[0]
	}
	if action == "sent" {
		return runTelemetrySent(env, opts, args[1:], stdout, stderr)
	}
	if len(args) > 1 {
		return o.usage("usage: crosshall owner telemetry [on|off|show|sent]")
	}

	environmentOff, managedOff := telemetryconfig.EnvironmentOff(), managedTelemetryOff(env)
	var setting telemetryconfig.Setting
	var err error
	switch action {
	case "":
		setting, err = telemetryconfig.ReadMode(env.TelemetryStateRoot)
	case telemetryconfig.On, telemetryconfig.Off:
		if action == telemetryconfig.On && environmentOff {
			return o.fail(core.ExitRefused, "", "an environment opt-out disables usage statistics", "clear CROSSHALL_TELEMETRY and DO_NOT_TRACK before turning usage statistics on")
		}
		if action == telemetryconfig.On && managedOff {
			return o.fail(core.ExitRefused, "", "administrator policy disables usage statistics", "ask your administrator about /etc/crosshall/telemetry-off")
		}
		setting, _, err = telemetryconfig.SetMode(env.TelemetryStateRoot, action, "owner-command", "")
	case "show":
		return runTelemetryShow(env, opts, stdout, stderr)
	default:
		return o.usage("usage: crosshall owner telemetry [on|off|show|sent]")
	}
	if err != nil {
		return o.fail(core.ExitError, CodeLocalError, err.Error(), "")
	}
	return renderTelemetryStatus(o, stdout, env, setting, environmentOff, managedOff, action == telemetryconfig.Off)
}

func renderTelemetryStatus(o outputs, out io.Writer, env Env, setting telemetryconfig.Setting, environmentOff, managedOff, turnedOff bool) core.ExitCode {
	days, _, err := telemetryconfig.ReadSent(env.TelemetryStateRoot, "")
	if err != nil {
		return o.fail(core.ExitError, CodeLocalError, err.Error(), "")
	}
	st, pending, err := telemetryconfig.ReadPending(env.TelemetryStateRoot, time.Now())
	if err != nil {
		return o.fail(core.ExitError, CodeLocalError, err.Error(), "")
	}
	fields := map[string]any{
		"v": 1, "ok": true, "kind": "owner telemetry", "mode": setting.Mode, "endpoint": env.TelemetryEndpoint,
		"environment_opt_out": environmentOff, "managed_opt_out": managedOff,
	}
	if !o.json {
		fmt.Fprintf(out, "Usage statistics: %s\n", setting.Mode)
	}
	if setting.Source != "" {
		fields["decision_source"] = setting.Source
		fields["decided_at"] = setting.ChangedAt
	}
	curlMissing := false
	if setting.Mode == telemetryconfig.On && env.TelemetryCurl != "" {
		info, err := os.Stat(env.TelemetryCurl)
		curlMissing = err != nil || !info.Mode().IsRegular()
		fields["curl_missing"] = curlMissing
	}
	if setting.Mode == telemetryconfig.On && !environmentOff && !managedOff {
		day := telemetryconfig.NextFullDay(setting.ChangedAt).Format(time.DateOnly)
		fields["eligible_from_day"] = day
		if !o.json {
			fmt.Fprintf(out, "First eligible UTC day from this setting: %s\n", day)
		}
	}
	if len(days) > 0 {
		day := days[len(days)-1]
		fields["last_sent_day"] = day
		if !o.json {
			fmt.Fprintf(out, "Last sent UTC day: %s\n", day)
		}
	}
	if st.LastStatus != "" {
		fields["last_answer"] = st.LastStatus
		if !o.json {
			fmt.Fprintf(out, "Last answer from the receiver: %s\n", st.LastStatus)
		}
	}
	if pending != nil {
		fields["pending_day"], fields["next_attempt"] = pending.Day, pending.NextAttempt
		if !o.json {
			fmt.Fprintf(out, "Waiting to be sent: the report for %s, not before %s (crosshall owner telemetry show shows it)\n",
				pending.Day, pending.NextAttempt.Format(time.RFC3339))
		}
	}
	if o.json {
		writeJSON(out, fields)
		return core.ExitOK
	}
	if environmentOff {
		fmt.Fprintln(out, "Sending is disabled by an environment opt-out.")
	}
	if managedOff {
		fmt.Fprintln(out, "Sending is disabled by /etc/crosshall/telemetry-off.")
	}
	if curlMissing {
		fmt.Fprintf(out, "Nothing is sent: reports are sent with %s, which is not installed.\n", env.TelemetryCurl)
	}
	if env.TelemetryEndpoint == "" {
		fmt.Fprintln(out, "Receiver: not configured")
	} else {
		fmt.Fprintf(out, "Receiver: %s\n", env.TelemetryEndpoint)
	}
	if turnedOff {
		fmt.Fprintln(out, "Reports already accepted keep their normal retention.")
	}
	return core.ExitOK
}

// runTelemetryShow prints the batch the dispatcher sends next, byte for
// byte as it will be sent.
func runTelemetryShow(env Env, opts options, stdout, stderr io.Writer) core.ExitCode {
	o := outputs{json: opts.json, stdout: stdout, stderr: stderr}
	_, pending, err := telemetryconfig.ReadPending(env.TelemetryStateRoot, time.Now())
	if err != nil {
		return o.fail(core.ExitError, CodeLocalError, err.Error(), "")
	}
	if pending == nil {
		return o.fail(core.ExitUnavailable, core.CodeUnsent, "no usage report is waiting to be sent",
			"crosshall owner telemetry sent lists the reports already sent")
	}
	if opts.json {
		writeJSON(stdout, map[string]any{"v": 1, "ok": true, "kind": "owner telemetry show", "day": pending.Day,
			"next_attempt": pending.NextAttempt, "report": json.RawMessage(pending.Body)})
		return core.ExitOK
	}
	if _, err := stdout.Write(pending.Body); err != nil {
		return o.fail(core.ExitError, CodeLocalError, err.Error(), "")
	}
	return core.ExitOK
}

func runTelemetrySent(env Env, opts options, args []string, stdout, stderr io.Writer) core.ExitCode {
	o := outputs{json: opts.json, stdout: stdout, stderr: stderr}
	day := ""
	if len(args) > 0 {
		if len(args) != 2 || args[0] != "--day" {
			return o.usage("usage: crosshall owner telemetry sent [--day YYYY-MM-DD]")
		}
		day = args[1]
		if _, err := time.Parse(time.DateOnly, day); err != nil {
			return o.usage("usage: crosshall owner telemetry sent [--day YYYY-MM-DD]")
		}
	}
	days, report, err := telemetryconfig.ReadSent(env.TelemetryStateRoot, day)
	if errors.Is(err, os.ErrNotExist) {
		return o.fail(core.ExitUnavailable, core.CodeUnsent, "no sent usage report for "+day, "")
	}
	if err != nil {
		return o.fail(core.ExitError, CodeLocalError, err.Error(), "")
	}
	if opts.json {
		fields := map[string]any{"v": 1, "ok": true, "kind": "owner telemetry sent"}
		if day == "" {
			fields["days"] = days
		} else {
			fields["day"], fields["report"] = day, json.RawMessage(report)
		}
		writeJSON(stdout, fields)
	} else if day != "" {
		if _, err := stdout.Write(report); err != nil {
			return o.fail(core.ExitError, CodeLocalError, err.Error(), "")
		}
	} else if len(days) == 0 {
		fmt.Fprintln(stdout, "No sent usage reports.")
	} else {
		fmt.Fprintln(stdout, strings.Join(days, "\n"))
	}
	return core.ExitOK
}
