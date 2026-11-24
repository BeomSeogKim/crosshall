package ident

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

const membershipFile = "membership"

func SelectedMembership(ctx context.Context, tx store.Tx, principal int64) (core.Membership, error) {
	var membership core.Membership
	err := tx.QueryRowContext(ctx, `SELECT binding_id, generation FROM member_selection WHERE principal_id = ?`, principal).
		Scan(&membership.BindingID, &membership.Generation)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return membership, err
}

func ReadMembership(leaf *os.Root) (core.Membership, error) {
	membership, err := LookupMembership(leaf)
	if membership == nil {
		return core.Membership{}, err
	}
	return *membership, err
}

func LookupMembership(leaf *os.Root) (*core.Membership, error) {
	var membership core.Membership
	data, err := ReadPrivateFile(leaf, membershipFile, 128)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &membership); err != nil || !membership.Valid() {
		return nil, errors.New("invalid member selection")
	}
	return &membership, nil
}

func RepublishMembership(leaf *os.Root, membership core.Membership) error {
	if !membership.Valid() {
		return errors.New("invalid member selection")
	}
	serialized, err := json.Marshal(membership)
	if err != nil {
		return err
	}
	return publishMetadata(leaf, membershipFile, string(serialized)+"\n")
}

func ReadRosterReceipt(leaf *os.Root, name string, membership core.Membership) (uint64, error) {
	stamped := membership.Generation > 1
	if stamped {
		name += "-move"
	}
	data, err := ReadPrivateFile(leaf, name, 96)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	value := strings.TrimSpace(string(data))
	if stamped {
		var matched bool
		value, matched = strings.CutPrefix(value, membershipStamp(membership)+" ")
		if !matched {
			return 0, nil
		}
	}
	return strconv.ParseUint(value, 10, 64)
}

func PublishRosterReceipt(leaf *os.Root, name string, membership core.Membership, seq uint64) error {
	value := strconv.FormatUint(seq, 10) + "\n"
	if membership.Generation > 1 {
		name += "-move"
		value = membershipStamp(membership) + " " + value
	}
	return publishMetadata(leaf, name, value)
}

func membershipStamp(membership core.Membership) string {
	return fmt.Sprintf("%d %d", membership.BindingID, membership.Generation)
}

func receiptMembership(memberships []core.Membership) core.Membership {
	if len(memberships) == 0 {
		return core.Membership{}
	}
	return memberships[0]
}
