package cli

import (
	"errors"
	"strings"

	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/ident"
)

func restoreCall(args []string) (Call, error) {
	usage := errors.New("usage: crosshall owner restore <binding-id> --new-key <vendor>:<key> (the binding ID is the number crosshall owner who --all shows; get the key with crosshall whoami --full)")
	if len(args) < 2 {
		return Call{}, usage
	}
	binding, err := parsePositive(args[0])
	value, last, found, flagErr := flagValue(args, 1, "--new-key")
	if err != nil || flagErr != nil || !found || last != len(args)-1 {
		return Call{}, usage
	}
	vendor, key, _ := strings.Cut(value, ":")
	id, err := ident.ParseRestoreKey(vendor, key)
	if err != nil {
		return Call{}, usage
	}
	return Call{Owner: true, Command: domain.Restore{BindingID: binding, Vendor: id.Vendor, Key: id.Key}}, nil
}
