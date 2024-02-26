// Package main provides a script that applies and asserts Pro-only policies to
// the provisioned Ubuntu client.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/ubuntu/adsys/e2e/internal/command"
	"github.com/ubuntu/adsys/e2e/internal/inventory"
	"github.com/ubuntu/adsys/e2e/internal/remote"
)

var sshKey, proToken string

const expectedMountedFileContents = "content from AD controller"

func main() {
	os.Exit(run())
}

func run() int {
	cmd := command.New(action,
		command.WithValidateFunc(validate),
		command.WithRequiredState(inventory.ADProvisioned),
	)
	cmd.Usage = fmt.Sprintf(`go run ./%s [options]

Apply and assert Pro-only policies on the Ubuntu client.

These policies are configured in the e2e/assets/gpo directory, and described as
part of the ADSys QA Plan document.

https://docs.google.com/document/d/1dIdhqAfNohapcTgWVVeyG7aSDMrGJekeezmoRdd_JSU/

The Pro token must be set in the ADSYS_PRO_TOKEN environment variable.

This script will:
 - attach the client VM to Ubuntu Pro
 - reboot the client VM to re-trigger machine policy application
 - assert Pro machine GPO rules were applied
 - assert Pro users and admins GPO rules were applied
 - reboot the client VM again and confirm the machine startup scripts were executed
 - detach the client VM from Ubuntu Pro

The run is considered successful if the script exits with a zero exit code.
If an error occurs during execution, the script will attempt to detach the client from Pro.

The runner must be connected to the ADSys E2E tests VPN.`, filepath.Base(os.Args[0]))

	return cmd.Execute(context.Background())
}

func validate(_ context.Context, cmd *command.Command) (err error) {
	sshKey, err = command.ValidateAndExpandPath(cmd.Inventory.SSHKeyPath, command.DefaultSSHKeyPath)
	if err != nil {
		return err
	}

	// Used in the tests below to attach the client to Ubuntu Pro
	proToken = os.Getenv("ADSYS_PRO_TOKEN")
	if proToken == "" {
		return fmt.Errorf("ADSYS_PRO_TOKEN environment variable must be set")
	}
	return nil
}

func action(ctx context.Context, cmd *command.Command) error {
	client, err := remote.NewClient(cmd.Inventory.IP, "root", sshKey)
	if err != nil {
		return fmt.Errorf("failed to connect to VM: %w", err)
	}

	defer func() {
		client, err := remote.NewClient(cmd.Inventory.IP, "root", sshKey)
		if err != nil {
			log.Errorf("Teardown: Failed to connect to VM as root: %v", err)
		}

		if _, err := client.Run(ctx, "adsysctl policy purge --all -v"); err != nil {
			log.Errorf("Teardown: Failed to purge policies: %v", err)
		}

		// Detach client from Ubuntu Pro
		if _, err := client.Run(ctx, "yes | pro detach"); err != nil {
			log.Errorf("Teardown: Failed to detach client from Ubuntu Pro: %v", err)
		}

		// Remove some files generated by the scripts policy to ensure idempotency
		if _, err := client.Run(ctx, "systemctl stop adsys-machine-scripts"); err != nil {
			log.Errorf("Teardown: Failed to stop machine scripts service: %v", err)
		}
		if _, err := client.Run(ctx, "rm -f /etc/created-by-adsys-machine-startup-script /etc/created-by-adsys-machine-shutdown-script"); err != nil {
			log.Errorf("Teardown: Failed to remove machine startup/shutdown scripts: %v", err)
		}
		if _, err := client.Run(ctx, "rm -f /home/*/created-by-adsys-admin-logon-script /home/*/created-by-adsys-admin-logoff-script /home/*/created-by-adsys-user-logon-script"); err != nil {
			log.Errorf("Teardown: Failed to remove user scripts: %v", err)
		}
	}()

	// Attach client to Ubuntu Pro
	if _, err := client.Run(ctx, fmt.Sprintf("pro attach %s --no-auto-enable", proToken)); err != nil {
		return fmt.Errorf("failed to attach client to Ubuntu Pro: %w", err)
	}
	if err := client.RequireContains(ctx, "pro status --wait", "Subscription: Ubuntu Pro"); err != nil {
		return fmt.Errorf("failed to confirm client is attached to Ubuntu Pro: %w", err)
	}

	// Start timer-triggered service to update policies
	if _, err := client.Run(ctx, "systemctl restart adsys-gpo-refresh"); err != nil {
		return err
	}

	// Assert machine policies were applied
	/// Mounts
	if cmd.Inventory.Codename != "focal" { // mount behavior is spotty on focal so avoid asserting it
		if err := client.RequireEqual(ctx, "cat /adsys/nfs/warthogs.biz/system-mount-nfs/file.txt", expectedMountedFileContents); err != nil {
			return err
		}
		if err := client.RequireEqual(ctx, "cat /adsys/cifs/warthogs.biz/system-mount-smb/file.txt", expectedMountedFileContents); err != nil {
			return err
		}
	}

	/// Privilege escalation
	if err := client.RequireEqual(ctx, "cat /etc/sudoers.d/99-adsys-privilege-enforcement", `# This file is managed by adsys.
# Do not edit this file manually.
# Any changes will be overwritten.

"adminuser@warthogs.biz"	ALL=(ALL:ALL) ALL`); err != nil {
		return err
	}
	// Only partly assert the polkit file contents as there are differences in polkit configurations between Ubuntu versions
	if err := client.RequireContains(ctx, "cat /etc/polkit-1/localauthority.conf.d/99-adsys-privilege-enforcement.conf", "unix-user:adminuser@warthogs.biz"); err != nil {
		return err
	}

	/// AppArmor
	if cmd.Inventory.Codename != "focal" { // aa-status raises a Python exception on focal
		if err := client.RequireContains(ctx, "aa-status", "/usr/bin/foo=(complain)"); err != nil {
			return err
		}
	}

	/// Scripts
	if err := client.RequireFileExists(ctx, "/etc/created-by-adsys-machine-startup-script"); err != nil {
		return fmt.Errorf("%w: file should have been created by the adsys-gpo-refresh service", err)
	}
	// Remove startup script so we can check creation at next reboot
	if _, err := client.Run(ctx, "rm -f /etc/created-by-adsys-machine-startup-script"); err != nil {
		log.Errorf("Failed to remove machine startup scripts: %v", err)
	}

	if err := client.RequireNoFileExists(ctx, "/etc/created-by-adsys-machine-shutdown-script"); err != nil {
		return err
	}

	// Assert policies only available in newer adsys releases
	if !slices.Contains([]string{"focal", "jammy"}, cmd.Inventory.Codename) {
		/// Certificates
		// Enrollment takes a few seconds, so no better way to do this than an arbitrary sleep :)
		time.Sleep(5 * time.Second)
		if err := client.RequireContains(ctx, "getcert list -i warthogs-CA.Machine", "status: MONITORING"); err != nil {
			return err
		}

		/// Proxy
		if err := client.RequireEqual(ctx, "cat /etc/apt/apt.conf.d/99ubuntu-proxy-manager", `### This file was generated by ubuntu-proxy-manager - manual changes will be overwritten
Acquire::ftp::Proxy "http://127.0.0.1:8080";`); err != nil {
			return err
		}
		if err := client.RequireEqual(ctx, "cat /etc/environment.d/99ubuntu-proxy-manager.conf", `### This file was generated by ubuntu-proxy-manager - manual changes will be overwritten
FTP_PROXY="http://127.0.0.1:8080"
ftp_proxy="http://127.0.0.1:8080"`); err != nil {
			return err
		}
		if err := client.RequireEqual(ctx, "gsettings get org.gnome.system.proxy.ftp host", "'127.0.0.1'"); err != nil {
			return err
		}

		if err := client.RequireEqual(ctx, "gsettings get org.gnome.system.proxy.ftp port", "8080"); err != nil {
			return err
		}
	}

	// Reboot and check machine scripts
	if err := client.Reboot(); err != nil {
		return err
	}
	if err := client.RequireFileExists(ctx, "/etc/created-by-adsys-machine-shutdown-script"); err != nil {
		return err
	}
	if err := client.RequireFileExists(ctx, "/etc/created-by-adsys-machine-startup-script"); err != nil {
		return err
	}

	////// Start policies for $HOST-USR@WARTHOGS.BIZ
	client, err = remote.NewClient(cmd.Inventory.IP, fmt.Sprintf("%s-usr@warthogs.biz", cmd.Inventory.Hostname), remote.DomainUserPassword)
	if err != nil {
		return fmt.Errorf("failed to connect to VM: %w", err)
	}

	/// User mounts
	// Workaround user mounts only being applied on graphical sessions
	if _, err := client.Run(ctx, "systemctl --user start adsys-user-mounts"); err != nil {
		log.Warningf("Failed to trigger user mounts: %v", err)
	}
	if err := client.RequireContains(ctx, "gio mount -l", "user-mount-smb on warthogs.biz -> smb://warthogs.biz/user-mount-smb/"); err != nil {
		return err
	}

	/// User scripts
	if err := client.RequireFileExists(ctx, "created-by-adsys-user-logon-script"); err != nil {
		return err
	}
	////// End policies for $HOST-USR@WARTHOGS.BIZ

	////// Start policies for $HOST-ADM@WARTHOGS.BIZ
	// Assert admin GPO policies were applied
	client, err = remote.NewClient(cmd.Inventory.IP, fmt.Sprintf("%s-adm@warthogs.biz", cmd.Inventory.Hostname), remote.DomainUserPassword)
	if err != nil {
		return fmt.Errorf("failed to connect to VM: %w", err)
	}

	/// User mounts
	// Workaround user mounts only being applied on graphical sessions
	if _, err := client.Run(ctx, "systemctl --user start adsys-user-mounts"); err != nil {
		log.Warningf("Failed to trigger user mounts: %v", err)
	}
	if err := client.RequireContains(ctx, "gio mount -l", "user-mount-smb on warthogs.biz -> smb://warthogs.biz/user-mount-smb/"); err != nil {
		return err
	}
	if err := client.RequireContains(ctx, "gio mount -l", "user-mount-nfs on warthogs.biz -> nfs://warthogs.biz/user-mount-nfs"); err != nil {
		return err
	}

	/// User scripts
	if err := client.RequireFileExists(ctx, "created-by-adsys-user-logon-script"); err != nil {
		return err
	}
	if err := client.RequireFileExists(ctx, "created-by-adsys-admin-logon-script"); err != nil {
		return err
	}
	if err := client.RequireNoFileExists(ctx, "created-by-adsys-admin-logoff-script"); err != nil {
		return err
	}

	// Force stop the user scripts service to trigger logoff script execution
	if _, err := client.Run(ctx, "systemctl --user stop adsys-user-scripts"); err != nil {
		return err
	}
	if err := client.RequireFileExists(ctx, "created-by-adsys-admin-logoff-script"); err != nil {
		return err
	}
	////// End policies for $HOST-ADM@WARTHOGS.BIZ

	return nil
}
