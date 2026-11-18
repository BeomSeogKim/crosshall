package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

const adapterPolicyUsage = "usage: crosshall owner adapter set <claude|codex> [--enabled on|off] [--reminder on|off]"

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
	_, err := fmt.Fprintf(w, "%s adapter enabled=%s reminder=%s epoch=%d\n", policy.Adapter, enabled, reminder, policy.Epoch)
	return err
}
