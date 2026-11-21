package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

const adapterPolicyUsage = "usage: crosshall owner adapter set <claude|codex|agy> [--enabled on|off] [--reminder on|off] [--publish on|off|default] [--turn-end on|off|default]"

func runAdapterPolicy(ctx context.Context, env Env, opts options, args []string, stdout, stderr io.Writer) core.ExitCode {
	o := outputs{json: opts.json, stdout: stdout, stderr: stderr}
	cmd, err := parseAdapterPolicy(args)
	if err != nil {
		return o.usage(err.Error())
	}
	if env.Start != nil {
		if err := env.Start(ctx); err != nil {
			return render(opts, cmd, domain.Fail(core.ExitUnavailable, core.CodeUnsent,
				"dispatcher could not start: "+err.Error(), "crosshall owner start"), stdout, stderr)
		}
	}
	return send(ctx, env, opts, Call{Owner: true, Command: cmd, OpKey: opKey(env, opts)}, stdout, stderr)
}

func parseAdapterPolicy(args []string) (domain.AdapterSet, error) {
	var cmd domain.AdapterSet
	if len(args) < 4 || args[0] != "adapter" || args[1] != "set" || !domain.ValidAdapterName(args[2]) {
		return cmd, fmt.Errorf("%s", adapterPolicyUsage)
	}
	cmd.Adapter = args[2]
	for i := 3; i < len(args); i++ {
		flag, _, _ := strings.Cut(args[i], "=")
		if flag == "--publish" || flag == "--turn-end" {
			destination := &cmd.Publish
			if flag == "--turn-end" {
				destination = &cmd.TurnEnd
			}
			if *destination != nil {
				return cmd, fmt.Errorf("%s was provided more than once", flag)
			}
			value, last, _, err := flagValue(args, i, flag)
			if err != nil {
				return cmd, err
			}
			if value != "on" && value != "off" && value != "default" {
				return cmd, fmt.Errorf("%s takes on, off or default", flag)
			}
			*destination = &value
			i = last
			continue
		}
		var destination **bool
		switch flag {
		case "--enabled":
			destination = &cmd.Enabled
		case "--reminder":
			destination = &cmd.Reminder
		default:
			return cmd, fmt.Errorf("unknown option %q for adapter set", args[i])
		}
		if *destination != nil {
			return cmd, fmt.Errorf("%s was provided more than once", flag)
		}
		value, last, _, err := flagValue(args, i, flag)
		if err != nil {
			return cmd, err
		}
		if value != "on" && value != "off" {
			return cmd, fmt.Errorf("%s takes on or off", flag)
		}
		enabled := value == "on"
		*destination = &enabled
		i = last
	}
	return cmd, nil
}

func renderAdapterPolicy(data []byte, w io.Writer) error {
	var policy store.AdapterPolicy
	if err := json.Unmarshal(data, &policy); err != nil {
		return err
	}
	enabled, reminder := "off", "off"
	if policy.Enabled {
		enabled = "on"
	}
	if policy.Reminder {
		reminder = "on"
	}
	publish := ""
	if policy.Publish != "" {
		publish = " publish=" + policy.Publish
	}
	if policy.TurnEnd != "" {
		publish += " turn_end=" + policy.TurnEnd
	}
	_, err := fmt.Fprintf(w, "%s adapter enabled=%s reminder=%s%s epoch=%d\n", policy.Adapter, enabled, reminder, publish, policy.Epoch)
	return err
}

func parseOwnerPublish(args []string) (domain.OwnerPublish, error) {
	usage := fmt.Errorf("usage: crosshall owner publish hall on|off, member <binding-id> on|off|default, or resume <binding-id>")
	var cmd domain.OwnerPublish
	switch {
	case len(args) == 2 && args[0] == "hall" && (args[1] == "on" || args[1] == "off"):
		cmd.Hall, cmd.Switch = true, args[1]
	case len(args) == 3 && args[0] == "member" && (args[2] == "on" || args[2] == "off" || args[2] == "default"):
		cmd.Switch = args[2]
		cmd.BindingID, _ = strconv.ParseInt(args[1], 10, 64)
	case len(args) == 2 && args[0] == "resume":
		cmd.Switch = args[0]
		cmd.BindingID, _ = strconv.ParseInt(args[1], 10, 64)
	default:
		return cmd, usage
	}
	if !cmd.Hall && cmd.BindingID <= 0 {
		return cmd, usage
	}
	return cmd, nil
}
