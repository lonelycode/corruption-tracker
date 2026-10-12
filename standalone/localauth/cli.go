// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package localauth

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// CLIUsage documents the `user` command group.
const CLIUsage = `usage: corruption-tracker user <command>

commands:
  list                                  list accounts, roles and status
  reset-password <username> [--password-stdin]
                                        set a new password and sign the user out
                                        everywhere; without --password-stdin a
                                        random password is generated and printed once
`

// RunCLI runs the `user` command group against svc. args excludes "user".
// It is the recovery path when nobody can sign in (email may not be
// configured, so there is no self-service reset).
func RunCLI(ctx context.Context, svc *Service, args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New(CLIUsage)
	}
	switch args[0] {
	case "list":
		return cliList(ctx, svc, stdout)
	case "reset-password":
		return cliResetPassword(ctx, svc, args[1:], stdin, stdout)
	default:
		return fmt.Errorf("unknown user command %q\n\n%s", args[0], CLIUsage)
	}
}

func cliList(ctx context.Context, svc *Service, stdout io.Writer) error {
	users, err := svc.Users(ctx)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "USERNAME\tROLE\tEXPORT\tSTATUS\tACTOR")
	for _, u := range users {
		status := "active"
		if u.Disabled {
			status = "disabled"
		}
		export := "implied"
		if u.Role == RoleViewer {
			export = fmt.Sprint(u.CanExport)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", u.Username, u.Role, export, status, u.ExternalRef)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if warn, err := svc.ReviewerWarning(ctx); err != nil {
		return err
	} else if warn != "" {
		fmt.Fprintln(stdout, "\nwarning:", warn)
	}
	return nil
}

func cliResetPassword(ctx context.Context, svc *Service, args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("reset-password", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fromStdin := fs.Bool("password-stdin", false, "read the new password from stdin")
	// Accept the flag before or after the username.
	var positional []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return fmt.Errorf("%v\n\n%s", err, CLIUsage)
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(positional) != 1 {
		return errors.New(CLIUsage)
	}
	u, err := svc.UserByName(ctx, positional[0])
	if errors.Is(err, ErrNotFound) {
		return fmt.Errorf("no user named %q", positional[0])
	}
	if err != nil {
		return err
	}
	pw := ""
	generated := false
	if *fromStdin {
		line, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		pw = strings.TrimRight(line, "\r\n")
	} else {
		if pw, err = GeneratePassword(); err != nil {
			return err
		}
		generated = true
	}
	if err := svc.ResetPassword(ctx, User{}, u.ID, pw); err != nil {
		return err
	}
	if generated {
		fmt.Fprintf(stdout, "New password for %s: %s\nIt is shown once; the user's sessions have been ended.\n", u.Username, pw)
	} else {
		fmt.Fprintf(stdout, "Password for %s updated; the user's sessions have been ended.\n", u.Username)
	}
	if u.Disabled {
		fmt.Fprintf(stdout, "Note: %s is disabled; an admin must re-enable them before they can sign in.\n", u.Username)
	}
	return nil
}
