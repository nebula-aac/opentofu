// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mitchellh/cli"
	"github.com/posener/complete"

	"github.com/opentofu/opentofu/internal/command/arguments"
	"github.com/opentofu/opentofu/internal/command/clistate"
	"github.com/opentofu/opentofu/internal/command/views"
	"github.com/opentofu/opentofu/internal/states"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

type WorkspaceDeleteCommand struct {
	Meta
	LegacyName bool
}

func (c *WorkspaceDeleteCommand) Run(args []string) int {
	args = c.Meta.process(args)
	envCommandShowWarning(c.Ui, c.LegacyName)

	var force bool
	var stateLock bool
	var stateLockTimeout time.Duration
	cmdFlags := c.Meta.defaultFlagSet("workspace delete")
	cmdFlags.BoolVar(&force, "force", false, "force removal of a non-empty workspace")
	cmdFlags.BoolVar(&stateLock, "lock", true, "lock state")
	cmdFlags.DurationVar(&stateLockTimeout, "lock-timeout", 0, "lock timeout")
	cmdFlags.Usage = func() { c.Ui.Error(c.Help()) }
	if err := cmdFlags.Parse(args); err != nil {
		c.Ui.Error(fmt.Sprintf("Error parsing command-line flags: %s\n", err.Error()))
		return 1
	}

	args = cmdFlags.Args()
	if len(args) != 1 {
		c.Ui.Error("Expected a single argument: NAME.\n")
		return cli.RunResultHelp
	}

	configPath, err := modulePath(args[1:])
	if err != nil {
		c.Ui.Error(err.Error())
		return 1
	}

	var diags tfdiags.Diagnostics

	backendConfig, backendDiags := c.loadBackendConfig(configPath)
	diags = diags.Append(backendDiags)
	if diags.HasErrors() {
		c.showDiagnostics(diags)
		return 1
	}

	// Load the backend
	b, backendDiags := c.Backend(&BackendOpts{
		Config: backendConfig,
	})
	diags = diags.Append(backendDiags)
	if backendDiags.HasErrors() {
		c.showDiagnostics(diags)
		return 1
	}

	// This command will not write state
	c.ignoreRemoteVersionConflict(b)

	ctx := context.TODO()

	workspaces, err := b.Workspaces(ctx)
	if err != nil {
		c.Ui.Error(err.Error())
		return 1
	}

	workspace := args[0]
	exists := false
	for _, ws := range workspaces {
		if workspace == ws {
			exists = true
			break
		}
	}

	if !exists {
		c.Ui.Error(fmt.Sprintf(strings.TrimSpace(envDoesNotExist), workspace))
		return 1
	}

	currentWorkspace, err := c.Workspace()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error selecting workspace: %s", err))
		return 1
	}
	if workspace == currentWorkspace {
		c.Ui.Error(fmt.Sprintf(strings.TrimSpace(envDelCurrent), workspace))
		return 1
	}

	// we need the actual state to see if it's empty
	stateMgr, err := b.StateMgr(ctx, workspace)
	if err != nil {
		c.Ui.Error(err.Error())
		return 1
	}

	var stateLocker clistate.Locker
	if stateLock {
		stateLocker = clistate.NewLocker(c.stateLockTimeout, views.NewStateLocker(arguments.ViewHuman, c.View))
		if diags := stateLocker.Lock(stateMgr, "state-replace-provider"); diags.HasErrors() {
			c.showDiagnostics(diags)
			return 1
		}
	} else {
		stateLocker = clistate.NewNoopLocker()
	}

	if err := stateMgr.RefreshState(ctx); err != nil {
		// We need to release the lock before exit
		stateLocker.Unlock()
		c.Ui.Error(err.Error())
		return 1
	}

	hasResources := stateMgr.State().HasManagedResourceInstanceObjects()

	if hasResources && !force {
		// We'll collect a list of what's being managed here as extra context
		// for the message.
		var buf strings.Builder
		for _, obj := range stateMgr.State().AllResourceInstanceObjectAddrs() {
			if obj.DeposedKey == states.NotDeposed {
				fmt.Fprintf(&buf, "\n  - %s", obj.Instance.String())
			} else {
				fmt.Fprintf(&buf, "\n  - %s (deposed object %s)", obj.Instance.String(), obj.DeposedKey)
			}
		}

		// We need to release the lock before exit
		stateLocker.Unlock()

		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Workspace is not empty",
			fmt.Sprintf(
				"Workspace %q is currently tracking the following resource instances:%s\n\nDeleting this workspace would cause OpenTofu to lose track of any associated remote objects, which would then require you to delete them manually outside of OpenTofu. You should destroy these objects with OpenTofu before deleting the workspace.\n\nIf you want to delete this workspace anyway, and have OpenTofu forget about these managed objects, use the -force option to disable this safety check.",
				workspace, buf.String(),
			),
		))
		c.showDiagnostics(diags)
		return 1
	}

	// We need to release the lock just before deleting the state, in case
	// the backend can't remove the resource while holding the lock. This
	// is currently true for Windows local files.
	//
	// TODO: While there is little safety in locking while deleting the
	// state, it might be nice to be able to coordinate processes around
	// state deletion, i.e. in a CI environment. Adding Delete() as a
	// required method of States would allow the removal of the resource to
	// be delegated from the Backend to the State itself.
	stateLocker.Unlock()

	err = b.DeleteWorkspace(ctx, workspace, force)
	if err != nil {
		c.Ui.Error(err.Error())
		return 1
	}

	c.Ui.Output(
		c.Colorize().Color(
			fmt.Sprintf(envDeleted, workspace),
		),
	)

	if hasResources {
		c.Ui.Output(
			c.Colorize().Color(
				fmt.Sprintf(envWarnNotEmpty, workspace),
			),
		)
	}

	return 0
}

func (c *WorkspaceDeleteCommand) AutocompleteArgs() complete.Predictor {
	return completePredictSequence{
		c.completePredictWorkspaceName(),
		complete.PredictDirs(""),
	}
}

func (c *WorkspaceDeleteCommand) AutocompleteFlags() complete.Flags {
	return complete.Flags{
		"-force": complete.PredictNothing,
	}
}

func (c *WorkspaceDeleteCommand) Help() string {
	helpText := `
Usage: tofu [global options] workspace delete [OPTIONS] NAME

  Delete a OpenTofu workspace


Options:

  -force             Remove a workspace even if it is managing resources.
                     OpenTofu can no longer track or manage the workspace's
                     infrastructure.

  -lock=false        Don't hold a state lock during the operation. This is
                     dangerous if others might concurrently run commands
                     against the same workspace.

  -lock-timeout=0s   Duration to retry a state lock.

`
	return strings.TrimSpace(helpText)
}

func (c *WorkspaceDeleteCommand) Synopsis() string {
	return "Delete a workspace"
}
