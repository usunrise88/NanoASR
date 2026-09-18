//go:build !windows

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// runAsService is the Windows service entry point, and there is nothing to
// enter here: systemd, launchd and the rest run an ordinary foreground process
// and signal it, which is what serve already is.
func runAsService() (bool, error) { return false, nil }

// serviceCommand explains where the equivalent lives rather than pretending
// the verb does not exist, because the command that sent an operator here —
// `nanoasr --install`, copied from the Windows instructions — is a reasonable
// thing to try on a Linux box.
func serviceCommand(args []string) error {
	verb, _ := takePositional(args, "status")
	switch verb {
	case "install", "uninstall", "start", "stop", "restart", "status":
	default:
		return fmt.Errorf("service %s: unknown subcommand; use %s", verb, listVerbs())
	}

	unit := "/etc/systemd/system/nanoasr.service"
	if runtime.GOOS != "linux" {
		return fmt.Errorf("`nanoasr service` registers a Windows service and this is %s; "+
			"run `nanoasr serve` under whatever supervises processes here", runtime.GOOS)
	}
	// The unit travels in the Linux archive, so name it where it actually is
	// when that can be established, and relative to the unpacked directory
	// otherwise — which is where someone reading this is most likely standing.
	shipped := "nanoasr.service"
	if exe, err := os.Executable(); err == nil {
		if beside := filepath.Join(filepath.Dir(exe), shipped); fileExists(beside) {
			shipped = beside
		}
	}
	return errors.New("`nanoasr service` registers a Windows service; on Linux the equivalent is systemd.\n" +
		"  scripts/install.sh does the whole installation, or by hand:\n" +
		"    sudo cp " + shipped + " " + unit + "\n" +
		"    sudo systemctl daemon-reload && sudo systemctl enable --now nanoasr\n" +
		"  then: systemctl status nanoasr, journalctl -u nanoasr -f")
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}
