package main

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Invite struct {
	Label   string    `json:"label"`
	Created time.Time `json:"created"`
	Revoked bool      `json:"revoked,omitempty"`
}

// Invites maps normalized (upper-case) code -> invite. File format: that map as JSON.
type Invites map[string]Invite

func normCode(c string) string { return strings.ToUpper(strings.TrimSpace(c)) }

func readInvites(path string) (Invites, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Invites{}, nil
	}
	if err != nil {
		return nil, err
	}
	inv := Invites{}
	return inv, json.Unmarshal(b, &inv)
}

// writeInvites replaces the file atomically (tmp + rename) so readers never see a partial file.
func writeInvites(path string, inv Invites) error {
	b, _ := json.MarshalIndent(inv, "", "  ")
	tmp, err := os.CreateTemp(filepath.Dir(path), ".invites-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func newCode() string {
	const alpha = "ABCDEFGHJKMNPQRSTUVWXYZ23456789" // no look-alikes
	b := make([]byte, 8)
	rand.Read(b)
	for i := range b {
		b[i] = alpha[int(b[i])%len(alpha)]
	}
	return string(b[:4]) + "-" + string(b[4:])
}

// inviteCmd implements `invite add|list|revoke`. Returns output text.
func inviteCmd(path string, args []string) (string, error) {
	inv, err := readInvites(path)
	if err != nil {
		return "", err
	}
	if len(args) == 0 {
		return "", fmt.Errorf("usage: invite add <label> | list | revoke <code>")
	}
	switch args[0] {
	case "add":
		if len(args) < 2 {
			return "", fmt.Errorf("usage: invite add <label>")
		}
		code := newCode()
		inv[code] = Invite{Label: strings.Join(args[1:], " "), Created: time.Now().UTC().Truncate(time.Second)}
		return code + "\n", writeInvites(path, inv)
	case "list":
		codes := make([]string, 0, len(inv))
		for c := range inv {
			codes = append(codes, c)
		}
		sort.Strings(codes)
		var sb strings.Builder
		for _, c := range codes {
			st := "active"
			if inv[c].Revoked {
				st = "revoked"
			}
			fmt.Fprintf(&sb, "%s\t%s\t%s\t%s\n", c, st, inv[c].Created.Format(time.RFC3339), inv[c].Label)
		}
		return sb.String(), nil
	case "revoke":
		if len(args) != 2 {
			return "", fmt.Errorf("usage: invite revoke <code>")
		}
		c := normCode(args[1])
		i, ok := inv[c]
		if !ok {
			return "", fmt.Errorf("unknown code %s", c)
		}
		i.Revoked = true
		inv[c] = i
		return "revoked " + c + "\n", writeInvites(path, inv)
	}
	return "", fmt.Errorf("unknown invite command %q", args[0])
}
